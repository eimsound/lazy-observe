"""Logging bridge: adds service.name and trace/span correlation, then exports
records as OTLP logs and optionally tees to stderr (Go's correlatingHandler).

Use through the returned logger (`lo.logger()`) or configure it in Django's
``LOGGING`` with ``"class": "lazyobserve.LazyObserveLogHandler"``.
"""

from __future__ import annotations

import json
import logging
import sys
import threading
from typing import Any, Dict, Optional, Set

from opentelemetry import trace as otel_trace
from opentelemetry._logs import LogRecord, SeverityNumber

_RESERVED_KEYS: Set[str] = {
    "name",
    "msg",
    "args",
    "levelname",
    "levelno",
    "pathname",
    "filename",
    "module",
    "exc_info",
    "exc_text",
    "stack_info",
    "lineno",
    "funcName",
    "created",
    "msecs",
    "relativeCreated",
    "thread",
    "threadName",
    "processName",
    "process",
    "taskName",
}

_LEVEL_TO_SEVERITY = {
    logging.DEBUG: SeverityNumber.DEBUG,
    logging.INFO: SeverityNumber.INFO,
    logging.WARNING: SeverityNumber.WARN,
    logging.ERROR: SeverityNumber.ERROR,
    logging.CRITICAL: SeverityNumber.FATAL,
}


def _span_context() -> tuple[Optional[str], Optional[str]]:
    span = otel_trace.get_current_span()
    span_context = span.get_span_context()
    if not span_context.is_valid:
        return None, None
    return format(span_context.trace_id, "032x"), format(span_context.span_id, "016x")


def _record_attributes(record: logging.LogRecord) -> Dict[str, Any]:
    attrs: Dict[str, Any] = {"service.name": ""}
    try:
        from . import _registry

        client = _registry.get_active_client()
        if client is not None:
            attrs["service.name"] = client.config.service_name
    except Exception:  # noqa: BLE001
        pass
    trace_id, span_id = _span_context()
    if trace_id and span_id:
        attrs["trace_id"] = trace_id
        attrs["span_id"] = span_id
    for key, value in record.__dict__.items():
        if key in _RESERVED_KEYS:
            continue
        if isinstance(value, (str, int, float, bool)):
            attrs[key] = value
    return attrs


class LazyObserveLogHandler(logging.Handler):
    """Python ``logging`` handler that exports records as OTLP logs with
    ``service.name`` and active ``trace_id``/``span_id`` injected.

    The handler resolves the active SDK client from the lazyobserve registry, so
    it can be declared in Django's ``LOGGING`` without a hard reference to the
    client instance.
    """

    def __init__(self, level: int = logging.NOTSET, tee_to_stderr: bool | None = None) -> None:
        super().__init__(level)
        self.tee_to_stderr = tee_to_stderr

    def emit(self, record: logging.LogRecord) -> None:
        client = None
        try:
            from . import _registry

            client = _registry.get_active_client()
        except Exception:  # noqa: BLE001
            client = None
        if client is None:
            # No SDK instance: still tee the record so logging is never quieter.
            if self._should_tee():
                self._emit_stderr(record)
            return
        self._emit(client, record)
        if self._should_tee(record=record):
            self._emit_stderr(record)

    def _should_tee(self, record: Optional[logging.LogRecord] = None) -> bool:
        if self.tee_to_stderr is not None:
            return self.tee_to_stderr
        try:
            from . import _registry

            client = _registry.get_active_client()
            if client is not None:
                return bool(client.config.tee_to_stderr)
        except Exception:  # noqa: BLE001
            pass
        return True

    def _emit(self, client: Any, record: logging.LogRecord) -> None:
        logger_provider = client.logger_provider
        if logger_provider is None:
            return
        body = record.getMessage()
        severity = _LEVEL_TO_SEVERITY.get(record.levelno, SeverityNumber.INFO)
        ts = int(record.created * 1_000_000_000)
        otel_record = LogRecord(
            timestamp=ts,
            observed_timestamp=ts,
            severity_number=severity,
            severity_text=record.levelname,
            body=body,
            attributes=_record_attributes(record),
        )
        try:
            logger = logger_provider.get_logger("lazyobserve")
            logger.emit(otel_record)
        except Exception:  # noqa: BLE001
            self.handleErrorWrapped(record)

    def _emit_stderr(self, record: logging.LogRecord) -> None:
        try:
            message = self.format(record)
            sys.stderr.write(message + "\n")
        except Exception:  # noqa: BLE001
            self.handleErrorWrapped(record)

    def handleErrorWrapped(self, record: logging.LogRecord) -> None:
        try:
            super().handleError(record)
        except Exception:  # noqa: BLE001
            pass

    def __repr__(self) -> str:
        return f"<LazyObserveLogHandler {'tee' if self._should_tee() else 'otel-only'}>"


class ClientLogger:
    """A stdlib ``Logger`` attached to a LazyObserveLogHandler (Go ``lo.Slog()``)."""

    def __init__(self, client: Any) -> None:
        self.logger = logging.getLogger("lazyobserve.client")
        self.logger.handlers[:] = [LazyObserveLogHandler(tee_to_stderr=bool(client.config.tee_to_stderr))]
        self.logger.propagate = False
        # NOTSET 会继承 root logger 的默认 WARNING，导致 info 级记录被过滤、handler
        # 不触发；显式设为 INFO 使所有常规记录都进入 OTLP 日志导出。
        self.logger.setLevel(logging.INFO)

    def __getattr__(self, name: str) -> Any:
        return getattr(self.logger, name)


def default_json_formatter() -> logging.Formatter:
    """Formatter used for the stderr tee."""

    return logging.Formatter("%(asctime)s %(levelname)s %(name)s %(message)s")
