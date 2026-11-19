"""``os.fork`` and the error delivery queue (gunicorn ``--preload``, uwsgi).

``lazyobserve.init`` starts the ``lazyobserve-error-events`` thread behind
``ErrorTransport``. A forked child inherits the queue, its locks and whatever the
parent had queued, but not the thread, so without a fork hook the child's error
events are never sent and ``shutdown`` waits out its whole budget. These tests use a
real ``os.fork`` and a local ``ThreadingHTTPServer`` as the platform; the child
reaches it over a socket, so every delivery shows up in the parent's capture list.
"""

from __future__ import annotations

import json
import os
import signal
import threading
import time
import warnings
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

import lazyobserve
from lazyobserve.delivery import DeliveryQueue

pytestmark = pytest.mark.skipif(not hasattr(os, "fork"), reason="needs os.fork")

SHUTDOWN_BUDGET = 2.0
HELD = "in-flight"  # the event whose request the platform holds until the gate opens


class Platform:
    """A local stand-in for the platform that records every error event it receives."""

    def __init__(self):
        self.lock = threading.Lock()
        self.messages = []  # exception value of every event received on /api/events/errors
        self.gate = threading.Event()  # set = answer at once; clear = hold the request
        self.gate.set()
        self.arrived = threading.Event()  # a request reached the server

        platform = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                body = self.rfile.read(int(self.headers["Content-Length"]))
                if self.path == "/api/events/errors":
                    messages = [e["exception"]["value"] for e in json.loads(body)["events"]]
                    if HELD in messages:  # only the request carrying HELD waits for the gate
                        platform.arrived.set()
                        platform.gate.wait(10)
                    with platform.lock:
                        platform.messages.extend(messages)
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"accepted":1}')

            def log_message(self, *args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.dsn = f"http://127.0.0.1:{self.server.server_address[1]}/p/default@TOKEN"

    def received(self):
        with self.lock:
            return list(self.messages)

    def wait_for(self, message, timeout=5.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if message in self.received():
                return True
            time.sleep(0.01)
        return False

    def close(self):
        self.gate.set()
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def platform():
    server = Platform()
    yield server
    server.close()


@pytest.fixture
def client(platform):
    created = lazyobserve.init(
        dsn=platform.dsn,
        service_name="fork-svc",
        export_interval=3600.0,  # the OTel pipeline is not under test: no periodic export
        delivery=lazyobserve.DeliveryConfig(max_attempts=1, retry_initial=0.0),
    )
    assert created.error_transport is not None, created.err()
    yield created
    created.shutdown(SHUTDOWN_BUDGET)


def run_in_child(fn, timeout=20):
    """Run ``fn`` in a forked child and return ``(wait status, JSON payload)``.

    A SIGALRM watchdog kills a child that deadlocks, which shows up as a signalled
    exit status instead of a hung test run.
    """
    read_fd, write_fd = os.pipe()
    with warnings.catch_warnings():
        warnings.simplefilter("ignore")  # "fork() in a multi-threaded process" (3.12+)
        pid = os.fork()
    if pid == 0:
        code = 1
        try:
            os.close(read_fd)
            signal.signal(signal.SIGALRM, signal.SIG_DFL)  # the watchdog kills a deadlocked child
            signal.alarm(timeout)
            payload = fn()
            os.write(write_fd, json.dumps(payload).encode())
            code = 0
        except BaseException as exc:  # noqa: BLE001 - report instead of hanging the parent
            try:
                os.write(write_fd, json.dumps({"error": repr(exc)}).encode())
            except BaseException:  # noqa: BLE001
                pass
        finally:
            os._exit(code)
    os.close(write_fd)
    chunks = []
    while True:
        data = os.read(read_fd, 65536)
        if not data:
            break
        chunks.append(data)
    os.close(read_fd)
    _, status = os.waitpid(pid, 0)
    raw = b"".join(chunks)
    return status, json.loads(raw) if raw else None


def assert_child_ok(status, payload):
    assert not os.WIFSIGNALED(status), f"child killed by signal {os.WTERMSIG(status)} (deadlock?)"
    assert os.WEXITSTATUS(status) == 0, payload
    assert payload is not None and "error" not in payload, payload
    return payload


@pytest.fixture(autouse=True)
def _watchdog():
    """A deadlocked test fails after 60 s instead of hanging the run."""
    previous = signal.signal(signal.SIGALRM, lambda *_: (_ for _ in ()).throw(TimeoutError("test watchdog")))
    signal.alarm(60)
    yield
    signal.alarm(0)
    signal.signal(signal.SIGALRM, previous)


def test_child_error_events_are_delivered_and_shutdown_returns_within_budget(platform, client):
    def child():
        client.capture_exception(RuntimeError("from-child"))
        started = time.monotonic()
        err = client.shutdown(SHUTDOWN_BUDGET)
        return {"err": repr(err), "elapsed": time.monotonic() - started}

    payload = assert_child_ok(*run_in_child(child))

    assert payload["err"] == "None"
    assert payload["elapsed"] < SHUTDOWN_BUDGET - 0.5
    assert "from-child" in platform.received()


def test_parent_keeps_delivering_after_a_child_was_forked(platform, client):
    assert_child_ok(*run_in_child(lambda: {"ok": True}))

    client.capture_exception(RuntimeError("from-parent"))

    assert platform.wait_for("from-parent")


def test_events_still_queued_in_the_parent_are_not_sent_again_by_the_child(platform, client):
    platform.gate.clear()  # the platform holds the parent's first request in flight
    client.capture_exception(RuntimeError(HELD))
    assert platform.arrived.wait(5)
    client.capture_exception(RuntimeError("queued-at-fork"))  # waits behind the in-flight batch

    def child():
        client.capture_exception(RuntimeError("from-child"))
        return {"err": repr(client.shutdown(SHUTDOWN_BUDGET))}

    assert_child_ok(*run_in_child(child))
    assert platform.received().count("from-child") == 1
    assert "queued-at-fork" not in platform.received()  # the child sent none of the parent's events

    platform.gate.set()  # the parent is still in charge of what it queued
    assert platform.wait_for(HELD)
    assert platform.wait_for("queued-at-fork")
    time.sleep(0.3)
    assert platform.received().count("queued-at-fork") == 1
    assert platform.received().count(HELD) == 1


def test_a_child_forked_while_another_thread_holds_the_queue_locks_does_not_deadlock(platform, client):
    queue = client.error_transport.delivery
    holding = threading.Event()
    done = threading.Event()

    def hold_locks():
        # Locks a parent thread happens to hold at fork time. The child inherits them
        # locked, with no thread left to release them.
        with queue._close_lock, queue._drop_lock, queue._items.mutex:
            holding.set()
            done.wait(30)

    holder = threading.Thread(target=hold_locks, daemon=True)
    holder.start()
    assert holding.wait(5)

    def child():
        client.capture_exception(RuntimeError("from-locked-child"))  # takes _close_lock and the queue's mutex
        return {"err": repr(client.shutdown(SHUTDOWN_BUDGET))}

    try:
        payload = assert_child_ok(*run_in_child(child))
    finally:
        done.set()
        holder.join(5)

    assert payload["err"] == "None"
    assert platform.wait_for("from-locked-child")


def test_the_fork_hook_survives_into_grandchildren(platform, client):
    def child():
        def grandchild():
            client.capture_exception(RuntimeError("from-grandchild"))
            return {"err": repr(client.shutdown(SHUTDOWN_BUDGET))}

        status, payload = run_in_child(grandchild)
        return {"status": status, "payload": payload}

    outer = assert_child_ok(*run_in_child(child))
    inner = assert_child_ok(outer["status"], outer["payload"])

    assert inner["err"] == "None"
    assert platform.wait_for("from-grandchild")


def test_a_queue_closed_before_the_fork_stays_closed_in_the_child(platform, client):
    assert client.shutdown(SHUTDOWN_BUDGET) is None
    delivery = client.error_transport.delivery

    def child():
        # This queue's own worker, not a process-wide count: an unclosed queue left by
        # another test would be revived in the child and show up under the same name.
        revived = delivery._thread.is_alive()
        started = time.monotonic()
        accepted = client.error_transport.enqueue(object())  # a closed queue refuses
        err = client.shutdown(SHUTDOWN_BUDGET)
        return {
            "revived": revived,
            "accepted": accepted,
            "err": repr(err),
            "elapsed": time.monotonic() - started,
        }

    payload = assert_child_ok(*run_in_child(child))

    assert payload["revived"] is False  # no worker is revived for a queue that was shut down
    assert payload["accepted"] is False
    assert payload["elapsed"] < 1.0


def test_a_queue_that_opted_out_is_left_alone_for_its_owner(platform):
    """The profile uploader's owner (``ProfilingController``) rebuilds it itself."""
    sent = []
    owned = DeliveryQueue(lambda batch, _t: sent.extend(batch), capacity=4, reinit_on_fork=False)
    try:

        def child():
            return {"alive": owned._thread.is_alive()}

        payload = assert_child_ok(*run_in_child(child))
        assert payload["alive"] is False  # untouched: still the parent's dead thread object
    finally:
        owned.close(1.0)


def test_the_error_queue_is_restarted_once_and_the_profile_uploader_is_left_to_its_controller(platform, client):
    from lazyobserve._profile_upload import ProfileUploader

    uploader = ProfileUploader(platform.dsn, "tok", "fork-svc")  # shares DeliveryQueue with the error transport
    errors = client.error_transport.delivery
    parent_worker = errors._thread

    def error_workers():
        return sum(1 for t in threading.enumerate() if t.name == "lazyobserve-error-events")

    # Compared with the parent rather than a fixed 1: a queue another test left open is
    # revived as well, and that is correct.
    parent_error_workers = error_workers()
    try:

        def child():
            return {
                "error_workers": error_workers(),
                "error_restarted": errors._thread is not parent_worker and errors._thread.is_alive(),
                "uploader_alive": uploader.delivery._thread.is_alive(),
            }

        payload = assert_child_ok(*run_in_child(child))

        assert payload["error_restarted"] is True
        assert payload["error_workers"] == parent_error_workers  # one worker per live queue, never two
        assert payload["uploader_alive"] is False  # ProfilingController builds its own in the child
    finally:
        uploader.close(1.0)


# --- Client._drop_lock ------------------------------------------------------------------------


def test_a_child_forked_while_another_thread_holds_the_drop_lock_does_not_deadlock(platform, client):
    """``record_telemetry_drop`` is what the profile uploader's warn callback and the
    ``sample_failed`` count call: a child that inherited ``_drop_lock`` locked would hang
    on the first drop it records."""
    client.record_telemetry_drop("profile", "queue_full", 3)  # the parent's own drops
    holding = threading.Event()
    done = threading.Event()

    def hold_lock():
        with client._drop_lock:
            holding.set()
            done.wait(30)

    holder = threading.Thread(target=hold_lock, daemon=True)
    holder.start()
    assert holding.wait(5)

    def child():
        client.record_telemetry_drop("profile", "sample_failed", 2)
        err = client.shutdown(SHUTDOWN_BUDGET)
        return {"err": repr(err), "dropped": client.diagnostics().dropped_telemetry}

    try:
        payload = assert_child_ok(*run_in_child(child))
    finally:
        done.set()
        holder.join(5)

    assert payload["err"] == "None"
    assert payload["dropped"] == 2  # the child counts its own drops; the parent's 3 stay the parent's
    client.record_telemetry_drop("profile", "sample_failed", 1)  # the parent's lock is untouched
    assert client.diagnostics().dropped_telemetry == 4


def test_the_drop_lock_is_replaced_again_in_grandchildren(platform, client):
    def child():
        holding = threading.Event()
        done = threading.Event()

        def hold_lock():
            with client._drop_lock:  # the child's own lock, held at the grandchild's fork
                holding.set()
                done.wait(30)

        holder = threading.Thread(target=hold_lock, daemon=True)
        holder.start()
        assert holding.wait(5)

        def grandchild():
            client.record_telemetry_drop("profile", "sample_failed", 1)
            return {"dropped": client.diagnostics().dropped_telemetry}

        try:
            status, payload = run_in_child(grandchild)
        finally:
            done.set()
            holder.join(5)
        return {"status": status, "payload": payload}

    outer = assert_child_ok(*run_in_child(child))
    inner = assert_child_ok(outer["status"], outer["payload"])

    assert inner["dropped"] == 1


def test_the_fork_hook_does_not_keep_a_client_alive(platform):
    import gc
    import weakref

    from lazyobserve.client import degraded_client

    ghost = degraded_client(lazyobserve.Config(), RuntimeError("never used"))
    ref = weakref.ref(ghost)
    del ghost
    gc.collect()

    assert ref() is None
    assert_child_ok(*run_in_child(lambda: {"ok": True}))  # forking with a dead entry in the registry
