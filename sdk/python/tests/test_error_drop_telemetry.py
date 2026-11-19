"""Error-event drops are counted in ``Diagnostics().dropped_telemetry``.

The Go, Node and Browser SDKs feed every drop of their error-event queue into the
``dropped_telemetry`` count and the ``lazy_observe_sdk_dropped_telemetry`` counter with
``signal="error"``. These tests run ``lazyobserve.init`` against a local platform that
can answer 5xx or hold a request, and check the three reasons the queue can drop for:
``queue_full``, ``retry_exhausted`` and ``shutdown_budget``.
"""

from __future__ import annotations

import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

import lazyobserve


class _Platform:
    """``/api/events/errors`` answers 200, 503, or holds the request until ``release``."""

    def __init__(self) -> None:
        self.status = 200
        self.hold = False
        self.release = threading.Event()
        self.arrived = threading.Event()  # a request reached the server
        platform = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                platform.arrived.set()
                if platform.hold:
                    platform.release.wait(10)
                self.send_response(platform.status)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"accepted":1}')

            def log_message(self, *args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.dsn = f"http://127.0.0.1:{self.server.server_address[1]}/p/default@TOKEN"

    def close(self) -> None:
        self.release.set()
        self.server.shutdown()
        self.server.server_close()


class _CounterRecorder:
    """Stands in for the ``lazy_observe_sdk_dropped_telemetry`` OTel counter."""

    def __init__(self) -> None:
        self.calls = []

    def add(self, amount, attributes):
        self.calls.append((amount, dict(attributes)))


@pytest.fixture
def platform():
    server = _Platform()
    yield server
    server.close()


def _init(platform, **delivery):
    client = lazyobserve.init(
        dsn=platform.dsn,
        service_name="drop-svc",
        export_interval=3600.0,  # the OTel pipeline is not under test: no periodic export
        delivery=lazyobserve.DeliveryConfig(**delivery),
    )
    assert client.error_transport is not None, client.err()
    assert client._dropped_counter is not None  # metrics are on by default
    counter = _CounterRecorder()
    client._dropped_counter = counter
    return client, counter


def _wait_until(predicate, timeout=5.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(0.01)
    return predicate()


def test_a_full_error_queue_counts_queue_full(platform):
    platform.hold = True  # the first event stays in flight and the queue behind it fills
    client, counter = _init(platform, error_queue_capacity=2, max_attempts=1, retry_initial=0.0)
    try:
        client.capture_exception(ValueError("in flight"))
        assert platform.arrived.wait(5)
        client.capture_exception(ValueError("queued 1"))
        client.capture_exception(ValueError("queued 2"))
        assert client.diagnostics().dropped_telemetry == 0

        client.capture_exception(ValueError("one too many"))

        assert client.diagnostics().dropped_telemetry == 1
        assert counter.calls == [(1, {"signal": "error", "reason": "queue_full"})]
    finally:
        platform.release.set()
        client.shutdown(2.0)


def test_a_batch_that_keeps_failing_counts_retry_exhausted(platform):
    platform.status = 503
    client, counter = _init(platform, error_queue_capacity=10, max_attempts=2, retry_initial=0.0)
    try:
        client.capture_exception(ValueError("platform is down"))

        assert _wait_until(lambda: client.diagnostics().dropped_telemetry >= 1)
        assert client.diagnostics().dropped_telemetry == 1
        assert counter.calls == [(1, {"signal": "error", "reason": "retry_exhausted"})]
    finally:
        platform.status = 200  # or the final OTLP metrics export keeps retrying past shutdown
        client.shutdown(2.0)


def test_events_left_when_the_shutdown_budget_runs_out_count_shutdown_budget(platform):
    platform.hold = True
    client, counter = _init(platform, error_queue_capacity=10, max_attempts=1, retry_initial=0.0)
    try:
        client.capture_exception(ValueError("in flight"))
        assert platform.arrived.wait(5)
        client.capture_exception(ValueError("queued 1"))
        client.capture_exception(ValueError("queued 2"))

        err = client.shutdown(0.3)  # the held request never finishes inside the budget

        assert isinstance(err, TimeoutError)
        # ``shutdown`` waits for its close step and ``close`` waits for the worker on the
        # same deadline, so ``shutdown`` may return just before ``close`` has discarded.
        assert _wait_until(lambda: client.diagnostics().dropped_telemetry >= 2)
        assert client.diagnostics().dropped_telemetry == 2
        assert counter.calls == [(2, {"signal": "error", "reason": "shutdown_budget"})]

        # The in-flight batch is the worker's: once the platform answers it is delivered,
        # and nothing already counted is counted again.
        platform.release.set()
        time.sleep(0.4)
        assert client.diagnostics().dropped_telemetry == 2
        assert len(counter.calls) == 1
    finally:
        platform.release.set()


def test_the_drop_is_still_logged(platform, caplog):
    platform.hold = True
    client, _ = _init(platform, error_queue_capacity=1, max_attempts=1, retry_initial=0.0)
    try:
        client.capture_exception(ValueError("in flight"))
        assert platform.arrived.wait(5)
        client.capture_exception(ValueError("queued"))
        with caplog.at_level("WARNING", logger="lazyobserve"):
            client.capture_exception(ValueError("dropped"))
        assert "error telemetry dropped: reason=queue_full count=1" in caplog.text
    finally:
        platform.release.set()
        client.shutdown(2.0)
