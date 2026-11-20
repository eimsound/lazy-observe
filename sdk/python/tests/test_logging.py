"""Tests for LazyObserveLogHandler: self-feedback loop prevention and exc_info preservation."""

from __future__ import annotations

import logging

import pytest

import lazyobserve
from lazyobserve.logging import LazyObserveLogHandler


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
        "lazyobserve",
        "lazyobserve.internal",
        "lazyobserve.delivery",
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
    lazyobserve._set_active(client)
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
        lazyobserve._set_active(None)

    assert provider.records == []
    assert "exporter warning" in capsys.readouterr().err


@pytest.mark.parametrize(
    "app_logger_name",
    [
        "orders.views",
        "lazyobserve.django",
        "lazyobserve.example",
        "lazyobserve.client",
    ],
)
def test_application_and_django_loggers_export_to_otlp(client, app_logger_name):
    provider = _RecordingLoggerProvider()
    client.logger_provider = provider
    lazyobserve._set_active(client)
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
        lazyobserve._set_active(None)

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
    lazyobserve._set_active(client)
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
        lazyobserve._set_active(None)

    assert reentry_attempts == 1
    assert len(provider.records) == 1
    assert provider.records[0].body == "outer log"


def test_exc_info_and_stack_info_are_preserved_in_otlp_record(client):
    provider = _RecordingLoggerProvider()
    client.logger_provider = provider
    lazyobserve._set_active(client)
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
        lazyobserve._set_active(None)

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
