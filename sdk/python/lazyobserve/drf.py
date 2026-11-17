"""DRF exception-handler adaptation. Parity with the Fiber 5xx capture: API
exceptions that resolve to an HTTP 5xx response are queued for Issue
aggregation with request context (method/route/status, query-free URL).

Configure in ``REST_FRAMEWORK``:

    "EXCEPTION_HANDLER": "lazyobserve.drf.exception_handler"

or bind explicitly with ``lo.drf_exception_handler()``.
"""

from __future__ import annotations

from typing import Any


def _request_context(request: Any) -> dict:
    if request is None:
        return {}
    resolver = getattr(request, "resolver_match", None)
    route = getattr(resolver, "route", None) if resolver else None
    if not route:
        route = getattr(request, "path", "") or ""
    path = getattr(request, "path", "") or ""
    method = getattr(request, "method", "") or ""
    scheme = "https" if getattr(request, "is_secure", lambda: False)() else "http"
    host = request.get_host() if hasattr(request, "get_host") else ""
    url = f"{scheme}://{host}{path}" if host else path
    return {
        "http.request.method": method,
        "http.route": route,
        "http.response.status_code": "",
        "url": url,
    }


def build_exception_handler(client: Any):
    """Return a DRF exception handler bound to a client."""

    def handler(exc: BaseException, context: dict):
        from rest_framework.views import exception_handler as drf_handler

        response = drf_handler(exc, context)
        if response is None:
            # Non-API exception: re-raise so Django's 500 path (and the lazyobserve
            # middleware) captures it once.
            raise exc
        status = response.status_code
        if status >= 500:
            info = _request_context(context.get("request"))
            tags = {
                "http.request.method": info.get("http.request.method", ""),
                "http.route": info.get("http.route", ""),
                "http.response.status_code": str(status),
            }
            client.capture_exception(
                exc,
                tags=tags,
                url=info.get("url") or None,
            )
        return response

    return handler


def exception_handler(exc: BaseException, context: dict):
    """Registry-bound DRF exception handler (usable as a module path)."""
    from . import _registry

    client = _registry.get_active_client()
    if client is None or client.degraded:
        from rest_framework.views import exception_handler as drf_handler

        return drf_handler(exc, context)
    return build_exception_handler(client)(exc, context)
