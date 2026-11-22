import threading
import time

import pytest

from lazy_observe.delivery import DeliveryQueue


def test_enqueue_and_batch_flush():
    sent = []
    queue = DeliveryQueue(
        send=lambda batch, _t: sent.append(batch),
        capacity=10,
        batch_size=5,
        flush_interval=0.02,
        max_attempts=1,
    )
    queue.enqueue("a")
    queue.enqueue("b")
    time.sleep(0.15)
    assert len(sent) == 1
    assert sent[0] == ["a", "b"]


def test_capacity_drops_with_local_warning():
    sent = []
    drops = []
    queue = DeliveryQueue(
        send=lambda batch, _t: time.sleep(0.05),
        capacity=3,
        batch_size=1,
        flush_interval=0.01,
        max_attempts=1,
        warn=lambda reason, err, count: drops.append((reason, count)),
    )
    for i in range(20):
        queue.enqueue(i)
    queue.close(timeout=2.0)
    assert queue.dropped() > 0
    assert any(reason == "queue_full" for reason, _ in drops)


def test_retry_then_drop_on_persistent_failure():
    attempts = 0
    drops = []

    def sender(batch, _t):
        nonlocal attempts
        attempts += 1
        raise RuntimeError("downstream down")

    queue = DeliveryQueue(
        send=sender,
        capacity=5,
        batch_size=1,
        flush_interval=0.01,
        max_attempts=2,
        retry_initial=0.01,
        retry_max=0.02,
        warn=lambda reason, err, count: drops.append(reason),
    )
    queue.enqueue("x")
    queue.close(timeout=2.0)
    assert attempts == 2
    assert queue.dropped() == 1
    assert "retry_exhausted" in drops


def test_close_drains_queued_items():
    sent = []

    def sender(batch, _t):
        sent.extend(batch)
        time.sleep(0.01)

    queue = DeliveryQueue(
        send=sender,
        capacity=10,
        batch_size=1,
        flush_interval=0.01,
        max_attempts=2,
        retry_initial=0.01,
        retry_max=0.02,
    )
    for i in range(5):
        queue.enqueue(f"e{i}")
    queue.close(timeout=3.0)
    assert sorted(sent) == ["e0", "e1", "e2", "e3", "e4"]


def test_close_is_idempotent():
    queue = DeliveryQueue(
        send=lambda batch, _t: None,
        capacity=2,
        batch_size=1,
        flush_interval=0.01,
        max_attempts=1,
    )
    queue.enqueue("a")
    queue.close(timeout=2.0)
    queue.close(timeout=2.0)
    assert not queue.enqueue("b")  # closed: enqueue rejected


def test_record_drop_counts_and_warns():
    drops = []
    queue = DeliveryQueue(
        send=lambda batch, _t: None,
        capacity=2,
        batch_size=1,
        flush_interval=0.01,
        max_attempts=1,
        warn=lambda reason, err, count: drops.append((reason, err, count)),
    )
    queue.record_drop("rejected", None, 2)
    assert queue.dropped() == 2
    assert drops == [("rejected", None, 2)]
    queue.close(timeout=2.0)


@pytest.mark.parametrize("budget", [0, 0.0, -1.0])
def test_close_with_no_budget_returns_at_once_and_counts_the_rest_as_shutdown_budget(budget):
    entered = threading.Event()
    release = threading.Event()
    sent = []
    drops = []

    def sender(batch, _t):
        entered.set()
        release.wait(10)  # the worker is stuck in the first send
        sent.extend(batch)

    queue = DeliveryQueue(
        send=sender,
        capacity=10,
        batch_size=1,
        flush_interval=0.01,
        max_attempts=1,
        warn=lambda reason, err, count: drops.append((reason, count)),
    )
    try:
        queue.enqueue("in-flight")
        assert entered.wait(5)
        queue.enqueue("queued-1")
        queue.enqueue("queued-2")

        started = time.monotonic()
        err = queue.close(budget)
        elapsed = time.monotonic() - started

        assert elapsed < 0.5  # no grace period on top of an exhausted budget (it used to be 1 s)
        assert isinstance(err, TimeoutError)
        assert drops == [("shutdown_budget", 2)]  # what was still queued, like Go's dropBatchAndQueued
        assert queue.dropped() == 2
    finally:
        release.set()
    queue.close(timeout=2.0)
    assert sent == ["in-flight"]  # the queued items were dropped, not sent late


