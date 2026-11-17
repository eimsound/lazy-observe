"""Django middleware. Parity with the Go SDK's ``lo.Fiber()``: extracts W3C
traceparent, creates a SERVER span, records request duration / active-request
metrics, recovers unhandled exceptions as HTTP 500 with a correlated error log,
and queues 5xx-originating exceptions as error events.

In Django, unhandled view exceptions reach middleware through the official
``process_exception`` hook (the plain ``get_response`` call never sees them —
Django's ``convert_exception_to_response`` wraps ``_get_response`` at the
innermost link of the middleware chain). We implement that hook plus a
defensive ``try/except`` for direct (non-Django) invocation.

Usage (registry-bound, keeps Django's string-based MIDDLEWARE config):

    MIDDLEWARE = ["lazyobserve.DjangoMiddleware", ...]

or bind explicitly on an instance:

    MIDDLEWARE = [lo.django_middleware(), ...]
"""

from __future__ import annotations

import logging
import re
import threading
import time as _time
from typing import Any, Optional

from opentelemetry import context as _otel_context
from opentelemetry import trace as _otel_trace

logger = logging.getLogger("lazyobserve")

_HTTP_DURATION_BUCKETS = [
    0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75,
    1.0, 2.5, 5.0, 7.5, 10.0,
]

_PROTOCOL_RE = re.compile(r"HTTP/([12]\.\d)$")

_SPAN_ATTR = "_lazyobserve_span"
_START_ATTR = "_lazyobserve_start"


def _request_scheme(request: Any) -> str:
    if getattr(request, "is_secure", lambda: False)():
        return "https"
    forwarded = request.META.get("HTTP_X_FORWARDED_PROTO", "")
    if forwarded:
        return forwarded.split(",")[0].strip()
    return "http"


def _protocol_version(request: Any) -> str:
    value = request.META.get("SERVER_PROTOCOL", "")
    match = _PROTOCOL_RE.match(value)
    return match.group(1) if match else ""


def _route_for(request: Any) -> str:
    resolve = getattr(request, "resolver_match", None)
    if resolve is not None and getattr(resolve, "route", None):
        return resolve.route
    return request.path


def _query_free_url(request: Any) -> str:
    host = request.get_host()
    path = request.path
    return f"{_request_scheme(request)}://{host}{path}"


def _error_tags(method: str, route: str, status: int) -> dict:
    return {
        "http.request.method": method or "",
        "http.route": route or "",
        "http.response.status_code": str(status),
    }


class _Instrumentation:
    """Cached instruments for HTTP server metrics."""

    def __init__(self, client: Any) -> None:
        meter = client.meter_provider.get_meter("io.lazyobserve.python")
        self.duration = meter.create_histogram(
            "http.server.request.duration",
            unit="s",
            description="Duration of HTTP server requests.",
            explicit_bucket_boundaries_advisory=_HTTP_DURATION_BUCKETS,
        )
        self.active = meter.create_up_down_counter(
            "http.server.active_requests",
            unit="{request}",
            description="Number of active HTTP server requests.",
        )
        self.tracer = client.tracer_provider.get_tracer("io.lazyobserve.python")


class _BaseMiddleware:
    """Shared behavior for registry-bound and explicit-bound middleware."""

    def __call__(self, request: Any):
        client = self._client
        if client is None or client.degraded:
            return self.get_response(request)
        return _instrument_request(self, client, request)

    def process_exception(self, request: Any, exception: BaseException):
        """Django's official exception hook (called via
        ``process_exception_by_middleware`` before the 500 handler)."""
        client = self._client
        if client is None or client.degraded:
            return None
        return _record_exception(self, client, request, exception)

    def _client(self) -> Any:  # placeholders overridden below
        raise NotImplementedError

    def _client_for(self) -> Any:  # placeholders overridden below
        raise NotImplementedError


