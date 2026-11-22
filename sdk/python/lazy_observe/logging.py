"""Logging bridge: adds service.name and trace/span correlation, then exports
records as OTLP logs and optionally tees to stderr (Go's correlatingHandler).

Use through the returned logger (`lo.logger()`) or configure it in Django's
``LOGGING`` with ``"class": "lazy_observe.LazyObserveLogHandler"``.
"""

from __future__ import annotations

import logging
import sys
import threading
import traceback
from typing import Any, Dict, Optional, Set, Tuple

from opentelemetry import trace as otel_trace
from opentelemetry._logs import LogRecord, SeverityNumber

_INTERNAL_LOGGER_EXACT: Set[str] = {"lazy_observe"}
_INTERNAL_LOGGER_PREFIXES: Tuple[str, ...] = (
    "opentelemetry",
    "urllib3",
    "requests",
    "httpx",
    "httpcore",
    "lazy_observe.internal",
    "lazy_observe.delivery",
    "lazy_observe.transport",
    "lazy_observe.profiling",
)

_in_emit = threading.local()

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


def _is_internal_logger(record: logging.LogRecord) -> bool:
    if getattr(record, "_lazy_observe_internal", False):
        return True
    name = getattr(record, "name", "") or ""
    if name in _INTERNAL_LOGGER_EXACT:
        return True
    for prefix in _INTERNAL_LOGGER_PREFIXES:
        if name == prefix or name.startswith(prefix + "."):
            return True
    return False


def _format_exc_info(record: logging.LogRecord) -> Tuple[Optional[str], Optional[str], str]:
    exc_type_name: Optional[str] = None
    exc_message: Optional[str] = None
    exc_text = record.exc_text or ""
    if record.exc_info:
        if isinstance(record.exc_info, tuple) and len(record.exc_info) == 3:
            exc_type, exc_value, exc_tb = record.exc_info
            if exc_type is not None:
                exc_type_name = getattr(exc_type, "__name__", str(exc_type))
            if exc_value is not None:
                exc_message = str(exc_value)
            if not exc_text and (exc_type is not None or exc_value is not None):
                try:
                    exc_text = "".join(traceback.format_exception(exc_type, exc_value, exc_tb)).rstrip()
                except Exception:  # noqa: BLE001
                    exc_text = ""
        elif isinstance(record.exc_info, BaseException):
            exc = record.exc_info
            exc_type_name = type(exc).__name__
            exc_message = str(exc)
            if not exc_text:
                try:
                    exc_text = "".join(
                        traceback.format_exception(type(exc), exc, exc.__traceback__)
                    ).rstrip()
                except Exception:  # noqa: BLE001
                    exc_text = ""
    return exc_type_name, exc_message, exc_text


def _record_attributes(
    record: logging.LogRecord, client: Optional[Any] = None
) -> Dict[str, Any]:
    attrs: Dict[str, Any] = {"service.name": ""}
    if client is None:
        try:
            from . import _registry

            client = _registry.get_active_client()
        except Exception:  # noqa: BLE001
            client = None
    if client is not None:
        attrs["service.name"] = client.config.service_name
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

    By default the handler resolves the active SDK client from the lazy_observe
    registry, so it can be declared in Django's ``LOGGING`` without a hard
    reference to the client instance. Pass ``client=...`` to bind the handler
    explicitly to one :class:`lazy_observe.Client` instance (used by
    :class:`ClientLogger` for multi-instance isolation).
    """

    def __init__(
        self,
        level: int = logging.NOTSET,
        tee_to_stderr: bool | None = None,
        client: Optional[Any] = None,
    ) -> None:
        super().__init__(level)
        self.tee_to_stderr = tee_to_stderr
        self._client = client

    def _resolve_client(self) -> Optional[Any]:
        if self._client is not None:
            return self._client
        try:
            from . import _registry

            return _registry.get_active_client()
        except Exception:  # noqa: BLE001
            return None

    def emit(self, record: logging.LogRecord) -> None:
        if getattr(_in_emit, "active", False):
            return
        _in_emit.active = True
        try:
            client = self._resolve_client()
            if client is not None and not _is_internal_logger(record):
                self._emit(client, record)
            if self._should_tee(record=record, client=client):
                self._emit_stderr(record)
        finally:
            _in_emit.active = False

    def _should_tee(
        self, record: Optional[logging.LogRecord] = None, client: Optional[Any] = None
    ) -> bool:
        if self.tee_to_stderr is not None:
            return self.tee_to_stderr
        if client is None:
            client = self._resolve_client()
        if client is not None:
            return bool(client.config.tee_to_stderr)
        return True

    def _emit(self, client: Any, record: logging.LogRecord) -> None:
        logger_provider = client.logger_provider
        if logger_provider is None:
            return
        body = record.getMessage()
        attrs = _record_attributes(record, client=client)
        exc_type_name, exc_message, exc_text = _format_exc_info(record)
        if exc_type_name:
            attrs["exception.type"] = exc_type_name
        if exc_message:
            attrs["exception.message"] = exc_message
        if exc_text:
            attrs["exception.stacktrace"] = exc_text
            body = f"{body}\n{exc_text}" if body else exc_text
        if record.stack_info:
            stack_text = str(record.stack_info).rstrip()
            if stack_text:
                attrs["code.stacktrace"] = stack_text
                body = f"{body}\n{stack_text}" if body else stack_text
        severity = _LEVEL_TO_SEVERITY.get(record.levelno, SeverityNumber.INFO)
        ts = int(record.created * 1_000_000_000)
        otel_record = LogRecord(
            timestamp=ts,
            observed_timestamp=ts,
            severity_number=severity,
            severity_text=record.levelname,
            body=body,
            attributes=attrs,
        )
        try:
            logger = logger_provider.get_logger("lazy_observe")
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
    """An isolated stdlib ``Logger`` bound explicitly to one SDK client (Go ``lo.Slog()``)."""

    def __init__(self, client: Any) -> None:
        self.client = client
        # Construct an isolated Logger instance instead of mutating the global
        # logging manager's ``"lazy_observe.client"`` singleton so multiple clients
        # and tests never overwrite each other's handlers or cross-talk via the
        # global active client registry.
        self.logger = logging.Logger("lazy_observe.client")
        self.logger.handlers[:] = [
            LazyObserveLogHandler(
                tee_to_stderr=bool(client.config.tee_to_stderr),
                client=client,
            )
        ]
        self.logger.propagate = False
        # NOTSET 会继承 root logger 的默认 WARNING，导致 info 级记录被过滤、handler
        # 不触发；显式设为 INFO 使所有常规记录都进入 OTLP 日志导出。
        self.logger.setLevel(logging.INFO)

    def __getattr__(self, name: str) -> Any:
        return getattr(self.logger, name)


def default_json_formatter() -> logging.Formatter:
    """Formatter used for the stderr tee."""

    return logging.Formatter("%(asctime)s %(levelname)s %(name)s %(message)s")
