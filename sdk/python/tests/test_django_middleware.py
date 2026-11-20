from types import SimpleNamespace

import django.http
import pytest
from django.core.exceptions import DisallowedHost
from django.core.signals import got_request_exception
from django.http import JsonResponse
from django.test import Client as DjangoTestClient
from django.test import RequestFactory, override_settings
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader

import lazyobserve
from lazyobserve.django import DjangoMiddleware
from tests.helpers import last_span, make_client


def _mw(client):
    factory = client.django_middleware()

    def get_response(request):
        return JsonResponse({"ok": True})

    return factory(get_response)


def test_unmatched_request_uses_low_cardinality_route_and_metrics(client):
    reader = InMemoryMetricReader()
    client.meter_provider = MeterProvider(metric_readers=[reader])
    mw = _mw(client)
    for path in ("/scan/random-1", "/scan/random-2", "/orders/42"):
        request = RequestFactory().get(path, HTTP_HOST="testserver")
        response = mw(request)
        assert response.status_code == 200

    span = last_span(client)
    assert span is not None
    assert span.name == "GET unmatched"
    assert span.kind.name == "SERVER"
    attrs = dict(span.attributes)
    assert attrs["http.request.method"] == "GET"
    assert attrs["http.route"] == "unmatched"
    assert attrs["url.path"] == "/orders/42"
    assert attrs["http.response.status_code"] == 200
    assert attrs["server.address"] == "testserver"

    data = reader.get_metrics_data()
    routes = set()
    for rm in data.resource_metrics:
        for sm in rm.scope_metrics:
            for metric in sm.metrics:
                if metric.name == "http.server.request.duration":
                    for point in metric.data.data_points:
                        routes.add(point.attributes.get("http.route"))
    assert routes == {"unmatched"}


def test_matched_route_updates_span_name_and_route_attribute(client):
    factory = client.django_middleware()

    def get_response(request):
        request.resolver_match = SimpleNamespace(route="orders/<int:order_id>")
        return JsonResponse({"ok": True})

    mw = factory(get_response)
    request = RequestFactory().get("/orders/42", HTTP_HOST="testserver")
    response = mw(request)
    assert response.status_code == 200
    span = last_span(client)
    assert span is not None
    assert span.name == "GET orders/<int:order_id>"
    assert dict(span.attributes)["http.route"] == "orders/<int:order_id>"


def test_unhandled_exception_becomes_500_and_preserves_real_exception_type(client, monkeypatch):
    captured = []
    monkeypatch.setattr(client, "capture_exception", lambda *a, **k: captured.append((a, k)))

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
    assert len(captured) == 1
    args, kwargs = captured[0]
    assert isinstance(args[0], ValueError)
    assert "exception_type" not in kwargs


def test_re_raise_on_500_flag_does_not_double_report(client, monkeypatch):
    client.config.re_raise_on_500 = True
    captured = []
    monkeypatch.setattr(client, "capture_exception", lambda *a, **k: captured.append((a, k)))

    def get_response(request):
        exc = ValueError("boom")
        mw.process_exception(request, exc)
        raise exc

    factory = client.django_middleware()
    mw = factory(get_response)
    request = RequestFactory().get("/boom", HTTP_HOST="testserver")
    with pytest.raises(ValueError):
        mw(request)
    assert len(captured) == 1


def test_process_exception_preserves_django_native_500_pipeline(client, monkeypatch):
    captured = []
    monkeypatch.setattr(client, "capture_exception", lambda *a, **k: captured.append((a, k)))
    lazyobserve._set_active(client)

    signal_hits = []

    def on_exception(sender, request=None, **kwargs):
        signal_hits.append(request.path if request else None)

    got_request_exception.connect(on_exception)
    try:
        with override_settings(
            DEBUG=False,
            MIDDLEWARE=["lazyobserve.DjangoMiddleware"],
        ):
            django_client = DjangoTestClient(raise_request_exception=False)
            response = django_client.get("/boom")
    finally:
        got_request_exception.disconnect(on_exception)
        lazyobserve._set_active(None)

    assert response.status_code == 500
    assert b"Server Error (500)" in response.content
    assert signal_hits == ["/boom"]
    assert len(captured) == 1
    args, kwargs = captured[0]
    assert isinstance(args[0], ValueError)
    assert kwargs["tags"]["http.route"] == "boom"


def test_disallowed_host_does_not_crash_middleware(client):
    mw = _mw(client)
    request = RequestFactory().get("/ok", HTTP_HOST="evil.example")

    def bad_host():
        raise DisallowedHost("Invalid HTTP_HOST header: 'evil.example'")

    request.get_host = bad_host
    response = mw(request)
    assert response.status_code == 200
    span = last_span(client)
    assert span is not None
    assert dict(span.attributes)["server.address"] == "evil.example"