class DjangoMiddleware(_BaseMiddleware):
    """Django middleware bound to the active lazyobserve client."""

    def __init__(self, get_response: Any) -> None:
        self.get_response = get_response
        self._client = None
        self._instrumentation = None
        self._lock = threading.Lock()
        try:
            from . import _registry

            self._client = _registry.get_active_client()
        except Exception:  # noqa: BLE001
            self._client = None

    def _client_for(self) -> Any:
        if self._instrumentation is None and self._client is not None and not self._client.degraded:
            with self._lock:
                if self._instrumentation is None:
                    self._instrumentation = _Instrumentation(self._client)
        return self._instrumentation


class _BoundMiddleware(_BaseMiddleware):
    """Middleware bound to an explicit client (``lo.django_middleware()``)."""

    def __init__(self, get_response: Any, client: Any) -> None:
        self.get_response = get_response
        self._client = client
        self._instrumentation = None
        self._lock = threading.Lock()

    def _client_for(self) -> Any:
        if self._instrumentation is None and self._client is not None and not self._client.degraded:
            with self._lock:
                if self._instrumentation is None:
                    self._instrumentation = _Instrumentation(self._client)
        return self._instrumentation


def build_middleware(client: Any):
    def factory(get_response: Any = None):
        if get_response is None:
            return factory
        return _BoundMiddleware(get_response, client)

    return factory


def _build_server_span(instrumentation: _Instrumentation, propagator: Any, request: Any, method: str, path: str, span_attrs: dict):
    carrier = {k.lower(): v for k, v in request.headers.items()}
    parent = propagator.extract(carrier)
    span = instrumentation.tracer.start_span(
        f"{method} {path}",
        context=parent,
        kind=_otel_trace.SpanKind.SERVER,
        attributes=span_attrs,
    )
    return _otel_trace.set_span_in_context(span), span


def _instrument_request(instance: _BaseMiddleware, client: Any, request: Any) -> Any:
    instrumentation = instance._client_for()
    if instrumentation is None:
        return instance.get_response(request)
    start = _time.monotonic()
    method = request.method
    path = request.path
    propagator = client.propagator

    span_attrs = {
        "http.request.method": method or "",
        "url.scheme": _request_scheme(request),
        "url.path": path or "",
        "server.address": request.get_host(),
    }
    protocol = _protocol_version(request)
    if protocol:
        span_attrs["network.protocol.version"] = protocol

    request_context, span = _build_server_span(instrumentation, propagator, request, method, path, span_attrs)
    token = _otel_context.attach(request_context)
    setattr(request, _SPAN_ATTR, span)
    setattr(request, _START_ATTR, start)
    instrumentation.active.add(1, {"http.request.method": method or ""})

    status = 500
    try:
        response = instance.get_response(request)
        status = response.status_code
        return response
    except Exception as exc:  # noqa: BLE001 - defensive path for non-Django callers
        return _record_exception(instance, client, request, exc)
    finally:
        span.set_attribute("http.route", _route_for(request))
        span.set_attribute("http.response.status_code", status)
        if status >= 500:
            span.set_status(_otel_trace.Status(_otel_trace.StatusCode.ERROR))
        instrumentation.duration.record(
            _time.monotonic() - start,
            {
                "http.request.method": method or "",
                "http.route": _route_for(request),
                "http.response.status_code": status,
            },
        )
        instrumentation.active.add(-1, {"http.request.method": method or ""})
        span.end()
        _otel_context.detach(token)


def _record_exception(instance: _BaseMiddleware, client: Any, request: Any, exc: BaseException) -> Any:
    """Mark the request span as errored and queue an error event. Returns the
    500 response, or re-raises when ``re_raise_on_500`` is enabled."""
    method = request.method or ""
    path = request.path or ""
    span = getattr(request, _SPAN_ATTR, None)
    if span is not None:
        span.record_exception(exc)
        span.set_status(_otel_trace.Status(_otel_trace.StatusCode.ERROR, str(exc)))
    client.logger.error(
        "unhandled exception",
        exc_info=exc,
        extra={
            "http.request.method": method,
            "url.path": path,
        },
    )
    route = _route_for(request)
    client.capture_exception(
        exc,
        tags=_error_tags(method, route, 500),
        url=_query_free_url(request),
        exception_type="panic",
    )
    if getattr(client.config, "re_raise_on_500", False):
        raise exc
    from django.http import HttpResponseServerError

    return HttpResponseServerError("Internal Server Error")
