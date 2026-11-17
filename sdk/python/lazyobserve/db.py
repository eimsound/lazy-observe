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


def _span_name(capture_statement: bool, sql: str) -> str:
    if not capture_statement or not sql:
        return "DB query"
    preview = " ".join(sql.split())[:160]
    return f"DB {preview}"


def django_db_wrapper(client: Any, using: str = "default"):
    """Context manager instrumenting DB executions for the given alias."""
    from django.db import connections

    db = connections[using]
    engine = db.settings_dict.get("ENGINE", "")
    db_system = _detect_engine_class(engine)
    capture_statement = bool(getattr(client.config, "capture_db_statement", False))
    meter = client.meter_provider.get_meter("io.lazyobserve.python")
    tracer = client.tracer_provider.get_tracer("io.lazyobserve.python")
    duration = meter.create_histogram(
        "db.client.operation.duration",
        unit="s",
        description="Duration of database operations.",
        explicit_bucket_boundaries_advisory=_DB_DURATION_BUCKETS,
    )

    def wrapper(execute, sql, params=None, many=False, context=None):
        start = _time.monotonic()
        attrs: dict = {"db.system.name": db_system}
        if capture_statement and sql:
            attrs["db.statement"] = sql[:4096]
        with tracer.start_as_current_span(
            _span_name(capture_statement, sql),
            kind=_otel_trace.SpanKind.CLIENT,
            attributes=attrs,
        ):
            result = execute(sql, params, many, context)
        duration.record(_time.monotonic() - start, {"db.system.name": db_system})
        return result

    @contextmanager
    def _cm():
        with db.execute_wrapper(wrapper):
            yield

    return _cm()
