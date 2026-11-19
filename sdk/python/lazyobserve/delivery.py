"""Bounded, non-blocking, at-least-once delivery queue with retry budget.

Behavior mirrors the Go SDK's delivery.go: capacity includes the batch currently
being exported, a full queue drops new items, sends retry with exponential
backoff (100ms -> 1s, 3 attempts by default), and shutdown drains the queue
within a caller-provided budget before dropping the remainder.

**fork** (``os.register_at_fork``): a forked child inherits the queue, its locks and
whatever the parent had queued, but not the worker thread. Without a hook the child's
events would sit in a queue nobody drains and ``close`` would wait out its whole
budget (gunicorn ``--preload`` and uwsgi fork after ``init``). So each queue is tracked
weakly, and one module-level hook (registered once) makes every live queue in the child
start over: new lock, condition variables, events and worker thread, an empty backlog
(what the parent still had queued is the parent's to send, never the child's) and a
fresh drop counter. Nothing inherited is acquired, since another parent thread may
have held it at the instant of the fork. A queue that was closed stays closed. A queue
built with ``reinit_on_fork=False`` is left alone for its owner to rebuild
(``ProfilingController`` replaces its whole uploader, queue included).
"""

from __future__ import annotations

import logging
import os
import queue
import threading
import time
import weakref
from typing import Callable, Generic, List, Optional, TypeVar

T = TypeVar("T")

_CLOSE = object()

DeliveryWarn = Callable[[str, Optional[BaseException], int], None]
DeliverySender = Callable[[List[T], Optional[float]], None]


class DeliveryQueue(Generic[T]):
    """Thread-backed bounded delivery queue modeled on the Go SDK."""

    def __init__(
        self,
        send: DeliverySender,
        capacity: int,
        batch_size: int = 20,
        flush_interval: float = 0.1,
        max_attempts: int = 3,
        retry_initial: float = 0.1,
        retry_max: float = 1.0,
        warn: Optional[DeliveryWarn] = None,
        name: str = "delivery",
        reinit_on_fork: bool = True,
    ) -> None:
        if capacity <= 0:
            raise ValueError("delivery queue capacity must be positive")
        self._send: DeliverySender = send
        self._capacity = capacity
        self._batch_size = max(1, min(batch_size, capacity)) if capacity else 1
        self._flush_interval = max(flush_interval, 0)
        self._max_attempts = max(max_attempts, 1)
        self._retry_initial = max(retry_initial, 0)
        self._retry_max = retry_max if retry_max >= self._retry_initial else self._retry_initial
        self._warn = warn
        self._name = name

        self._closed = False
        self._init_state()
        if reinit_on_fork:
            _track(self)  # before the worker starts: no running worker is ever untracked
        self._start_worker()

    def _init_state(self) -> None:
        """(Re)create everything that holds a lock, a condition variable or the backlog."""
        self._items: "queue.Queue[T | object]" = queue.Queue(maxsize=self._capacity)
        self._cancel = threading.Event()
        self._close_lock = threading.Lock()
        self._dropped = 0
        self._drop_lock = threading.Lock()
        self._last_error: Optional[BaseException] = None
        self._done = threading.Event()

    def _start_worker(self) -> None:
        self._thread = threading.Thread(
            target=self._run, name=f"lazyobserve-{self._name}", daemon=True
        )
        self._thread.start()

    def _reinit_after_fork(self) -> None:
        """Runs in the child right after ``fork``; only the forking thread exists.

        Nothing inherited is used, least of all acquired: the locks, the queue's
        mutex and the events may have been held by a parent thread at fork time and
        would stay locked forever. The backlog is dropped without being counted; the
        parent still owns and will send it.
        """
        self._init_state()
        if self._closed:
            self._done.set()  # shut down before the fork: stay closed, no worker
            return
        self._start_worker()

    # -- public API ---------------------------------------------------------

    def enqueue(self, item: T) -> bool:
        if not item or item is _CLOSE:
            return False
        with self._close_lock:
            if self._closed:
                self._record_drop("shutdown_budget", None, 1)
                return False
        try:
            self._items.put_nowait(item)
            return True
        except queue.Full:
            self._record_drop("queue_full", None, 1)
            return False

    def close(self, timeout: Optional[float] = None) -> Optional[BaseException]:
        with self._close_lock:
            if not self._closed:
                self._closed = True
                try:
                    self._items.put_nowait(_CLOSE)
                except queue.Full:
                    # The worker is busy; the stop flag still terminates draining.
                    pass
        if not self._done.wait(timeout):
            self._cancel.set()
            self._thread.join(timeout=min(timeout or 1.0, 1.0))
            return self._last_error or TimeoutError("delivery queue close timed out")
        return self._last_error

    def dropped(self) -> int:
        with self._drop_lock:
            return self._dropped

    def record_drop(
        self, reason: str, err: Optional[BaseException] = None, count: int = 1
    ) -> None:
        """Count a caller-side drop and emit the queue's warn callback.

        Used when the sender deliberately discards an item (for example a 4xx
        rejection) without treating it as a retryable failure.
        """
        self._record_drop(reason, err, count)

    # -- internals ----------------------------------------------------------

    def _record_drop(self, reason: str, err: Optional[BaseException], count: int) -> None:
        if count <= 0:
            return
        with self._drop_lock:
            self._dropped += count
        if self._warn is not None:
            self._warn(reason, err, count)

    def _set_error(self, err: BaseException) -> None:
        self._last_error = err

    def _retry_delay(self, retry_index: int) -> float:
        delay = self._retry_initial
        for _ in range(retry_index):
            if delay >= self._retry_max / 2:
                return self._retry_max
            delay *= 2.0
        return min(delay, self._retry_max)

    def _wait(self, delay: float) -> bool:
        """Return False when the queue should stop waiting."""
        deadline = time.monotonic() + delay
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return True
            if self._cancel.wait(min(remaining, 0.05)):
                return False

    def _run(self) -> None:
        timeout = self._flush_interval if self._flush_interval > 0 else None
        try:
            while True:
                try:
                    item = self._items.get(timeout=timeout)
                except queue.Empty:
                    if self._closed:
                        break
                    continue
                if item is _CLOSE:
                    self._drain_and_close()
                    return
                batch = [item]  # type: List[T]
                self._gather(batch)
                if not self._deliver(batch):
                    return
        finally:
            self._done.set()

    def _gather(self, batch: List[T]) -> None:
        deadline = time.monotonic() + self._flush_interval
        while len(batch) < self._batch_size:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return
            try:
                item = self._items.get(timeout=remaining)
            except queue.Empty:
                return
            if item is _CLOSE:
                self._closed = True
                return
            batch.append(item)

    def _deliver(self, batch: List[T]) -> bool:
        """Deliver a batch; returns True to continue, False to stop the worker."""
        err: Optional[BaseException] = None
        for attempt in range(self._max_attempts):
            err = None
            try:
                self._send(batch, None)
                return True
            except BaseException as exc:  # noqa: BLE001 - delivery must not crash the worker
                err = exc
            if self._cancel.is_set():
                self._set_error(err)
                self._record_drop("shutdown_budget", err, len(batch))
                return False
            if attempt + 1 == self._max_attempts:
                self._set_error(err)
                self._record_drop("retry_exhausted", err, len(batch))
                return True
            if not self._wait(self._retry_delay(attempt)):
                self._set_error(err or RuntimeError("delivery cancelled"))
                self._record_drop("shutdown_budget", err, len(batch))
                return False
        return True

    def _drain_and_close(self) -> None:
        """Drain items that were queued before close() within the shutdown budget."""
        remaining: List[T] = []
        while True:
            try:
                item = self._items.get_nowait()
            except queue.Empty:
                break
            if item is not _CLOSE:
                remaining.append(item)
        if not remaining:
            return
        if self._cancel.is_set():
            self._record_drop("shutdown_budget", None, len(remaining))
            return
        self._deliver(remaining)

    @property
    def name(self) -> str:
        return self._name


