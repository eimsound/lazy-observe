"""Lifecycle of Python continuous profiling (ADR 0014): the window loop.

``Config(profiling=True)`` makes ``build_client`` start one
:class:`ProfilingController`. It owns a single daemon thread that

1. builds the sampler (:meth:`lazyobserve._sampler.Sampler.create`, whose probe runs a
   0.1 s trial sample, hence in this thread and never inside ``build_client``) and,
   if the interpreter cannot be sampled, logs one warning with a stable reason code
   and ends;
2. otherwise loops like the Go SDK's ``profilingLoop``: re-check the GIL, collect one
   10 s window, encode it, queue the upload, then wait until ``profiling_interval``
   has passed since the window *started* (so an interval below 10 s means windows
   run back to back);
3. stops for good, with one warning, when the sampler disables itself (too many
   failed windows, failure rate over 50 %, GIL switched back on). There is no retry
   and no backoff for sampling; backoff exists for uploads only.

Everything here degrades silently (``sdk/AGENTS.md``): nothing is raised into the
application, not from the sampling thread, not from ``shutdown``, not from
``os.fork``.

**Warnings** are never the sampler's free-text callback. Each reason maps to a stable
code (:func:`warning_code`) and is logged once per process per code, with a link to
the user documentation. A forked child is a new process and may warn again.

**shutdown(timeout)** mirrors what Go does on ``ctx.Done()`` (stop sampling, then drain
the upload queue within the budget) and, like Go, follows the owner's decision 14 for
the partial window: sampling stops at once (``Sampler.stop``), a window that already ran
for at least 5 s is encoded and uploaded inside the budget, a shorter one is dropped
without counting as dropped telemetry.

**fork** (``os.register_at_fork``): the child inherits the parent's window in flight,
its aggregation and whatever locks other parent threads held at that instant, but
none of the threads. So an active controller rebuilds *everything* in the child (new
lock, new stop event, new sampler, new uploader and its queue) and starts a new
sampling thread. The parent's partial window and anything still queued in the parent's
uploader are never sent by the child, so a ``--preload`` master's samples are not
uploaded once per worker. ``upload_id`` is drawn from ``os.urandom`` when a window is
encoded, which is fork safe, so parent and child never share one. A controller that was
shut down, or disabled for an environmental reason, stays off in the child.
"""

from __future__ import annotations

import datetime
import logging
import os
import threading
import time
import weakref
from typing import Any, Callable, List, Optional

from ._profile_upload import CollectedProfile, ProfileUploader, new_upload_id
from ._sampler import (
    PROFILE_TYPE,
    REASON_DISABLED,
    REASON_FAILURE_RATE,
    REASON_GIL_BUILD,
    REASON_GIL_ENABLED,
    REASON_IMPORT_FAILED,
    REASON_MODE_MISSING,
    REASON_PLATFORM,
    REASON_PYTHON_TOO_OLD,
    REASON_SIGNATURE_MISMATCH,
    REASON_TRIAL_FAILED,
    WINDOW_SECONDS,
    Sampler,
)

#: Where the user documentation explains each warning code (written in S4).
DOC_URL = "https://github.com/eimsound/lazy-observe/blob/main/docs/sdk/python.md#continuous-profiling"
#: A window cut short by ``shutdown`` is uploaded only if it already ran this long.
SHUTDOWN_MIN_WINDOW_SECONDS = 5.0
THREAD_NAME = "lazyobserve-profiling"

#: Warning codes (ADR 0014); stable, documented, and the only thing users should match on.
CODE_PYTHON_TOO_OLD = "python_too_old"
CODE_UNSUPPORTED_PLATFORM = "unsupported_platform"
CODE_GIL_BUILD = "gil_build"
CODE_GIL_ENABLED = "gil_enabled"
CODE_PROBE_FAILED = "probe_failed"
CODE_TOO_MANY_FAILURES = "too_many_failures"
CODE_SAMPLE_FAILURE_RATE = "sample_failure_rate"
CODE_INTERNAL_ERROR = "internal_error"

_WARNING_CODES = {
    REASON_PYTHON_TOO_OLD: CODE_PYTHON_TOO_OLD,
    REASON_PLATFORM: CODE_UNSUPPORTED_PLATFORM,
    REASON_GIL_BUILD: CODE_GIL_BUILD,
    REASON_GIL_ENABLED: CODE_GIL_ENABLED,
    REASON_IMPORT_FAILED: CODE_PROBE_FAILED,
    REASON_SIGNATURE_MISMATCH: CODE_PROBE_FAILED,
    REASON_MODE_MISSING: CODE_PROBE_FAILED,
    REASON_TRIAL_FAILED: CODE_PROBE_FAILED,
    REASON_DISABLED: CODE_TOO_MANY_FAILURES,
    REASON_FAILURE_RATE: CODE_SAMPLE_FAILURE_RATE,
}

_EPOCH = datetime.datetime(1970, 1, 1, tzinfo=datetime.timezone.utc)

RecordDrop = Callable[[str, str, int], None]


def warning_code(reason: Optional[str]) -> str:
    """Stable warning code for a ``_sampler.REASON_*`` value."""
    return _WARNING_CODES.get(reason or "", CODE_INTERNAL_ERROR)


