import threading
import time

import pytest

from lazyobserve.delivery import DeliveryQueue


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