def test_close_with_a_small_budget_still_gives_the_worker_a_moment_to_stop():
    release = threading.Event()
    queue = DeliveryQueue(
        send=lambda batch, _t: release.wait(10),
        capacity=4,
        batch_size=1,
        flush_interval=0.01,
        max_attempts=1,
    )
    queue.enqueue("stuck")
    time.sleep(0.1)
    started = time.monotonic()
    err = queue.close(0.1)
    elapsed = time.monotonic() - started
    release.set()
    assert isinstance(err, TimeoutError)
    assert 0.1 <= elapsed < 0.9  # the budget plus a join bounded by it (0.1 s), not 1 s


def test_a_worker_blocked_on_an_empty_queue_still_stops_after_a_budget_spent_close():
    """With ``flush_interval=0`` the worker blocks on ``get`` with no timeout, so only the
    close sentinel ends it. A full queue keeps ``close`` from enqueueing one; the discard
    that empties the queue must leave one behind."""
    entered = threading.Event()
    release = threading.Event()
    drops = []

    def sender(batch, _t):
        entered.set()
        release.wait(10)

    queue = DeliveryQueue(
        send=sender,
        capacity=2,
        batch_size=1,
        flush_interval=0,
        max_attempts=1,
        warn=lambda reason, err, count: drops.append((reason, count)),
    )
    queue.enqueue("in-flight")
    assert entered.wait(5)
    assert queue.enqueue("queued-1") and queue.enqueue("queued-2")  # full: no room for the sentinel

    err = queue.close(0)
    release.set()

    assert isinstance(err, TimeoutError)
    assert drops == [("shutdown_budget", 2)]
    assert queue._done.wait(5)  # the worker saw a sentinel and stopped


def test_a_cancelled_queue_sends_nothing_and_counts_the_batch_as_shutdown_budget():
    """Go checks ``runCtx.Err()`` before the first send. Once the budget is spent the batch
    the worker picks up is dropped and counted, not sent as one last request."""
    sent = []
    drops = []
    queue = DeliveryQueue(
        send=lambda batch, _t: sent.append(list(batch)),
        capacity=4,
        batch_size=1,
        flush_interval=0.01,
        max_attempts=3,
        warn=lambda reason, err, count: drops.append((reason, count)),
    )
    queue._cancel.set()  # the budget is already spent when the worker takes the batch
    queue.enqueue("a")

    err = queue.close(timeout=2.0)  # waits for the worker; no sleep on the clock
    assert sent == []
    assert drops == [("shutdown_budget", 1)]
    assert queue.dropped() == 1
    assert isinstance(err, TimeoutError)  # the spent budget, whoever reads the error first


def test_a_retry_is_not_sent_when_the_queue_is_cancelled_while_waiting():
    """Every attempt, not only the first, checks for cancellation before it sends."""
    sent = []
    drops = []

    def sender(batch, _t):
        sent.append(list(batch))
        raise RuntimeError("boom")

    queue = DeliveryQueue(
        send=sender,
        capacity=4,
        batch_size=1,
        flush_interval=0.01,
        max_attempts=3,
        retry_initial=0,
        warn=lambda reason, err, count: drops.append((reason, count)),
    )

    def cancelled_during_the_wait(_delay):
        queue._cancel.set()  # cancel lands as the delay ends: _wait still says "go on"
        return True

    queue._wait = cancelled_during_the_wait  # type: ignore[method-assign]
    queue.enqueue("a")

    assert queue._done.wait(5)
    assert sent == [["a"]]  # the first attempt only; no second request after the cancel
    assert drops == [("shutdown_budget", 1)]
    assert queue.dropped() == 1


def test_enqueue_accepts_falsy_non_none_items_and_rejects_none():
    sent = []
    q = DeliveryQueue(
        send=lambda batch, _t: sent.extend(batch),
        capacity=10,
        batch_size=5,
        flush_interval=0.01,
        max_attempts=1,
    )
    assert q.enqueue(None) is False
    assert q.enqueue(0) is True
    assert q.enqueue("") is True
    assert q.close(timeout=2.0) is None
    assert sent == [0, ""]