def test_request_scheme_ignores_untrusted_x_forwarded_proto(client):
    mw = _mw(client)
    request = RequestFactory().get(
        "/ok",
        HTTP_HOST="testserver",
        HTTP_X_FORWARDED_PROTO="https",
    )
    mw(request)
    span = last_span(client)
    assert dict(span.attributes)["url.scheme"] == "http"

    secure_req = RequestFactory().get("/ok", HTTP_HOST="testserver", secure=True)
    mw(secure_req)
    span_secure = last_span(client)
    assert dict(span_secure.attributes)["url.scheme"] == "https"


def test_delayed_init_is_picked_up_by_django_middleware(client):
    lazyobserve._set_active(None)
    mw = DjangoMiddleware(lambda request: JsonResponse({"ok": True}))
    try:
        assert mw(RequestFactory().get("/before")).status_code == 200
        assert last_span(client) is None

        lazyobserve._set_active(client)
        assert mw(RequestFactory().get("/after", HTTP_HOST="testserver")).status_code == 200
        span = last_span(client)
        assert span is not None
        assert dict(span.attributes)["url.path"] == "/after"
    finally:
        lazyobserve._set_active(None)


def test_instrumentation_init_failure_degrades_silently(client):
    class _BrokenMeter:
        def create_histogram(self, *args, **kwargs):
            raise RuntimeError("otel meter broken")

    class _BrokenMeterProvider:
        def get_meter(self, _name):
            return _BrokenMeter()

    client.meter_provider = _BrokenMeterProvider()
    mw = _mw(client)
    response = mw(RequestFactory().get("/ok", HTTP_HOST="testserver"))
    assert response.status_code == 200


def test_histogram_falls_back_when_advisory_kwarg_unsupported(client):
    recorded = []

    class _OldHistogram:
        def record(self, value, attrs):
            recorded.append((value, dict(attrs)))

    class _OldUpDown:
        def add(self, value, attrs):
            pass

    class _OldMeter:
        def create_histogram(self, name, *, unit="", description=""):
            return _OldHistogram()

        def create_up_down_counter(self, name, *, unit="", description=""):
            return _OldUpDown()

    class _OldMeterProvider:
        def get_meter(self, _name):
            return _OldMeter()

    client.meter_provider = _OldMeterProvider()
    mw = _mw(client)
    response = mw(RequestFactory().get("/ok", HTTP_HOST="testserver"))
    assert response.status_code == 200
    assert len(recorded) == 1
    assert recorded[0][1]["http.route"] == "unmatched"


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
    lazyobserve._set_active(None)

    def get_response(request):
        return JsonResponse({"ok": True})

    mw = DjangoMiddleware(get_response)
    response = mw(RequestFactory().get("/ok"))
    assert response.status_code == 200


def test_middleware_declares_sync_and_async_capable(client):
    assert DjangoMiddleware.sync_capable is True
    assert DjangoMiddleware.async_capable is True
    factory = client.django_middleware()
    assert factory.sync_capable is True
    assert factory.async_capable is True


def test_async_middleware_runs_natively_without_thread_hopping(client):
    import asyncio
    import threading
    from asgiref.sync import iscoroutinefunction

    caller_thread = threading.get_ident()
    view_threads = []

    async def async_get_response(request):
        view_threads.append(threading.get_ident())
        request.resolver_match = SimpleNamespace(route="async/items/<int:id>")
        return JsonResponse({"async": True})

    mw = client.django_middleware()(async_get_response)
    assert iscoroutinefunction(mw)

    request = RequestFactory().get("/async/items/7", HTTP_HOST="testserver")
    response = asyncio.run(mw(request))
    assert response.status_code == 200
    assert view_threads == [caller_thread]

    span = last_span(client)
    assert span is not None
    assert span.name == "GET async/items/<int:id>"
    assert dict(span.attributes)["http.route"] == "async/items/<int:id>"
    assert dict(span.attributes)["http.response.status_code"] == 200