# --- one warning per class per process ---------------------------------------------

_warned: set = set()
_warned_lock = threading.Lock()


def _warn_once(logger: logging.Logger, code: str, detail: str) -> None:
    with _warned_lock:
        if code in _warned:
            return
        _warned.add(code)
    try:
        logger.warning(
            "lazyobserve continuous profiling is off: reason=%s (%s); see %s", code, detail, DOC_URL
        )
    except Exception:  # noqa: BLE001 - a broken log handler must not reach the app
        return


def _reset_warning_state() -> None:
    """Forget which warnings were emitted (tests; a forked child does it by itself)."""
    global _warned, _warned_lock
    _warned_lock = threading.Lock()
    _warned = set()


def report_start_failure(logger: logging.Logger, exc: BaseException) -> None:
    """``build_client`` could not even start profiling: warn, never raise."""
    _warn_once(logger, CODE_INTERNAL_ERROR, repr(exc))


# --- the controller ---------------------------------------------------------------------


class ProfilingController:
    """Owns the sampling thread and its uploader; see the module docstring.

    Every dependency is injected, so the loop is testable without a real interpreter
    sampler: ``sampler_factory`` returns an object with the ``Sampler`` surface,
    ``uploader_factory`` one with ``enqueue``/``close``, ``clock`` and ``sleep`` drive
    time (``sleep(delay)`` returns True once the loop should stop).
    """

    def __init__(
        self,
        *,
        interval: float,
        sampler_factory: Callable[[], Any],
        uploader_factory: Callable[[], Any],
        record_drop: RecordDrop,
        logger: logging.Logger,
        window_seconds: float = WINDOW_SECONDS,
        min_shutdown_window: float = SHUTDOWN_MIN_WINDOW_SECONDS,
        new_upload_id: Callable[[], str] = new_upload_id,
        clock: Callable[[], float] = time.monotonic,
        sleep: Optional[Callable[[float], bool]] = None,
    ) -> None:
        self._interval = interval
        self._sampler_factory = sampler_factory
        self._uploader_factory = uploader_factory
        self._record_drop = record_drop
        self._logger = logger
        self._window_seconds = window_seconds
        self._min_shutdown_ns = int(min_shutdown_window * 1_000_000_000)
        self._new_upload_id = new_upload_id
        self._clock = clock
        self._custom_sleep = sleep

        self._lock = threading.Lock()  # guards _closed, _sampler, _uploader
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None
        self._sampler: Any = None
        self._uploader: Any = None
        self._started = False
        self._closed = False
        self._disabled = False  # the loop ended for an environmental reason

    # --- public ---------------------------------------------------------------------

    @property
    def interval(self) -> float:
        return self._interval

    def start(self) -> None:
        """Start the sampling thread (once; a no-op after ``shutdown``)."""
        with self._lock:
            if self._closed or self._started:
                return
            self._started = True
            self._launch()
        _track(self)

    def is_running(self) -> bool:
        thread = self._thread
        return thread is not None and thread.is_alive()

    def join(self, timeout: Optional[float] = None) -> bool:
        """Wait for the sampling thread to end; ``True`` once it has (or never ran)."""
        thread = self._thread
        if thread is None:
            return True
        thread.join(timeout)
        return not thread.is_alive()

    def shutdown(self, timeout: float = 10.0) -> Optional[BaseException]:
        """Stop sampling now, upload a partial window of at least 5 s, drain the queue.

        Bounded by ``timeout`` (one budget shared by waiting for the sampling thread
        and closing the upload queue). Idempotent. Never raises; the only error
        result is the upload queue's, as for every other signal.
        """
        with self._lock:
            if self._closed:
                return None
            self._closed = True
            sampler = self._sampler
            thread = self._thread
        deadline = time.monotonic() + max(0.0, timeout)

        self._stop.set()
        if sampler is not None:
            try:
                sampler.stop()
            except Exception:  # noqa: BLE001
                pass
        if thread is not None and thread is not threading.current_thread():
            thread.join(max(0.0, deadline - time.monotonic()))

        with self._lock:
            uploader = self._uploader
        if uploader is None:
            return None
        try:
            return uploader.close(max(0.0, deadline - time.monotonic()))
        except Exception as exc:  # noqa: BLE001
            return exc

    # --- thread -----------------------------------------------------------------------

    def _launch(self) -> None:
        thread = threading.Thread(target=self._run, name=THREAD_NAME, daemon=True)
        self._thread = thread
        thread.start()

    def _run(self) -> None:
        try:
            self._loop()
        except Exception as exc:  # noqa: BLE001 - nothing may escape the thread
            self._disabled = True
            _warn_once(self._logger, CODE_INTERNAL_ERROR, repr(exc))

    def _loop(self) -> None:
        try:
            sampler = self._sampler_factory()  # probes (0.1 s trial sample) in this thread
        except Exception as exc:  # noqa: BLE001 - the probe catches its own; this is belt and braces
            self._disabled = True
            _warn_once(self._logger, CODE_PROBE_FAILED, repr(exc))
            return
        with self._lock:
            if self._closed:
                return
            self._sampler = sampler
            uploader = self._uploader_factory() if sampler.available() else None
            self._uploader = uploader
        if uploader is None:
            self._give_up(sampler)
            return

        while not self._stop.is_set():
            cycle_start = self._clock()
            if not sampler.recheck_gil():
                self._give_up(sampler)
                return
            self._one_window(sampler, uploader)
            if sampler.disabled():
                self._give_up(sampler)
                return
            if self._stop.is_set():
                return
            delay = max(0.0, self._interval - (self._clock() - cycle_start))
            if self._sleep(delay):
                return

    def _sleep(self, delay: float) -> bool:
        if self._custom_sleep is not None:
            return self._custom_sleep(delay)
        return self._stop.wait(delay)

    def _one_window(self, sampler: Any, uploader: Any) -> None:
        window = sampler.collect_window(self._window_seconds)
        failed_ticks = sampler.last_failed_ticks()
        if failed_ticks > 0:  # also for a window the sampler discarded
            self._record_drop("profile", "sample_failed", failed_ticks)
        if window is None:
            return
        if self._stop.is_set() and window.duration_ns < self._min_shutdown_ns:
            return  # cut short by shutdown before it was worth uploading
        data = sampler.encode_window(window)
        if data is None:
            return
        uploader.enqueue(
            CollectedProfile(
                upload_id=self._new_upload_id(),  # at encode time, in the process that uploads
                type_name=PROFILE_TYPE,
                start=_timestamp(window.start_ns),
                end=_timestamp(window.end_ns),
                data=data,
            )
        )

    def _give_up(self, sampler: Any) -> None:
        """The sampler is unavailable or disabled for good: warn once and end."""
        self._disabled = True
        _warn_once(self._logger, warning_code(sampler.reason()), sampler.detail())

    # --- fork -------------------------------------------------------------------------

    def _reinit_after_fork(self) -> None:
        """Runs in the child right after ``fork``; only the forking thread exists."""
        active = self._started and not self._closed and not self._disabled
        # Whatever the parent's other threads held at fork time stays locked forever
        # in the child, so nothing inherited is reused: not the lock, the stop event
        # (it has its own condition lock), the sampler (its window in flight and its
        # failure counters), nor the uploader (queue, lock and a worker thread that no
        # longer exists, possibly holding windows the parent is going to upload).
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._thread = None
        self._sampler = None
        self._uploader = None
        if active:
            self._launch()