def test_drain_and_close_chunks_backlog_by_batch_size():
    entered = threading.Event()
    release = threading.Event()
    batch_sizes = []

    def sender(batch, _t):
        batch_sizes.append(len(batch))
        if len(batch_sizes) == 1:
            entered.set()
            release.wait(5)

    q = DeliveryQueue(
        send=sender,
        capacity=100,
        batch_size=20,
        flush_interval=0.01,
        max_attempts=1,
    )
    q.enqueue("first")
    assert entered.wait(5)
    for i in range(55):
        assert q.enqueue(f"item-{i}")
    release.set()
    assert q.close(timeout=3.0) is None
    assert all(size <= 20 for size in batch_sizes)
    assert sum(batch_sizes) == 56


def test_last_error_is_cleared_after_subsequent_successful_delivery():
    calls = 0

    def sender(batch, _t):
        nonlocal calls
        calls += 1
        if batch == ["fail"]:
            raise RuntimeError("transient outage")

    q = DeliveryQueue(
        send=sender,
        capacity=10,
        batch_size=1,
        flush_interval=0.01,
        max_attempts=1,
        retry_initial=0.0,
    )
    q.enqueue("fail")
    deadline = time.monotonic() + 2.0
    while q.dropped() < 1 and time.monotonic() < deadline:
        time.sleep(0.01)
    assert q.dropped() == 1

    q.enqueue("ok")
    assert q.close(timeout=2.0) is None


def test_flush_interval_zero_exits_cleanly_on_close_with_backlog():
    entered = threading.Event()
    release = threading.Event()
    sent = []

    def sender(batch, _t):
        if not entered.is_set():
            entered.set()
            release.wait(5)
        sent.extend(batch)

    q = DeliveryQueue(
        send=sender,
        capacity=3,
        batch_size=2,
        flush_interval=0,
        max_attempts=1,
    )
    q.enqueue("a")
    assert entered.wait(5)
    assert q.enqueue("b")
    assert q.enqueue("c")
    assert q.enqueue("d")  # queue is now full; close() cannot enqueue _CLOSE

    closer = threading.Thread(target=lambda: q.close(timeout=3.0))
    closer.start()
    time.sleep(0.05)
    release.set()
    closer.join(timeout=3.0)
    assert not closer.is_alive()
    assert q._done.is_set()
    assert sorted(sent) == ["a", "b", "c", "d"]


def test_enqueue_and_close_race_never_silently_loses_accepted_items():
    for _ in range(25):
        sent = []
        drops = []
        q = DeliveryQueue(
            send=lambda batch, _t: sent.extend(batch),
            capacity=50,
            batch_size=10,
            flush_interval=0.005,
            max_attempts=1,
            warn=lambda reason, err, count: drops.append((reason, count)),
        )
        accepted = []

        def producer():
            for i in range(40):
                if q.enqueue(f"e-{i}"):
                    accepted.append(f"e-{i}")

        t = threading.Thread(target=producer)
        t.start()
        time.sleep(0.001)
        q.close(timeout=2.0)
        t.join(timeout=2.0)
        assert sorted(sent) == sorted(accepted)


def test_retry_delay_applies_jitter_and_respects_retry_after():
    from lazy_observe.delivery import MAX_RETRY_AFTER, RetryableError, parse_retry_after

    q = DeliveryQueue(
        send=lambda batch, _t: None,
        capacity=4,
        batch_size=1,
        flush_interval=0.01,
        max_attempts=3,
        retry_initial=0.1,
        retry_max=1.0,
    )
    try:
        samples = [q._retry_delay(0) for _ in range(30)]
        assert all(0.05 <= s <= 0.1 for s in samples)
        assert len(set(samples)) > 1  # jitter varies delays

        # Retry-After overrides short exponential backoff up to MAX_RETRY_AFTER
        err = RetryableError("503", retry_after=1.0)
        assert q._retry_delay(0, err) >= 1.0
        capped = RetryableError("503", retry_after=999.0)
        assert q._retry_delay(0, capped) == MAX_RETRY_AFTER
        assert parse_retry_after("1") == 1.0
        assert parse_retry_after("120") == MAX_RETRY_AFTER
        assert parse_retry_after("invalid") is None
    finally:
        q.close(timeout=2.0)
