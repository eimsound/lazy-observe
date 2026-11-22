"""Tests for profile upload: multipart contract, upload ids, and degradation."""

from __future__ import annotations

import datetime
import threading
import time

import httpx

from lazy_observe._profile_upload import (
    CollectedProfile,
    ProfileUploader,
    new_upload_id,
)

ENDPOINT = "http://127.0.0.1:3100"
TOKEN = "secret-token"
SERVICE = "checkout"
START = datetime.datetime(2026, 11, 18, 12, 0, 0, tzinfo=datetime.timezone.utc)
END = START + datetime.timedelta(seconds=60)


class _FakeTransport(httpx.BaseTransport):
    """Records requests; ``responses`` items are status codes, (status, headers), or exceptions."""

    def __init__(self, responses) -> None:
        self.responses = list(responses)
        self.requests = []
        self.timeouts = []
        self.closed = False

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        timeout = request.extensions.get("timeout", {})
        self.timeouts.append(timeout.get("read"))
        item = self.responses.pop(0)
        if isinstance(item, tuple):
            status, headers = item
            return httpx.Response(status, headers=headers, content=b"rejected")
        if isinstance(item, int):
            if 200 <= item < 300:
                return httpx.Response(item, content=b"")
            return httpx.Response(item, content=b"rejected")
        if isinstance(item, BaseException):
            raise item
        return httpx.Response(200, content=item)

    def close(self) -> None:
        self.closed = True


class _BlockingTransport(httpx.BaseTransport):
    """First request blocks until released, so the queue stays full."""

    def __init__(self) -> None:
        self.requests = []
        self.release = threading.Event()

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        self.release.wait(timeout=2.0)
        return httpx.Response(201, content=b"")


def _parse_multipart(body: bytes, content_type: str):
    prefix = "multipart/form-data; boundary="
    assert content_type.startswith(prefix), content_type
    boundary = content_type[len(prefix) :].encode()
    parts = body.split(b"--" + boundary)[1:-1]
    fields = {}
    files = {}
    for part in parts:
        part = part.lstrip(b"\r\n")
        head, _, payload = part.partition(b"\r\n\r\n")
        payload = payload[:-2]
        headers = {}
        for line in head.split(b"\r\n"):
            key, _, value = line.partition(b": ")
            headers[key.decode().lower()] = value.decode()
        disposition = headers["content-disposition"]
        name = disposition.split('name="', 1)[1].split('"', 1)[0]
        if "filename=" in disposition:
            files[name] = (headers.get("content-type"), payload)
        else:
            fields[name] = payload.decode()
    return fields, files


def _wait_for(predicate, timeout: float = 2.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.01)
    raise AssertionError("condition not reached in time")


def _profile(data: bytes = b"\x1f\x8bprofile") -> CollectedProfile:
    return CollectedProfile(
        upload_id="a" * 32,
        type_name="cpu",
        start=START,
        end=END,
        data=data,
    )


def _uploader(transport, **overrides) -> ProfileUploader:
    fields = {
        "endpoint": ENDPOINT,
        "token": TOKEN,
        "service": SERVICE,
        "transport": transport,
        "queue_capacity": 6,
        "max_attempts": 1,
        "retry_initial": 0.01,
        "retry_max": 0.02,
    }
    fields.update(overrides)
    return ProfileUploader(**fields)


def test_upload_posts_the_go_sdk_multipart_contract():
    transport = _FakeTransport([201])
    uploader = _uploader(transport)
    assert uploader.enqueue(_profile(b"gzip-bytes"))
    _wait_for(lambda: len(transport.requests) == 1)
    assert uploader.close(timeout=2.0) is None

    request = transport.requests[0]
    assert str(request.url) == ENDPOINT + "/api/profiles"
    assert request.method == "POST"
    assert request.headers["Authorization"] == "Bearer " + TOKEN
    content_type = request.headers["Content-Type"]
    fields, files = _parse_multipart(request.content, content_type)
    assert fields == {
        "service": SERVICE,
        "upload_id": "a" * 32,
        "type": "cpu",
        "start": "2026-11-18T12:00:00.000000Z",
        "end": "2026-11-18T12:01:00.000000Z",
    }
    assert files == {"profile": ("application/octet-stream", b"gzip-bytes")}
    assert transport.timeouts == [15.0]
    assert transport.closed is True
    assert uploader.dropped() == 0


def test_upload_id_is_unique_and_hex32():
    ids = [new_upload_id() for _ in range(1000)]
    assert all(len(value) == 32 for value in ids)
    assert all(all(char in "0123456789abcdef" for char in value) for value in ids)
    assert len(set(ids)) == len(ids)


def test_404_is_silent_and_counted_as_server_rejected():
    transport = _FakeTransport([404, 201])
    drops = []
    uploader = _uploader(
        transport,
        max_attempts=3,
        warn=lambda reason, err, count: drops.append((reason, count)),
    )
    assert uploader.enqueue(_profile())
    _wait_for(lambda: len(transport.requests) == 1)
    assert uploader.close(timeout=2.0) is None
    assert len(transport.requests) == 1
    assert uploader.dropped() == 1
    assert drops == [("server_rejected", 1)]


