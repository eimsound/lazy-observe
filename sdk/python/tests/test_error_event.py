import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from lazyobserve.error_event import (
    ErrorEvent,
    ErrorTransport,
    collect_frames,
    exception_type_name,
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