def _timestamp(ns: int) -> datetime.datetime:
    return _EPOCH + datetime.timedelta(microseconds=ns // 1000)


# --- fork hook ---------------------------------------------------------------------------

_controllers: List["weakref.ReferenceType[ProfilingController]"] = []
_fork_hook_registered = False


def _track(controller: ProfilingController) -> None:
    global _fork_hook_registered
    _controllers.append(weakref.ref(controller))
    if not _fork_hook_registered and hasattr(os, "register_at_fork"):
        _fork_hook_registered = True
        os.register_at_fork(after_in_child=_after_fork_in_child)


def _after_fork_in_child() -> None:
    global _controllers
    try:
        _reset_warning_state()  # a new process warns again, once per code
        alive = [ref for ref in list(_controllers) if ref() is not None]
        _controllers = alive
    except Exception:  # noqa: BLE001 - never let a fork hook break the child
        return
    for ref in alive:
        controller = ref()
        if controller is None:
            continue
        try:
            controller._reinit_after_fork()
        except Exception:  # noqa: BLE001 - one controller failing must not skip the rest
            continue


# --- wiring into the client ---------------------------------------------------------------


def build_uploader(client: Any, parsed: Any, config: Any, logger: logging.Logger) -> ProfileUploader:
    """Profile uploader whose queue drops are counted and logged like Go's."""

    def warn(reason: str, err: Optional[BaseException], count: int) -> None:
        client.record_telemetry_drop("profile", reason, count)
        detail = f" error={err}" if err else ""
        logger.warning("profile telemetry dropped: reason=%s profiles=%d%s", reason, count, detail)

    return ProfileUploader(
        endpoint=parsed.endpoint,
        token=parsed.token,
        service=config.service_name,
        max_attempts=config.delivery.max_attempts,
        retry_initial=config.delivery.retry_initial,
        retry_max=config.delivery.retry_max,
        warn=warn,
    )


def start_profiling(client: Any, parsed: Any, config: Any, logger: logging.Logger) -> ProfilingController:
    """Create and start the controller for ``Config(profiling=True)``.

    Returns at once: the probe, including its trial sample, runs on the sampling
    thread.
    """

    def debug_warn(reason: str, err: Optional[BaseException], failures: int) -> None:
        logger.debug("profiling window failed: %s (consecutive=%d): %r", reason, failures, err)

    controller = ProfilingController(
        interval=config.profiling_interval,
        sampler_factory=lambda: Sampler.create(warn=debug_warn),
        uploader_factory=lambda: build_uploader(client, parsed, config, logger),
        record_drop=client.record_telemetry_drop,
        logger=logger,
    )
    controller.start()
    return controller
