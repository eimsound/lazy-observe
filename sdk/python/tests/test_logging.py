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
