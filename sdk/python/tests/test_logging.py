"""Tests for LazyObserveLogHandler: self-feedback loop prevention and exc_info preservation."""

from __future__ import annotations

import logging

import pytest

import lazy_observe
from lazy_observe.logging import LazyObserveLogHandler


class _RecordingOtelLogger:
    def __init__(self, sink, on_emit=None):
        self._sink = sink
        self._on_emit = on_emit

    def emit(self, record):
        self._sink.append(record)
        if self._on_emit is not None:
            self._on_emit(record)


class _RecordingLoggerProvider:
    def __init__(self, on_emit=None):
        self.records = []
        self._on_emit = on_emit

    def get_logger(self, _name):
        return _RecordingOtelLogger(self.records, on_emit=self._on_emit)


@pytest.mark.parametrize(
    "internal_name",
    [
        "lazy_observe",
        "lazy_observe.internal",
        "lazy_observe.delivery",
        "opentelemetry",
        "opentelemetry.exporter.otlp.proto.http._log_exporter",
        "urllib3.connectionpool",
        "requests.packages.urllib3",
        "httpx",
        "httpcore.http11",
    ],
)
def test_internal_loggers_skip_otlp_export_but_still_tee_to_stderr(
    client, capsys, internal_name
):
    provider = _RecordingLoggerProvider()
    client.logger_provider = provider
    lazy_observe._set_active(client)
    handler = LazyObserveLogHandler(tee_to_stderr=True)
    record = logging.LogRecord(
        name=internal_name,
        level=logging.WARNING,
        pathname=__file__,
        lineno=1,
        msg="exporter warning",
        args=(),
        exc_info=None,
    )
    try:
        handler.emit(record)
    finally:
        lazy_observe._set_active(None)

    assert provider.records == []
    assert "exporter warning" in capsys.readouterr().err


@pytest.mark.parametrize(
    "app_logger_name",
    [
        "orders.views",
        "lazy_observe.django",
        "lazy_observe.example",
        "lazy_observe.client",
    ],
)
def test_application_and_django_loggers_export_to_otlp(client, app_logger_name):
    provider = _RecordingLoggerProvider()
    client.logger_provider = provider
    lazy_observe._set_active(client)
    handler = LazyObserveLogHandler(tee_to_stderr=False)
    record = logging.LogRecord(
        name=app_logger_name,
        level=logging.INFO,
        pathname=__file__,
        lineno=1,
        msg="handled request",
        args=(),
        exc_info=None,
    )
    try:
        handler.emit(record)
    finally:
        lazy_observe._set_active(None)

    assert len(provider.records) == 1
    assert provider.records[0].body == "handled request"


def test_reentrant_emit_is_guarded_against_feedback_loops(client):
    handler = LazyObserveLogHandler(tee_to_stderr=False)
    reentry_attempts = 0

    def trigger_reentrant_log(_otel_record):
        nonlocal reentry_attempts
        reentry_attempts += 1
        nested = logging.LogRecord(
            name="app.nested",
            level=logging.ERROR,
            pathname=__file__,
            lineno=1,
            msg="nested log during emit",
            args=(),
            exc_info=None,
        )
        handler.emit(nested)

    provider = _RecordingLoggerProvider(on_emit=trigger_reentrant_log)
    client.logger_provider = provider
    lazy_observe._set_active(client)
    try:
        outer = logging.LogRecord(
            name="app.outer",
            level=logging.INFO,
            pathname=__file__,
            lineno=1,
            msg="outer log",
            args=(),
            exc_info=None,
        )
        handler.emit(outer)
    finally:
        lazy_observe._set_active(None)

    assert reentry_attempts == 1
    assert len(provider.records) == 1
    assert provider.records[0].body == "outer log"


def test_exc_info_and_stack_info_are_preserved_in_otlp_record(client):
    provider = _RecordingLoggerProvider()
    client.logger_provider = provider
    lazy_observe._set_active(client)
    handler = LazyObserveLogHandler(tee_to_stderr=False)
    app_logger = logging.getLogger("test.logging.exc")
    app_logger.handlers[:] = [handler]
    app_logger.propagate = False
    app_logger.setLevel(logging.ERROR)

    def _fail():
        raise ValueError("bad input value")

    try:
        try:
            _fail()
        except ValueError:
            app_logger.exception("processing failed", stack_info=True)
    finally:
        app_logger.handlers.clear()
        lazy_observe._set_active(None)

    assert len(provider.records) == 1
    otel_rec = provider.records[0]
    assert "processing failed" in otel_rec.body
    assert "ValueError: bad input value" in otel_rec.body
    assert "_fail" in otel_rec.body
    attrs = dict(otel_rec.attributes)
    assert attrs["exception.type"] == "ValueError"
    assert attrs["exception.message"] == "bad input value"
    assert "_fail" in attrs["exception.stacktrace"]
    assert "code.stacktrace" in attrs


