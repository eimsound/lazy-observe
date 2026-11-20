"""Explicit Django DB instrumentation via ``connection.execute_wrapper`` -
the Django-sanctioned, no-monkey-patch mechanism. Parity with Go's
``lo.WrapDB``: creates DB client spans and records operation duration; SQL
text is captured only when ``capture_db_statement`` is enabled (parameters
never captured).

Usage:

    with lo.django_db():
        ...  # queries issued inside this block are instrumented
"""

from __future__ import annotations

import time as _time
from contextlib import contextmanager
from typing import Any, Optional

from opentelemetry import trace as _otel_trace

from .client import _detect_engine_class

_DB_DURATION_BUCKETS = [0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0]


def _safe_sql_text(sql: Any) -> str:
    if sql is None:
        return ""
    if isinstance(sql, str):
        return sql
    if isinstance(sql, (bytes, bytearray)):
        return sql.decode("utf-8", "replace")
    try:
        return str(sql)
    except Exception:  # noqa: BLE001
        return ""


def _span_name(capture_statement: bool, sql: str) -> str:
    if not capture_statement or not sql:
        return "DB query"
    preview = " ".join(sql.split())[:160]
    return f"DB {preview}"


@contextmanager
def django_db_wrapper(client: Any, using: str = "default"):
    """Context manager instrumenting DB executions for the given alias."""
    if client is None or client.degraded:
        yield
        return

    try:
        from django.db import connections

        db = connections[using]
        engine = db.settings_dict.get("ENGINE", "")
        db_system = _detect_engine_class(engine)
        capture_statement = bool(getattr(client.config, "capture_db_statement", False))
        tracer = client.tracer_provider.get_tracer("io.lazyobserve.python")
        duration = client.db_duration_histogram(_DB_DURATION_BUCKETS)
    except Exception:  # noqa: BLE001 - silent degradation, never break app queries
        yield
        return

    def wrapper(execute, sql, params=None, many=False, context=None):
        start = _time.monotonic()
        sql_text = _safe_sql_text(sql) if capture_statement else ""
        attrs: dict = {"db.system.name": db_system}
        if capture_statement and sql_text:
            attrs["db.statement"] = sql_text[:4096]
        with tracer.start_as_current_span(
            _span_name(capture_statement, sql_text),
            kind=_otel_trace.SpanKind.CLIENT,
            attributes=attrs,
        ) as span:
            try:
                return execute(sql, params, many, context)
            except BaseException as exc:
                try:
                    span.record_exception(exc)
                    span.set_status(_otel_trace.Status(_otel_trace.StatusCode.ERROR, str(exc)))
                except Exception:  # noqa: BLE001
                    pass
                raise
            finally:
                try:
                    duration.record(_time.monotonic() - start, {"db.system.name": db_system})
                except Exception:  # noqa: BLE001
                    pass

    with db.execute_wrapper(wrapper):
        yield