# --- fork hook -----------------------------------------------------------------------------

_queues: "List[weakref.ReferenceType[DeliveryQueue]]" = []
_queues_lock = threading.Lock()
_fork_hook_registered = False


def _track(delivery_queue: "DeliveryQueue") -> None:
    global _fork_hook_registered
    with _queues_lock:
        _queues[:] = [ref for ref in _queues if ref() is not None]  # queues come and go
        _queues.append(weakref.ref(delivery_queue))
        if not _fork_hook_registered and hasattr(os, "register_at_fork"):
            _fork_hook_registered = True
            os.register_at_fork(after_in_child=_after_fork_in_child)


def _after_fork_in_child() -> None:
    global _queues_lock
    # Another parent thread may have held the registry lock at the fork: replace it,
    # never acquire it. A copy of the list is enough, since this is the only thread.
    _queues_lock = threading.Lock()
    try:
        alive = [ref for ref in list(_queues) if ref() is not None]
        _queues[:] = alive
    except Exception:  # noqa: BLE001 - never let a fork hook break the child
        return
    for ref in alive:
        delivery_queue = ref()
        if delivery_queue is None:
            continue
        try:
            delivery_queue._reinit_after_fork()
        except Exception:  # noqa: BLE001 - one queue failing must not skip the rest
            continue


def default_warn_factory(logger: logging.Logger, signal: str) -> DeliveryWarn:
    def warn(reason: str, err: Optional[BaseException], count: int) -> None:
        detail = f" {err}" if err else ""
        logger.warning("%s telemetry dropped: reason=%s count=%d%s", signal, reason, count, detail)

    return warn