def test_409_conflict_is_silent_and_counted_as_server_rejected():
    transport = _FakeTransport([409, 201])
    drops = []
    uploader = _uploader(
        transport,
        max_attempts=3,
        warn=lambda reason, err, count: drops.append((reason, count)),
    )
    assert uploader.enqueue(_profile())
    _wait_for(lambda: len(transport.requests) == 1)
    assert uploader.close(timeout=2.0) is None
    assert len(transport.requests) == 1
    assert uploader.dropped() == 1
    assert drops == [("server_rejected", 1)]


def test_408_and_429_are_retried_until_accepted():
    transport = _FakeTransport([408, (429, {"Retry-After": "0.02"}), 201])
    drops = []
    uploader = _uploader(
        transport,
        max_attempts=3,
        retry_initial=0.01,
        retry_max=0.02,
        warn=lambda reason, err, count: drops.append((reason, count)),
    )
    assert uploader.enqueue(_profile())
    _wait_for(lambda: len(transport.requests) == 3)
    assert uploader.close(timeout=2.0) is None
    assert len(transport.requests) == 3
    assert uploader.dropped() == 0
    assert drops == []


def test_profile_exceeding_10mb_is_rejected_before_upload():
    from lazy_observe._profile_upload import MAX_PROFILE_SIZE

    transport = _FakeTransport([201])
    drops = []
    uploader = _uploader(
        transport,
        max_attempts=3,
        warn=lambda reason, err, count: drops.append((reason, count)),
    )
    oversized = _profile(b"x" * (MAX_PROFILE_SIZE + 1))
    assert uploader.enqueue(oversized) is False
    assert uploader.close(timeout=2.0) is None
    assert transport.requests == []
    assert uploader.dropped() == 1
    assert drops == [("server_rejected", 1)]


def test_profile_uploader_refuses_redirects_and_ignores_global_opener():
    import urllib.request
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

    target_hits = []

    class _Target(BaseHTTPRequestHandler):
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

    target = ThreadingHTTPServer(("127.0.0.1", 0), _Target)
    target_port = target.server_address[1]
    threading.Thread(target=target.serve_forever, daemon=True).start()

    class _Redirect(BaseHTTPRequestHandler):
        def do_POST(self):
            self.rfile.read(int(self.headers.get("Content-Length", "0")))
            self.send_response(302)
            self.send_header("Location", f"http://127.0.0.1:{target_port}/steal")
            self.end_headers()

        def log_message(self, *args):
            pass

    redirect = ThreadingHTTPServer(("127.0.0.1", 0), _Redirect)
    redirect_port = redirect.server_address[1]
    threading.Thread(target=redirect.serve_forever, daemon=True).start()

    class _Hijack(urllib.request.BaseHandler):
        def http_open(self, req):
            raise AssertionError("global urllib opener must not be used")

    prev_opener = urllib.request._opener
    urllib.request.install_opener(urllib.request.build_opener(_Hijack()))
    try:
        uploader = ProfileUploader(
            endpoint=f"http://127.0.0.1:{redirect_port}",
            token="lo_secret",
            service=SERVICE,
            max_attempts=1,
            retry_initial=0.0,
        )
        uploader.enqueue(_profile())
        err = uploader.close(timeout=3.0)
        assert err is not None
        assert target_hits == []
        assert uploader._http.closed is True
    finally:
        urllib.request.install_opener(prev_opener)
        redirect.shutdown()
        redirect.server_close()
        target.shutdown()
        target.server_close()


def test_5xx_is_retried_until_accepted():
    transport = _FakeTransport([503, 500, 201])
    uploader = _uploader(transport, max_attempts=3)
    assert uploader.enqueue(_profile())
    _wait_for(lambda: len(transport.requests) == 3)
    assert uploader.close(timeout=2.0) is None
    assert len(transport.requests) == 3
    assert uploader.dropped() == 0


def test_network_errors_exhaust_the_retry_budget_and_drop():
    transport = _FakeTransport(
        [httpx.ConnectError("connection refused"), httpx.ConnectError("connection refused")]
    )
    drops = []
    uploader = _uploader(
        transport,
        max_attempts=2,
        warn=lambda kind, err, count: drops.append((kind, count)),
    )
    assert uploader.enqueue(_profile())
    _wait_for(lambda: len(transport.requests) == 2)
    uploader.close(timeout=2.0)
    assert len(transport.requests) == 2
    assert uploader.dropped() == 1
    assert drops == [("retry_exhausted", 1)]


def test_queue_is_bounded_and_reports_drops():
    transport = _BlockingTransport()
    uploader = _uploader(transport, queue_capacity=1)
    try:
        results = [uploader.enqueue(_profile()) for _ in range(20)]
        assert False in results
        assert uploader.dropped() > 0
    finally:
        transport.release.set()
        uploader.close(timeout=2.0)