def test_django_async_client_integrates_with_asgi_middleware(client, monkeypatch):
    import asyncio
    from django.test import AsyncClient

    captured = []
    monkeypatch.setattr(client, "capture_exception", lambda *a, **k: captured.append((a, k)))
    lazyobserve._set_active(client)

    async def run():
        async_client = AsyncClient(raise_request_exception=False)
        resp_ok = await async_client.get("/async-ok")
        assert resp_ok.status_code == 200
        resp_err = await async_client.get("/async-boom")
        assert resp_err.status_code == 500

    try:
        with override_settings(DEBUG=False, MIDDLEWARE=["lazyobserve.DjangoMiddleware"]):
            asyncio.run(run())
    finally:
        lazyobserve._set_active(None)

    spans = client._test_span_exporter.get_finished_spans()
    names = [s.name for s in spans]
    assert "GET async-ok" in names
    assert "GET async-boom" in names
    assert len(captured) == 1
    assert isinstance(captured[0][0][0], ValueError)


def test_sync_streaming_response_tracks_full_stream_duration_and_detaches_context(client):
    import time
    from django.http import StreamingHttpResponse
    from opentelemetry import trace as otel_trace

    reader = InMemoryMetricReader()
    client.meter_provider = MeterProvider(metric_readers=[reader])

    def chunks():
        yield b"chunk-1-"
        time.sleep(0.05)
        yield b"chunk-2"

    def get_response(request):
        request.resolver_match = SimpleNamespace(route="stream/sync")
        return StreamingHttpResponse(chunks(), status=200)

    mw = client.django_middleware()(get_response)
    request = RequestFactory().get("/stream/sync", HTTP_HOST="testserver")
    response = mw(request)

    # Context must be detached immediately upon returning StreamingHttpResponse.
    assert not otel_trace.get_current_span().get_span_context().is_valid
    # Span has not finished yet because stream has not been consumed.
    assert len(client._test_span_exporter.get_finished_spans()) == 0

    body = b"".join(response.streaming_content)
    assert body == b"chunk-1-chunk-2"

    span = last_span(client)
    assert span is not None
    assert span.name == "GET stream/sync"
    assert (span.end_time - span.start_time) / 1e9 >= 0.04

    data = reader.get_metrics_data()
    active_values = []
    duration_sums = []
    for rm in data.resource_metrics:
        for sm in rm.scope_metrics:
            for metric in sm.metrics:
                if metric.name == "http.server.active_requests":
                    active_values.extend(p.value for p in metric.data.data_points)
                if metric.name == "http.server.request.duration":
                    duration_sums.extend(p.sum for p in metric.data.data_points)
    assert active_values == [0]
    assert duration_sums and duration_sums[0] >= 0.04


def test_async_streaming_response_tracks_full_stream_and_close_finalizes(client):
    import asyncio
    from django.http import StreamingHttpResponse
    from opentelemetry import trace as otel_trace

    async def run():
        async def achunks():
            yield b"a-"
            await asyncio.sleep(0.05)
            yield b"b"

        async def async_get_response(request):
            request.resolver_match = SimpleNamespace(route="stream/async")
            return StreamingHttpResponse(achunks(), status=200)

        mw = client.django_middleware()(async_get_response)
        request = RequestFactory().get("/stream/async", HTTP_HOST="testserver")
        response = await mw(request)
        assert not otel_trace.get_current_span().get_span_context().is_valid
        assert len(client._test_span_exporter.get_finished_spans()) == 0

        parts = [chunk async for chunk in response.streaming_content]
        assert b"".join(parts) == b"a-b"

        span = last_span(client)
        assert span is not None
        assert span.name == "GET stream/async"
        assert (span.end_time - span.start_time) / 1e9 >= 0.04

        # Unconsumed stream still finalizes cleanly when closed.
        request2 = RequestFactory().get("/stream/async-close", HTTP_HOST="testserver")
        response2 = await mw(request2)
        before_count = len(client._test_span_exporter.get_finished_spans())
        response2.close()
        assert len(client._test_span_exporter.get_finished_spans()) == before_count + 1

    asyncio.run(run())


def test_request_setup_failure_passes_through_without_500_sync_and_async(client):
    import asyncio

    class _BrokenTracer:
        def start_span(self, *args, **kwargs):
            raise RuntimeError("tracer exploded")

    class _BrokenTracerProvider:
        def get_tracer(self, _name):
            return _BrokenTracer()

    client.tracer_provider = _BrokenTracerProvider()

    # Sync WSGI path
    mw_sync = _mw(client)
    resp_sync = mw_sync(RequestFactory().get("/ok", HTTP_HOST="testserver"))
    assert resp_sync.status_code == 200

    # Async ASGI path
    async def async_get_response(request):
        return JsonResponse({"ok": True})

    mw_async = client.django_middleware()(async_get_response)
    resp_async = asyncio.run(mw_async(RequestFactory().get("/ok", HTTP_HOST="testserver")))
    assert resp_async.status_code == 200
