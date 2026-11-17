import pytest
from rest_framework.exceptions import APIException, AuthenticationFailed, ValidationError
from rest_framework.test import APIRequestFactory


def _make_req(method="GET", path="/api/server"):
    factory = APIRequestFactory()
    request = factory.generic(method, path, HTTP_HOST="testserver")
    return request


class _FiveHundred(APIException):
    status_code = 500
    default_detail = "server exploded"


def test_api_validation_error_no_capture(client, monkeypatch):
    captured = []
    monkeypatch.setattr(client, "capture_exception", lambda *a, **k: captured.append((a, k)))
    handler = client.drf_exception_handler()
    response = handler(ValidationError("bad"), {"request": _make_req(path="/api/boom")})
    assert response.status_code == 400
    assert captured == []


def test_api_5xx_captured(client, monkeypatch):
    captured = []
    monkeypatch.setattr(client, "capture_exception", lambda *a, **k: captured.append((a, k)))
    handler = client.drf_exception_handler()
    response = handler(_FiveHundred("kaput"), {"request": _make_req()})
    assert response.status_code == 500
    assert len(captured) == 1
    args, kwargs = captured[0]
    assert type(args[0]) is _FiveHundred
    assert kwargs["tags"]["http.response.status_code"] == "500"
    assert kwargs["tags"]["http.route"] == "/api/server"
    assert kwargs["url"] == "http://testserver/api/server"


def test_non_api_exception_reraises(client):
    handler = client.drf_exception_handler()
    with pytest.raises(RuntimeError):
        handler(RuntimeError("kaput"), {"request": _make_req()})


def test_authentication_exception_not_5xx(client, monkeypatch):
    captured = []
    monkeypatch.setattr(client, "capture_exception", lambda *a, **k: captured.append((a, k)))
    handler = client.drf_exception_handler()
    response = handler(AuthenticationFailed("nope"), {"request": _make_req()})
    assert response.status_code == 401
    assert captured == []
