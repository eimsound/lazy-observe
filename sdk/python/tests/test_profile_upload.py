"""Tests for profile upload: multipart contract, upload ids, and degradation."""

from __future__ import annotations

import datetime
import io
import threading
import time
import urllib.error

from lazyobserve._profile_upload import (
    CollectedProfile,
    ProfileUploader,
    new_upload_id,
)

ENDPOINT = "http://127.0.0.1:3100"
TOKEN = "secret-token"
SERVICE = "checkout"
START = datetime.datetime(2026, 11, 18, 12, 0, 0, tzinfo=datetime.timezone.utc)
END = START + datetime.timedelta(seconds=60)


class _FakeResponse:
    def __init__(self, body: bytes = b"") -> None:
        self._body = body

    def read(self, size: int = -1) -> bytes:
        return self._body if size != 0 else b""

    def __enter__(self):
        return self

    def __exit__(self, *args):
        return False


class _FakeOpener:
    """Records requests; ``responses`` items are status codes or exceptions."""

    def __init__(self, responses) -> None:
        self.responses = list(responses)
        self.requests = []
        self.timeouts = []

    def __call__(self, request, timeout=None):
        self.requests.append(request)
        self.timeouts.append(timeout)
        item = self.responses.pop(0)
        if isinstance(item, int):
            if 200 <= item < 300:
                return _FakeResponse()
            raise urllib.error.HTTPError(
                request.full_url, item, "status", {}, io.BytesIO(b"rejected")
            )
        if isinstance(item, BaseException):
            raise item
        return _FakeResponse(item)


class _BlockingOpener:
    """First request blocks until released, so the queue stays full."""

    def __init__(self) -> None:
        self.requests = []
        self.release = threading.Event()

    def __call__(self, request, timeout=None):
        self.requests.append(request)
        self.release.wait(timeout=2.0)
        return _FakeResponse()


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


def _uploader(opener, **overrides) -> ProfileUploader:
    fields = {
        "endpoint": ENDPOINT,
        "token": TOKEN,
        "service": SERVICE,
        "opener": opener,
        "queue_capacity": 6,
        "max_attempts": 1,
        "retry_initial": 0.01,
        "retry_max": 0.02,
    }
    fields.update(overrides)
    return ProfileUploader(**fields)


def test_upload_posts_the_go_sdk_multipart_contract():
    opener = _FakeOpener([201])
    uploader = _uploader(opener)
    assert uploader.enqueue(_profile(b"gzip-bytes"))
    _wait_for(lambda: len(opener.requests) == 1)
    assert uploader.close(timeout=2.0) is None

    request = opener.requests[0]
    assert request.full_url == ENDPOINT + "/api/profiles"
    assert request.get_method() == "POST"
    assert request.get_header("Authorization") == "Bearer " + TOKEN
    content_type = request.get_header("Content-type")
    fields, files = _parse_multipart(request.data, content_type)
    assert fields == {
        "service": SERVICE,
        "upload_id": "a" * 32,
        "type": "cpu",
        "start": "2026-11-18T12:00:00.000000Z",
        "end": "2026-11-18T12:01:00.000000Z",
    }
    assert files == {"profile": ("application/octet-stream", b"gzip-bytes")}
    assert opener.timeouts == [15.0]
    assert uploader.dropped() == 0


def test_upload_id_is_unique_and_hex32():
    ids = [new_upload_id() for _ in range(1000)]
    assert all(len(value) == 32 for value in ids)
    assert all(all(char in "0123456789abcdef" for char in value) for value in ids)
    assert len(set(ids)) == len(ids)


def test_404_is_silent_and_counted_as_rejected():
    opener = _FakeOpener([404, 201])
    drops = []
    uploader = _uploader(
        opener,
        max_attempts=3,
        warn=lambda reason, err, count: drops.append((reason, count)),
    )
    assert uploader.enqueue(_profile())
    _wait_for(lambda: len(opener.requests) == 1)
    assert uploader.close(timeout=2.0) is None
    assert len(opener.requests) == 1
    assert uploader.dropped() == 1
    assert drops == [("rejected", 1)]


def test_409_conflict_is_silent_and_counted_as_rejected():
    opener = _FakeOpener([409, 201])
    drops = []
    uploader = _uploader(
        opener,
        max_attempts=3,
        warn=lambda reason, err, count: drops.append((reason, count)),
    )
    assert uploader.enqueue(_profile())
    _wait_for(lambda: len(opener.requests) == 1)
    assert uploader.close(timeout=2.0) is None
    assert len(opener.requests) == 1
    assert uploader.dropped() == 1
    assert drops == [("rejected", 1)]


def test_5xx_is_retried_until_accepted():
    opener = _FakeOpener([503, 500, 201])
    uploader = _uploader(opener, max_attempts=3)
    assert uploader.enqueue(_profile())
    _wait_for(lambda: len(opener.requests) == 3)
    assert uploader.close(timeout=2.0) is None
    assert len(opener.requests) == 3
    assert uploader.dropped() == 0


def test_network_errors_exhaust_the_retry_budget_and_drop():
    reason = urllib.error.URLError("connection refused")
    opener = _FakeOpener([reason, urllib.error.URLError("connection refused")])
    drops = []
    uploader = _uploader(
        opener,
        max_attempts=2,
        warn=lambda kind, err, count: drops.append((kind, count)),
    )
    assert uploader.enqueue(_profile())
    _wait_for(lambda: len(opener.requests) == 2)
    uploader.close(timeout=2.0)
    assert len(opener.requests) == 2
    assert uploader.dropped() == 1
    assert drops == [("retry_exhausted", 1)]


def test_queue_is_bounded_and_reports_drops():
    opener = _BlockingOpener()
    uploader = _uploader(opener, queue_capacity=1)
    try:
        results = [uploader.enqueue(_profile()) for _ in range(20)]
        assert False in results
        assert uploader.dropped() > 0
    finally:
        opener.release.set()
        uploader.close(timeout=2.0)
