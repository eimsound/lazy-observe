"""Client.shutdown: no pseudo errors for unused signals, one shared deadline.

The Go SDK passes a single ``ctx`` to every shutdown step (error transport, profiling,
OTel providers, in that order); a step that finds the ctx expired still runs and
reports the expiry. These tests pin the same shape for Python.
"""

import itertools
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

import lazyobserve
import lazyobserve.client as client_module
from tests.helpers import make_client


class _OkHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, *args):
        pass


@pytest.fixture
def ok_server():
    server = ThreadingHTTPServer(("127.0.0.1", 0), _OkHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield server
    server.shutdown()
    server.server_close()


# With every signal flag off the SDK turns all three on, so that combination is the
# "all on" one; the others select a subset. Error events are always on.
_COMBOS = [c for c in itertools.product([False, True], repeat=3)]


@pytest.mark.parametrize(("metrics", "logs", "traces"), _COMBOS)
def test_shutdown_reports_no_error_for_any_signal_combination(ok_server, metrics, logs, traces):
    port = ok_server.server_address[1]
    lo = lazyobserve.init(
        dsn=f"http://127.0.0.1:{port}/p/default@TOKEN",
        service_name="shutdown-combo",
        metrics=metrics,
        logs=logs,
        traces=traces,
    )
    assert not lo.degraded
    lo.capture_exception(ValueError("boom"))
    assert lo.shutdown(timeout=2.0) is None


def test_traces_only_shutdown_has_no_noop_meter_error():
    lo = lazyobserve.init(
        dsn="http://127.0.0.1:1/p/default@TOKEN", service_name="traces-only", traces=True
    )
    assert lo.shutdown(1.0) is None


def test_degraded_client_shutdown_has_no_error():
    lo = lazyobserve.init({"dsn": "garbage", "service_name": "x"})
    assert lo.degraded
    assert lo.shutdown(timeout=1.0) is None


# --- one shared deadline ---------------------------------------------------------------


class _Steps:
    """Records the order, the budget and the thread of every shutdown step."""

    def __init__(self):
        self.calls = []
        self.threads = []

    def record(self, name, budget=None):
        self.calls.append((name, budget))
        self.threads.append(threading.get_ident())

    def names(self):
        return [name for name, _ in self.calls]

    def wait_for(self, count, timeout=5.0):
        # A step whose budget is already spent is started and not waited for, so it may
        # record itself only after shutdown has returned.
        deadline = time.monotonic() + timeout
        while len(self.calls) < count and time.monotonic() < deadline:
            time.sleep(0.01)
        assert len(self.calls) >= count, self.calls


class _FakeTransport:
    def __init__(self, steps, hold):
        self.steps = steps
        self.hold = hold  # seconds the close call blocks, whatever budget it was given
        self.release = threading.Event()

    def close(self, timeout=None):
        self.steps.record("errors", timeout)
        self.release.wait(self.hold)
        return None


class _FakeProfiling:
    def __init__(self, steps):
        self.steps = steps

    def shutdown(self, timeout=10.0):
        self.steps.record("profiling", timeout)
        return None


class _FakeProvider:
    def __init__(self, steps, name, raises=None):
        self.steps = steps
        self.name = name
        self.raises = raises

    def shutdown(self, *args, **kwargs):
        self.steps.record(self.name)
        if self.raises is not None:
            raise self.raises


def _rigged_client(steps, hold, provider_raises=None):
    client = make_client()
    transport = _FakeTransport(steps, hold)
    client.error_transport = transport
    client.profiling = _FakeProfiling(steps)
    client.logger_provider = _FakeProvider(steps, "logs")
    client.tracer_provider = _FakeProvider(steps, "traces", provider_raises)
    client.meter_provider = _FakeProvider(steps, "metrics")
    return client, transport


def test_stuck_error_transport_cannot_push_shutdown_past_the_timeout():
    steps = _Steps()
    client, transport = _rigged_client(steps, hold=5.0)
    begin = time.monotonic()
    try:
        err = client.shutdown(0.4)
    finally:
        transport.release.set()
    elapsed = time.monotonic() - begin

    # Generous slack for slow CI; without the shared deadline this took the full 5 s.
    assert elapsed < 0.4 + 0.4, elapsed
    assert isinstance(err, TimeoutError)  # the budget ran out: reported, not raised
    # Every later step was still called. They all start with no budget left, so they
    # run concurrently and the order they record in is not fixed (free-threaded builds).
    steps.wait_for(5)
    assert steps.names()[0] == "errors"
    assert sorted(steps.names()[1:]) == ["logs", "metrics", "profiling", "traces"]


def test_every_step_draws_on_one_deadline():
    steps = _Steps()
    client, transport = _rigged_client(steps, hold=0.2)
    begin = time.monotonic()
    err = client.shutdown(1.0)
    elapsed = time.monotonic() - begin

    assert err is None
    assert elapsed < 1.0
    # Each step finished inside its budget, so each started after the previous one.
    assert steps.names() == ["errors", "profiling", "logs", "traces", "metrics"]
    budgets = dict(steps.calls)
    assert budgets["errors"] == pytest.approx(1.0, abs=0.1)
    # The error transport held 0.2 s of the budget: profiling only gets what is left.
    assert 0.0 < budgets["profiling"] <= budgets["errors"] - 0.2 + 0.02


def test_exhausted_budget_still_calls_profiling_with_nothing_left():
    steps = _Steps()
    client, transport = _rigged_client(steps, hold=5.0)
    try:
        client.shutdown(0.2)
    finally:
        transport.release.set()
    steps.wait_for(2)
    budgets = dict(steps.calls)
    assert budgets["profiling"] == pytest.approx(0.0, abs=0.05)


def test_a_failing_provider_does_not_stop_the_others_nor_raise():
    steps = _Steps()
    client, transport = _rigged_client(steps, hold=0.0, provider_raises=RuntimeError("exporter down"))
    err = client.shutdown(1.0)
    assert isinstance(err, RuntimeError)
    assert "traces shutdown" in str(err)
    assert [name for name, _ in steps.calls][-3:] == ["logs", "traces", "metrics"]


def test_real_error_queue_with_a_hung_sender_stays_inside_the_timeout():
    # The real queue waits out its budget and then grants itself up to 1 s more to join
    # the worker; shutdown must not pass that on to the caller.
    from lazyobserve.delivery import default_warn_factory
    from lazyobserve.error_event import ErrorTransport

    release = threading.Event()
    client = make_client()
    transport = ErrorTransport(
        endpoint="http://127.0.0.1:1/api/events/errors",
        token="TOKEN",
        logger=client.logger,
        queue_capacity=10,
        max_attempts=1,
        retry_initial=0.0,
        retry_max=0.0,
        warn=default_warn_factory(client.logger, "error"),
    )
    transport.delivery._send = lambda batch, _timeout: release.wait(10.0)
    client.error_transport = transport
    client.capture_exception(ValueError("stuck"))
    time.sleep(0.2)  # let the worker pick the event up and hang in the sender

    begin = time.monotonic()
    try:
        err = client.shutdown(0.5)
    finally:
        release.set()
    elapsed = time.monotonic() - begin
    # Without the shared deadline the queue's own extra join made this 0.5 + 1 s.
    assert elapsed < 0.5 + 0.4, elapsed
    assert err is not None


# --- no thread can be started --------------------------------------------------------


def test_shutdown_while_finalizing_runs_every_step_inline(monkeypatch):
    # At interpreter teardown Thread.start raises (3.12+) or blocks for good (3.9-3.11).
    monkeypatch.setattr(client_module.sys, "is_finalizing", lambda: True)
    steps = _Steps()
    client, _ = _rigged_client(steps, hold=0.0)
    assert client.shutdown(1.0) is None
    assert steps.names() == ["errors", "profiling", "logs", "traces", "metrics"]
    assert set(steps.threads) == {threading.get_ident()}


def test_shutdown_when_threads_cannot_start_runs_inline_and_does_not_raise(monkeypatch):
    steps = _Steps()
    client, _ = _rigged_client(steps, hold=0.0, provider_raises=RuntimeError("exporter down"))

    def refuse(self):
        raise RuntimeError("can't start new thread")

    monkeypatch.setattr(client_module.threading.Thread, "start", refuse)
    err = client.shutdown(1.0)
    assert isinstance(err, RuntimeError)
    assert "traces shutdown" in str(err)
    assert steps.names() == ["errors", "profiling", "logs", "traces", "metrics"]
