from types import SimpleNamespace

import pytest
from django.core.exceptions import DisallowedHost
from rest_framework.exceptions import APIException, AuthenticationFailed, ValidationError
from rest_framework.test import APIRequestFactory
from rest_framework.views import APIView


def _make_req(method="GET", path="/api/server", route=None, **extra):
    factory = APIRequestFactory()
    request = factory.generic(method, path, HTTP_HOST="testserver", **extra)
    if route is not None:
        request.resolver_match = SimpleNamespace(route=route)
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


def test_api_5xx_captured_with_matched_route(client, monkeypatch):
    captured = []
    monkeypatch.setattr(client, "capture_exception", lambda *a, **k: captured.append((a, k)))
    handler = client.drf_exception_handler()
    req = _make_req(path="/api/orders/42", route="api/orders/<int:pk>")
    response = handler(_FiveHundred("kaput"), {"request": req})
    assert response.status_code == 500
    assert len(captured) == 1
    args, kwargs = captured[0]
    assert type(args[0]) is _FiveHundred
    assert kwargs["tags"]["http.response.status_code"] == "500"
    assert kwargs["tags"]["http.route"] == "api/orders/<int:pk>"
    assert kwargs["url"] == "http://testserver/api/orders/42"


def test_api_5xx_unmatched_route_uses_low_cardinality_constant(client, monkeypatch):
    captured = []
    monkeypatch.setattr(client, "capture_exception", lambda *a, **k: captured.append((a, k)))
    handler = client.drf_exception_handler()
    response = handler(_FiveHundred("kaput"), {"request": _make_req(path="/random/12345")})
    assert response.status_code == 500
    assert len(captured) == 1
    _, kwargs = captured[0]
    assert kwargs["tags"]["http.route"] == "unmatched"


def test_non_api_exception_returns_none_per_drf_contract(client):
    handler = client.drf_exception_handler()
    assert handler(RuntimeError("kaput"), {"request": _make_req()}) is None


def test_drf_apiview_uncaught_exception_uses_raise_uncaught_exception(client, monkeypatch):
    import lazyobserve

    lazyobserve._set_active(client)
    raised = []

    class _ExplodingView(APIView):
        authentication_classes = []
        permission_classes = []

        def raise_uncaught_exception(self, exc):
            raised.append(exc)
            raise exc

    try:
        view = _ExplodingView()
        view.request = _make_req(path="/api/exploding")
        view.args = ()
        view.kwargs = {}
        view.format_kwarg = None
        with pytest.raises(RuntimeError, match="non-api boom"):
            view.handle_exception(RuntimeError("non-api boom"))
        assert len(raised) == 1
    finally:
        lazyobserve._set_active(None)


def test_drf_disallowed_host_does_not_crash_5xx_capture(client, monkeypatch):
    captured = []
    monkeypatch.setattr(client, "capture_exception", lambda *a, **k: captured.append((a, k)))
    req = _make_req(path="/api/server", route="api/server")

    def bad_host():
        raise DisallowedHost("evil.host")

    req.get_host = bad_host
    handler = client.drf_exception_handler()
    response = handler(_FiveHundred("kaput"), {"request": req})
    assert response.status_code == 500
    assert len(captured) == 1
    _, kwargs = captured[0]
    assert kwargs["url"] == "http://testserver/api/server"


def test_authentication_exception_not_5xx(client, monkeypatch):
    captured = []
    monkeypatch.setattr(client, "capture_exception", lambda *a, **k: captured.append((a, k)))
    handler = client.drf_exception_handler()
    response = handler(AuthenticationFailed("nope"), {"request": _make_req()})
    assert response.status_code == 401
    assert captured == []
