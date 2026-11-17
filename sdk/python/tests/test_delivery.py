import threading
import time

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
