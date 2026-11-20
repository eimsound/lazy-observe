import io
import json
import logging
import os
import sysconfig
import threading
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from lazyobserve.error_event import (
    _EVENT_SIZE_LIMIT,
    ErrorEvent,
    ErrorFrame,
    ErrorTransport,
    _in_app,
    collect_frames,
    exception_type_name,
    fit_event_to_size_limit,
    new_event_id,
)


class _CaptureHandler(BaseHTTPRequestHandler):
    captured = []

    def do_POST(self):
        length = int(self.headers["Content-Length"])
        body = self.rfile.read(length)
        auth = self.headers.get("Authorization", "")
        _CaptureHandler.captured.append((self.path, auth, json.loads(body)))
        payload = json.dumps({"accepted": 1}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *args):
        pass


@pytest.fixture
def capture_server():
    _CaptureHandler.captured = []
    server = ThreadingHTTPServer(("127.0.0.1", 0), _CaptureHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield server
    server.shutdown()
    server.server_close()


def _raise_somewhere():
    raise RuntimeError("kaboom")


def test_collect_frames_outermost_first():
    try:
        _raise_somewhere()
    except RuntimeError as exc:
        frames = collect_frames(exc.__traceback__)
    assert len(frames) >= 2
    # outermost first: caller below raiser
    assert frames[-1].function == "_raise_somewhere"
    assert frames[-1].module.endswith("test_error_event")
    assert frames[-1].lineno is not None
    assert frames[-1].in_app is True


def test_exception_type_name():
    assert exception_type_name(RuntimeError("x")) == "RuntimeError"


def test_event_id_hex32():
    assert len(new_event_id()) == 32


def test_payload_shape():
    event = ErrorEvent(
        event_id="a" * 32,
        timestamp="2024-01-01T00:00:00.000000Z",
        platform="python",
        release="1.0",
        tags={"service.name": "svc"},
        exception_type="ValueError",
        exception_value="nope",
        frames=[
            __import__(
                "lazyobserve.error_event",
                fromlist=["ErrorFrame"],
            ).ErrorFrame(function="f", filename="x.py", lineno=3)
        ],
    )
    payload = event.to_json()
    assert payload["exception"]["type"] == "ValueError"
    assert payload["exception"]["value"] == "nope"
    frame = payload["exception"]["stacktrace"]["frames"][0]
    assert frame["function"] == "f"
    assert frame["filename"] == "x.py"
    assert frame["lineno"] == 3
    assert "abs_path" not in frame
    assert "module" not in frame
    assert "contexts" not in payload


def test_transport_upload(capture_server):
    port = capture_server.server_address[1]
    transport = ErrorTransport(
        endpoint=f"http://127.0.0.1:{port}",
        token="tok",
        logger=__import__("logging").getLogger("test"),
        max_attempts=2,
        retry_initial=0.01,
    )
    event = ErrorEvent(
        event_id="b" * 32,
        timestamp="2024-01-01T00:00:00.000000Z",
        platform="python",
        level="error",
        message="boom",
        exception_type="RuntimeError",
        exception_value="boom",
        tags={"service.name": "svc"},
    )
    transport.enqueue(event)
    transport.close(timeout=3.0)
    assert len(_CaptureHandler.captured) == 1
    path, auth, payload = _CaptureHandler.captured[0]
    assert path == "/api/events/errors"
    assert auth == "Bearer tok"
    assert payload["events"][0]["event_id"] == "b" * 32
    assert payload["events"][0]["platform"] == "python"


def test_in_app_excludes_stdlib_third_party_and_frameworks():
    stdlib_dir = sysconfig.get_path("stdlib") or os.path.dirname(os.__file__)
    assert _in_app("orders.views", "/srv/app/orders/views.py") is True
    assert _in_app("lazyobserve.django", "/srv/app/lazyobserve/django.py") is False
    assert _in_app("django.core.handlers.base", "/srv/app/django/core/handlers/base.py") is False
    assert _in_app("rest_framework.views", "/srv/app/rest_framework/views.py") is False
    assert _in_app("opentelemetry.sdk.trace", "/srv/app/otel.py") is False
    assert _in_app("json", os.path.join(stdlib_dir, "json", "__init__.py")) is False
    assert _in_app("custom_stdlib_path", os.path.join(stdlib_dir, "socket.py")) is False
    assert _in_app("thirdparty.mod", "/app/.venv/lib/python3.13/site-packages/thirdparty/mod.py") is False
    assert _in_app("thirdparty.mod", "/usr/lib/python3/dist-packages/thirdparty/mod.py") is False
    assert _in_app("thirdparty.mod", r"C:\venv\Lib\site-packages\thirdparty\mod.py") is False
    assert _in_app("importlib._bootstrap", "<frozen importlib._bootstrap>") is False


def test_fit_event_trims_huge_event_within_200kb_and_keeps_outer_and_inner_frames():
    huge_frames = [
        ErrorFrame(
            function=f"frame_{i}",
            filename=f"f{i}.py",
            lineno=i,
            abs_path=f"/very/long/path/{'x' * 120}/f{i}.py",
            module="app.recurse",
            in_app=True,
        )
        for i in range(1000)
    ]
    event = ErrorEvent(
        event_id="c" * 32,
        timestamp="2024-01-01T00:00:00.000000Z",
        platform="python",
        message="M" * (250 << 10),
        exception_type="RecursionError",
        exception_value="V" * (250 << 10),
        frames=huge_frames,
        tags={"service.name": "svc", "big": "T" * 10000},
    )
    fitted = fit_event_to_size_limit(event)
    assert fitted is not None
    encoded = json.dumps(fitted.to_json(), separators=(",", ":")).encode("utf-8")
    assert len(encoded) <= _EVENT_SIZE_LIMIT
    assert fitted.frames[0].function == "frame_0"
    assert fitted.frames[-1].function == "frame_999"


def test_untrimmable_oversized_event_is_dropped_without_poisoning_batch(capture_server):
    port = capture_server.server_address[1]
    drops = []
    transport = ErrorTransport(
        endpoint=f"http://127.0.0.1:{port}",
        token="tok",
        logger=logging.getLogger("test"),
        max_attempts=1,
        retry_initial=0.0,
        warn=lambda reason, err, count: drops.append((reason, count)),
    )
    huge_untrimmable = ErrorEvent(
        event_id="x" * (210 << 10),
        timestamp="2024-01-01T00:00:00.000000Z",
        platform="python",
        exception_type="ValueError",
        exception_value="bad",
    )
    normal = ErrorEvent(
        event_id="d" * 32,
        timestamp="2024-01-01T00:00:00.000000Z",
        platform="python",
        exception_type="ValueError",
        exception_value="good",
    )
    assert transport.enqueue(huge_untrimmable) is False
    assert transport.enqueue(normal) is True
    assert transport.close(timeout=3.0) is None
    assert drops == [("server_rejected", 1)]
    assert len(_CaptureHandler.captured) == 1
    assert [e["event_id"] for e in _CaptureHandler.captured[0][2]["events"]] == ["d" * 32]


@pytest.mark.parametrize("status_code", [400, 401, 403, 404, 409, 413, 422])
def test_transport_drops_permanent_4xx_immediately_as_server_rejected_and_closes_response(
    status_code,
):
    calls = []
    drops = []
    closed_flags = []

    class _TrackedBody(io.BytesIO):
        def close(self):
            closed_flags.append(True)
            super().close()

    def opener(request, timeout=None):
        calls.append(request)
        raise urllib.error.HTTPError(
            request.full_url, status_code, "bad", {}, _TrackedBody(b"rejected")
        )

    transport = ErrorTransport(
        endpoint="http://127.0.0.1:1",
        token="tok",
        logger=logging.getLogger("test"),
        max_attempts=3,
        retry_initial=0.01,
        warn=lambda reason, err, count: drops.append((reason, count)),
        opener=opener,
    )
    event = ErrorEvent(
        event_id="e" * 32,
        timestamp="2024-01-01T00:00:00.000000Z",
        platform="python",
        exception_type="ValueError",
        exception_value="bad",
    )
    transport.enqueue(event)
    assert transport.close(timeout=2.0) is None
    assert len(calls) == 1
    assert drops == [("server_rejected", 1)]
    assert closed_flags


@pytest.mark.parametrize("status_code", [408, 429])
def test_transport_retries_408_and_429_and_honors_retry_after(status_code):
    calls = []
    drops = []

    class _OkResp:
        def read(self, n=-1):
            return b'{"accepted":1}'

        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

    def opener(request, timeout=None):
        calls.append(request)
        if len(calls) == 1:
            raise urllib.error.HTTPError(
                request.full_url,
                status_code,
                "retry",
                {"Retry-After": "0.02"},
                io.BytesIO(b"slow down"),
            )
        return _OkResp()

    transport = ErrorTransport(
        endpoint="http://127.0.0.1:1",
        token="tok",
        logger=logging.getLogger("test"),
        max_attempts=2,
        retry_initial=0.0,
        warn=lambda reason, err, count: drops.append((reason, count)),
        opener=opener,
    )
    transport.enqueue(
        ErrorEvent(
            event_id="f" * 32,
            timestamp="2024-01-01T00:00:00.000000Z",
            platform="python",
            exception_type="RuntimeError",
            exception_value="transient",
        )
    )
    assert transport.close(timeout=2.0) is None
    assert len(calls) == 2
    assert drops == []


def test_transport_ignores_global_opener_and_refuses_redirects():
    target_hits = []

    class _TargetHandler(BaseHTTPRequestHandler):
        def do_GET(self):
            target_hits.append(("GET", self.headers.get("Authorization")))
            self.send_response(200)
            self.end_headers()

        def do_POST(self):
            target_hits.append(("POST", self.headers.get("Authorization")))
            self.send_response(200)
            self.end_headers()

        def log_message(self, *args):
            pass

    target_srv = ThreadingHTTPServer(("127.0.0.1", 0), _TargetHandler)
    target_port = target_srv.server_address[1]
    threading.Thread(target=target_srv.serve_forever, daemon=True).start()

    class _RedirectHandler(BaseHTTPRequestHandler):
        def do_POST(self):
            self.rfile.read(int(self.headers.get("Content-Length", "0")))
            self.send_response(302)
            self.send_header("Location", f"http://127.0.0.1:{target_port}/steal")
            self.end_headers()

        def log_message(self, *args):
            pass

    redirect_srv = ThreadingHTTPServer(("127.0.0.1", 0), _RedirectHandler)
    redirect_port = redirect_srv.server_address[1]
    threading.Thread(target=redirect_srv.serve_forever, daemon=True).start()

    class _HijackHandler(urllib.request.BaseHandler):
        def http_open(self, req):
            raise AssertionError("global urllib opener must not be used")

    prev_opener = urllib.request._opener
    urllib.request.install_opener(urllib.request.build_opener(_HijackHandler()))
    try:
        drops = []
        transport = ErrorTransport(
            endpoint=f"http://127.0.0.1:{redirect_port}",
            token="lo_secret_token",
            logger=logging.getLogger("test"),
            max_attempts=1,
            retry_initial=0.0,
            warn=lambda reason, err, count: drops.append((reason, count)),
        )
        transport.enqueue(
            ErrorEvent(
                event_id="1" * 32,
                timestamp="2024-01-01T00:00:00.000000Z",
                platform="python",
                exception_type="RuntimeError",
                exception_value="boom",
            )
        )
        err = transport.close(timeout=3.0)
        assert err is not None
        assert target_hits == []
        assert drops == [("retry_exhausted", 1)]
    finally:
        urllib.request.install_opener(prev_opener)
        redirect_srv.shutdown()
        redirect_srv.server_close()
        target_srv.shutdown()
        target_srv.server_close()
