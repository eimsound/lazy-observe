import django.http
import pytest
from django.http import JsonResponse
from django.test import RequestFactory

from tests.helpers import last_span, make_client


def _mw(client):
    factory = client.django_middleware()

    def get_response(request):
        return JsonResponse({"ok": True})

    return factory(get_response)


def test_normal_request_records_span_and_metrics(client):
    mw = _mw(client)
    request = RequestFactory().get("/orders/42", HTTP_HOST="testserver")
    response = mw(request)
    assert response.status_code == 200
    span = last_span(client)
    assert span is not None
    assert span.name == "GET /orders/42"
    assert span.kind.name == "SERVER"
    attrs = dict(span.attributes)
    assert attrs["http.request.method"] == "GET"
    assert attrs["http.route"] == "/orders/42"
    assert attrs["http.response.status_code"] == 200
    assert attrs["server.address"] == "testserver"


def test_unhandled_exception_becomes_500(client):
    def get_response(request):
        raise ValueError("boom")

    factory = client.django_middleware()
    mw = factory(get_response)
    request = RequestFactory().get("/boom", HTTP_HOST="testserver")
    response = mw(request)
    assert response.status_code == 500
    span = last_span(client)
    assert span is not None
    assert span.status.status_code.name == "ERROR"
    assert dict(span.attributes)["http.response.status_code"] == 500


def test_re_raise_on_500_flag(client):
    client.config.re_raise_on_500 = True

    def get_response(request):
        raise ValueError("boom")

    factory = client.django_middleware()
    mw = factory(get_response)
    request = RequestFactory().get("/boom", HTTP_HOST="testserver")
    with pytest.raises(ValueError):
        mw(request)


def test_traceparent_propagation(client):
    """A parent trace id in a traceparent header becomes the span's parent."""
    mw = _mw(client)
    request = RequestFactory().get(
        "/ok",
        HTTP_HOST="testserver",
        HTTP_TRACEPARENT="00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
    )
    mw(request)
    span = last_span(client)
    assert format(span.parent.span_id, "016x") == "00f067aa0ba902b7"


def test_transparent_when_no_client():
    # Build client directly, then reset registry to None
    import lazyobserve

    lazyobserve._set_active(None)
    from lazyobserve.django import DjangoMiddleware

    def get_response(request):
        return JsonResponse({"ok": True})

    mw = DjangoMiddleware(get_response)
    response = mw(RequestFactory().get("/ok"))
    assert response.status_code == 200
