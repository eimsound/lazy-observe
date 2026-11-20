"""Django middleware (WSGI and ASGI). Parity with the Go SDK's ``lo.Fiber()``:
extracts W3C traceparent, creates a SERVER span, records request duration and
active-request metrics, observes unhandled exceptions via ``process_exception``
while preserving Django's native 500 pipeline, and queues 5xx-originating
exceptions as error events.

WSGI / ASGI and streaming support boundaries:
- Declares ``sync_capable = True`` and ``async_capable = True``. When Django
  passes an async ``get_response`` under ASGI, the middleware marks itself as a
  coroutine function and runs natively on the event loop without ``sync_to_async``
  thread hopping.
- Wraps both synchronous and asynchronous ``StreamingHttpResponse`` content
  iterators (and hooks ``_resource_closers``) so span duration,
  ``http.server.request.duration``, and ``http.server.active_requests`` reflect
  the full stream lifetime rather than stopping when headers are returned. The
  OpenTelemetry request context is still detached on middleware return so it
  never leaks across requests or outer middleware.
- In Django, unhandled view exceptions reach middleware through the official
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

import inspect
import logging
import re
import threading
import time as _time
from typing import Any, Callable, Optional, Tuple

from opentelemetry import context as _otel_context
from opentelemetry import trace as _otel_trace

from .client import _create_histogram_compat

logger = logging.getLogger("lazyobserve")
_django_logger = logging.getLogger("lazyobserve.django")

_HTTP_DURATION_BUCKETS = [
    0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75,
    1.0, 2.5, 5.0, 7.5, 10.0,
]

_PROTOCOL_RE = re.compile(r"HTTP/([12]\.\d)$")

_SPAN_ATTR = "_lazyobserve_span"
_START_ATTR = "_lazyobserve_start"
_RECORDED_EXCS_ATTR = "_lazyobserve_recorded_exceptions"
_UNMATCHED_ROUTE = "unmatched"


def _is_coroutine_function(fn: Any) -> bool:
    try:
        from asgiref.sync import iscoroutinefunction

        return bool(iscoroutinefunction(fn))
    except Exception:  # noqa: BLE001
        return inspect.iscoroutinefunction(fn)


def _mark_coroutine_function(obj: Any) -> None:
    try:
        from asgiref.sync import markcoroutinefunction

        markcoroutinefunction(obj)
    except Exception:  # noqa: BLE001
        pass


def _request_scheme(request: Any) -> str:
    is_secure = getattr(request, "is_secure", None)
    if callable(is_secure):
        try:
            return "https" if is_secure() else "http"
        except Exception:  # noqa: BLE001
            return "http"
    return "http"


def _safe_host(request: Any) -> str:
    get_host = getattr(request, "get_host", None)
    if callable(get_host):
        try:
            return str(get_host() or "")
        except Exception:  # noqa: BLE001 - e.g. django.core.exceptions.DisallowedHost
            pass
    meta = getattr(request, "META", None)
    if meta is not None and hasattr(meta, "get"):
        try:
            return str(meta.get("HTTP_HOST", "") or "")
        except Exception:  # noqa: BLE001
            pass
    return ""


def _protocol_version(request: Any) -> str:
    meta = getattr(request, "META", None)
    value = meta.get("SERVER_PROTOCOL", "") if meta is not None and hasattr(meta, "get") else ""
    match = _PROTOCOL_RE.match(str(value or ""))
    return match.group(1) if match else ""


def _route_for(request: Any) -> str:
    resolve = getattr(request, "resolver_match", None)
    if resolve is not None:
        route = getattr(resolve, "route", None)
        if route:
            return str(route)
        if route == "":
            return "/"
    return _UNMATCHED_ROUTE


def _query_free_url(request: Any) -> str:
    host = _safe_host(request)
    path = getattr(request, "path", "") or ""
    if not host:
        return path
    return f"{_request_scheme(request)}://{host}{path}"


def _error_tags(method: str, route: str, status: int) -> dict:
    return {
        "http.request.method": method or "",
        "http.route": route or _UNMATCHED_ROUTE,
        "http.response.status_code": str(status),
    }


class _Instrumentation:
    """Cached instruments for HTTP server metrics."""

    def __init__(self, client: Any) -> None:
        meter = client.meter_provider.get_meter("io.lazyobserve.python")
        self.duration = _create_histogram_compat(
            meter,
            "http.server.request.duration",
            unit="s",
            description="Duration of HTTP server requests.",
            buckets=_HTTP_DURATION_BUCKETS,
        )
        self.active = meter.create_up_down_counter(
            "http.server.active_requests",
            unit="{request}",
            description="Number of active HTTP server requests.",
        )
        self.tracer = client.tracer_provider.get_tracer("io.lazyobserve.python")


class _BaseMiddleware:
    """Shared behavior for registry-bound and explicit-bound middleware."""

    sync_capable = True
    async_capable = True

    def _init_async_mode(self, get_response: Any) -> None:
        self.get_response = get_response
        self._is_async = _is_coroutine_function(get_response)
        if self._is_async:
            _mark_coroutine_function(self)

    def __call__(self, request: Any):
        if self._is_async:
            return self._acall(request)
        client = self._resolve_client()
        if client is None or client.degraded:
            return self.get_response(request)
        return _instrument_request(self, client, request)

    async def _acall(self, request: Any):
        client = self._resolve_client()
        if client is None or client.degraded:
            return await self.get_response(request)
        return await _instrument_request_async(self, client, request)

    def process_exception(self, request: Any, exception: BaseException):
        """Django's official exception hook (called via
        ``process_exception_by_middleware`` before the 500 handler).

        Observes the exception and returns ``None`` so Django's native 500
        pipeline (subsequent middleware, ``got_request_exception``, DEBUG error
        page, ``handler500``) remains intact.
        """
        client = self._resolve_client()
        if client is None or client.degraded:
            return None
        _observe_exception(client, request, exception)
        return None

    def _resolve_client(self) -> Any:
        raise NotImplementedError

    def _client_for(self, client: Any) -> Any:
        if client is None or client.degraded:
            return None
        if self._instrumentation is not None and self._instrumentation_client is client:
            return self._instrumentation
        with self._lock:
            if self._instrumentation is not None and self._instrumentation_client is client:
                return self._instrumentation
            try:
                self._instrumentation = _Instrumentation(client)
                self._instrumentation_client = client
            except Exception as exc:  # noqa: BLE001 - silent degradation on SDK init failure
                logger.warning("lazyobserve Django middleware instrumentation degraded: %s", exc)
                return None
        return self._instrumentation


class DjangoMiddleware(_BaseMiddleware):
    """Django middleware bound to the active lazyobserve client."""

    def __init__(self, get_response: Any) -> None:
        self._instrumentation: Optional[_Instrumentation] = None
        self._instrumentation_client: Any = None
        self._lock = threading.Lock()
        self._init_async_mode(get_response)

    def _resolve_client(self) -> Any:
        try:
            from . import _registry

            return _registry.get_active_client()
        except Exception:  # noqa: BLE001
            return None


class _BoundMiddleware(_BaseMiddleware):
    """Middleware bound to an explicit client (``lo.django_middleware()``)."""

    def __init__(self, get_response: Any, client: Any) -> None:
        self._client = client
        self._instrumentation: Optional[_Instrumentation] = None
        self._instrumentation_client: Any = None
        self._lock = threading.Lock()
        self._init_async_mode(get_response)

    def _resolve_client(self) -> Any:
        return self._client


def build_middleware(client: Any):
    def factory(get_response: Any = None):
        if get_response is None:
            return factory
        return _BoundMiddleware(get_response, client)

    factory.sync_capable = True  # type: ignore[attr-defined]
    factory.async_capable = True  # type: ignore[attr-defined]
    return factory


def _span_name(method: str, route: str) -> str:
    if method and route:
        return f"{method} {route}"
    return method or route or _UNMATCHED_ROUTE


def _build_server_span(
    instrumentation: _Instrumentation,
    propagator: Any,
    request: Any,
    method: str,
    route: str,
    span_attrs: dict,
):
    headers = getattr(request, "headers", None)
    carrier = {k.lower(): v for k, v in headers.items()} if headers is not None else {}
    parent = propagator.extract(carrier)
    span = instrumentation.tracer.start_span(
        _span_name(method, route),
        context=parent,
        kind=_otel_trace.SpanKind.SERVER,
        attributes=span_attrs,
    )
    return _otel_trace.set_span_in_context(span), span


def _setup_request_instrumentation(
    instrumentation: _Instrumentation,
    client: Any,
    request: Any,
) -> Optional[Tuple[Any, Any, str, float]]:
    """Start SERVER span, attach OTel context, increment active_requests.

    Returns ``(token, span, method, start)`` on success, or ``None`` if setup
    failed (after cleaning up partial state so the caller can pass through).
    """
    token = None
    span = None
    active_added = False
    start = _time.monotonic()
    method = ""
    try:
        method = getattr(request, "method", "") or ""
        path = getattr(request, "path", "") or ""
        initial_route = _route_for(request)
        propagator = client.propagator

        span_attrs = {
            "http.request.method": method,
            "url.scheme": _request_scheme(request),
            "url.path": path,
            "server.address": _safe_host(request),
        }
        protocol = _protocol_version(request)
        if protocol:
            span_attrs["network.protocol.version"] = protocol

        request_context, span = _build_server_span(
            instrumentation, propagator, request, method, initial_route, span_attrs
        )
        token = _otel_context.attach(request_context)
        setattr(request, _SPAN_ATTR, span)
        setattr(request, _START_ATTR, start)
        instrumentation.active.add(1, {"http.request.method": method})
        active_added = True
        return token, span, method, start
    except Exception as exc:  # noqa: BLE001 - SDK setup failure must never break the request
        logger.warning("lazyobserve Django middleware request setup failed: %s", exc)
        if active_added:
            try:
                instrumentation.active.add(-1, {"http.request.method": method})
            except Exception:  # noqa: BLE001
                pass
        if span is not None:
            try:
                span.end()
            except Exception:  # noqa: BLE001
                pass
        if token is not None:
            try:
                _otel_context.detach(token)
            except Exception:  # noqa: BLE001
                pass
        return None


def _make_finalizer(
    instrumentation: _Instrumentation,
    request: Any,
    span: Any,
    method: str,
    start: float,
    status_holder: list[int],
) -> Callable[[Optional[BaseException]], None]:
    """Build an idempotent finalizer recording duration, active_requests, and span end."""
    lock = threading.Lock()
    finished = False

    def finalize(stream_exc: Optional[BaseException] = None) -> None:
        nonlocal finished
        with lock:
            if finished:
                return
            finished = True
        status = status_holder[0]
        try:
            route = _route_for(request)
            span.update_name(_span_name(method, route))
            span.set_attribute("http.route", route)
            span.set_attribute("http.response.status_code", status)
            if stream_exc is not None:
                if isinstance(stream_exc, Exception):
                    span.record_exception(stream_exc)
                span.set_status(_otel_trace.Status(_otel_trace.StatusCode.ERROR, str(stream_exc)))
            elif status >= 500:
                span.set_status(_otel_trace.Status(_otel_trace.StatusCode.ERROR))
            instrumentation.duration.record(
                _time.monotonic() - start,
                {
                    "http.request.method": method,
                    "http.route": route,
                    "http.response.status_code": status,
                },
            )
        except Exception:  # noqa: BLE001
            pass
        try:
            instrumentation.active.add(-1, {"http.request.method": method})
        except Exception:  # noqa: BLE001
            pass
        try:
            span.end()
        except Exception:  # noqa: BLE001
            pass

    return finalize


def _wrap_streaming_response(
    response: Any,
    finalize: Callable[[Optional[BaseException]], None],
) -> bool:
    """Wrap a Django ``StreamingHttpResponse`` so metrics/span finish when streaming ends."""
    if not getattr(response, "streaming", False):
        return False
    if not hasattr(response, "streaming_content"):
        return False
    try:
        is_async_stream = bool(getattr(response, "is_async", False))
        original = response.streaming_content
        if is_async_stream:
            async def _async_stream():
                try:
                    async for chunk in original:
                        yield chunk
                except BaseException as exc:
                    finalize(exc)
                    raise
                finally:
                    finalize()

            response.streaming_content = _async_stream()
        else:
            def _sync_stream():
                try:
                    for chunk in original:
                        yield chunk
                except BaseException as exc:
                    finalize(exc)
                    raise
                finally:
                    finalize()

            response.streaming_content = _sync_stream()

        closers = getattr(response, "_resource_closers", None)
        if isinstance(closers, list):
            closers.append(finalize)
        return True
    except Exception:  # noqa: BLE001 - fall back to immediate finalization on any unexpected shape
        return False


def _detach_context(token: Any) -> None:
    try:
        _otel_context.detach(token)
    except Exception:  # noqa: BLE001
        pass


def _instrument_request(instance: _BaseMiddleware, client: Any, request: Any) -> Any:
    instrumentation = instance._client_for(client)
    if instrumentation is None:
        return instance.get_response(request)

    setup = _setup_request_instrumentation(instrumentation, client, request)
    if setup is None:
        return instance.get_response(request)
    token, span, method, start = setup

    status_holder = [500]
    finalize = _make_finalizer(instrumentation, request, span, method, start, status_holder)
    stream_owned = False
    try:
        response = instance.get_response(request)
        status_holder[0] = getattr(response, "status_code", 200)
        stream_owned = _wrap_streaming_response(response, finalize)
        return response
    except Exception as exc:  # noqa: BLE001 - defensive path for non-Django callers
        if _is_exception_recorded(request, exc):
            raise
        _observe_exception(client, request, exc)
        if getattr(client.config, "re_raise_on_500", False):
            raise
        from django.http import HttpResponseServerError

        return HttpResponseServerError("Internal Server Error")
    finally:
        if not stream_owned:
            finalize(None)
        _detach_context(token)


async def _instrument_request_async(instance: _BaseMiddleware, client: Any, request: Any) -> Any:
    instrumentation = instance._client_for(client)
    if instrumentation is None:
        return await instance.get_response(request)

    setup = _setup_request_instrumentation(instrumentation, client, request)
    if setup is None:
        return await instance.get_response(request)
    token, span, method, start = setup

    status_holder = [500]
    finalize = _make_finalizer(instrumentation, request, span, method, start, status_holder)
    stream_owned = False
    try:
        response = await instance.get_response(request)
        status_holder[0] = getattr(response, "status_code", 200)
        stream_owned = _wrap_streaming_response(response, finalize)
        return response
    except Exception as exc:  # noqa: BLE001 - defensive path for non-Django callers
        if _is_exception_recorded(request, exc):
            raise
        _observe_exception(client, request, exc)
        if getattr(client.config, "re_raise_on_500", False):
            raise
        from django.http import HttpResponseServerError

        return HttpResponseServerError("Internal Server Error")
    finally:
        if not stream_owned:
            finalize(None)
        _detach_context(token)


def _is_exception_recorded(request: Any, exc: BaseException) -> bool:
    recorded = getattr(request, _RECORDED_EXCS_ATTR, None)
    return isinstance(recorded, set) and id(exc) in recorded


def _mark_exception_recorded(request: Any, exc: BaseException) -> bool:
    """Return True if ``exc`` was not yet recorded on ``request``, and mark it."""
    recorded = getattr(request, _RECORDED_EXCS_ATTR, None)
    if not isinstance(recorded, set):
        recorded = set()
        try:
            setattr(request, _RECORDED_EXCS_ATTR, recorded)
        except Exception:  # noqa: BLE001
            return True
    exc_id = id(exc)
    if exc_id in recorded:
        return False
    recorded.add(exc_id)
    return True


def _observe_exception(client: Any, request: Any, exc: BaseException) -> bool:
    """Record exception on span, log with exc_info, and enqueue an error event once."""
    if not _mark_exception_recorded(request, exc):
        return False
    try:
        method = getattr(request, "method", "") or ""
        path = getattr(request, "path", "") or ""
        span = getattr(request, _SPAN_ATTR, None)
        if span is not None:
            span.record_exception(exc)
            span.set_status(_otel_trace.Status(_otel_trace.StatusCode.ERROR, str(exc)))
        exc_logger = (
            _django_logger
            if getattr(client.logger, "name", "") == "lazyobserve"
            else client.logger
        )
        exc_logger.error(
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
        )
    except Exception:  # noqa: BLE001 - silent degradation, never mask original exception
        pass
    return True