def test_client_logger_instances_are_isolated_from_each_other_and_global_registry():
    from tests.helpers import make_client

    c1 = make_client(service_name="svc-alpha", tee_to_stderr=False)
    c2 = make_client(service_name="svc-beta", tee_to_stderr=False)
    p1 = _RecordingLoggerProvider()
    p2 = _RecordingLoggerProvider()
    c1.logger_provider = p1
    c2.logger_provider = p2

    global_client_logger = logging.getLogger("lazy_observe.client")
    prev_global_handlers = list(global_client_logger.handlers)
    sentinel_handler = logging.NullHandler()
    global_client_logger.handlers[:] = [sentinel_handler]

    try:
        l1 = c1.logger_factory()
        l2 = c2.logger_factory()

        # Isolated loggers: neither touches the process-global singleton or each other.
        assert l1 is not l2
        assert l1 is not global_client_logger
        assert l2 is not global_client_logger
        assert global_client_logger.handlers == [sentinel_handler]

        # Even when global active client points to c2 (or None), l1 still exports to c1.
        lazy_observe._set_active(c2)
        l1.info("hello from alpha")
        lazy_observe._set_active(None)
        l2.info("hello from beta")

        assert len(p1.records) == 1
        assert p1.records[0].body == "hello from alpha"
        assert dict(p1.records[0].attributes)["service.name"] == "svc-alpha"

        assert len(p2.records) == 1
        assert p2.records[0].body == "hello from beta"
        assert dict(p2.records[0].attributes)["service.name"] == "svc-beta"
    finally:
        global_client_logger.handlers[:] = prev_global_handlers
        lazy_observe._set_active(None)
        c1.shutdown(timeout=1.0)
        c2.shutdown(timeout=1.0)


def test_log_handler_explicit_client_binding_overrides_global_registry():
    from tests.helpers import make_client

    bound_client = make_client(service_name="bound-svc", tee_to_stderr=False)
    active_client = make_client(service_name="active-svc", tee_to_stderr=False)
    bound_provider = _RecordingLoggerProvider()
    active_provider = _RecordingLoggerProvider()
    bound_client.logger_provider = bound_provider
    active_client.logger_provider = active_provider

    lazy_observe._set_active(active_client)
    try:
        handler = LazyObserveLogHandler(client=bound_client)
        record = logging.LogRecord(
            name="app.worker",
            level=logging.INFO,
            pathname=__file__,
            lineno=1,
            msg="explicitly bound",
            args=(),
            exc_info=None,
        )
        handler.emit(record)
        assert len(bound_provider.records) == 1
        assert dict(bound_provider.records[0].attributes)["service.name"] == "bound-svc"
        assert active_provider.records == []
    finally:
        lazy_observe._set_active(None)
        bound_client.shutdown(timeout=1.0)
        active_client.shutdown(timeout=1.0)


def test_otlp_log_export_preserves_resource_trace_context_and_zero_warnings():
    import threading
    import warnings
    from http.server import BaseHTTPRequestHandler, HTTPServer

    from opentelemetry.proto.collector.logs.v1.logs_service_pb2 import (
        ExportLogsServiceRequest,
    )

    log_bodies = []
    lock = threading.Lock()

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            if self.path == "/v1/logs":
                with lock:
                    log_bodies.append(body)
            self.send_response(200)
            self.end_headers()

        def log_message(self, format, *args):
            return

    server = HTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    endpoint = f"http://127.0.0.1:{server.server_address[1]}"

    try:
        with warnings.catch_warnings(record=True) as caught:
            warnings.simplefilter("always")
            lo = lazy_observe.init(
                dsn=f"{endpoint}/p/proj@lo_tok",
                service_name="log-compat-svc",
                metrics=False,
                traces=True,
                logs=True,
                tee_to_stderr=False,
            )
            assert not lo.degraded, lo.err()
            try:
                with lo.tracer().start_as_current_span("log-span") as span:
                    span_ctx = span.get_span_context()
                    lo.logger_factory().info("inside span")
                lo.logger_factory().info("outside span")
            finally:
                lo.shutdown(timeout=3.0)

        deprecation_warnings = [
            f"{w.filename}:{w.lineno}: {w.category.__name__}: {w.message}"
            for w in caught
            if issubclass(w.category, (DeprecationWarning, PendingDeprecationWarning))
            and ("lazy_observe" in (w.filename or "") or "opentelemetry" in (w.filename or ""))
        ]
        assert deprecation_warnings == []

        assert len(log_bodies) > 0
        records_by_body = {}
        for raw in log_bodies:
            req = ExportLogsServiceRequest()
            req.ParseFromString(raw)
            for rl in req.resource_logs:
                res_attrs = {a.key: a.value.string_value for a in rl.resource.attributes}
                assert res_attrs["service.name"] == "log-compat-svc"
                assert res_attrs["lazy_observe.project"] == "proj"
                for sl in rl.scope_logs:
                    for lr in sl.log_records:
                        records_by_body[lr.body.string_value] = lr

        assert "inside span" in records_by_body
        inside = records_by_body["inside span"]
        assert inside.trace_id == span_ctx.trace_id.to_bytes(16, "big")
        assert inside.span_id == span_ctx.span_id.to_bytes(8, "big")
        assert inside.flags == int(span_ctx.trace_flags)

        assert "outside span" in records_by_body
        outside = records_by_body["outside span"]
        assert outside.trace_id == b""
        assert outside.span_id == b""
        assert outside.flags == 0
    finally:
        server.shutdown()
        server.server_close()
