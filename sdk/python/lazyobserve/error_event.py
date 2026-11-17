"""Error event capture and platform delivery (POST /api/events/errors).

The JSON schema mirrors the Go SDK's errorEvent and the platform's
errortrack.Event wire format exactly.
"""

from __future__ import annotations

import json
import logging
import os
import time
import traceback
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Tuple

from opentelemetry import trace as otel_trace

from .delivery import DeliveryQueue, DeliveryWarn

_ERROR_BATCH_SIZE = 20
_ERROR_FLUSH_INTERVAL = 0.1
_ERROR_SEND_TIMEOUT = 5.0
_EVENT_SIZE_LIMIT = 200 << 10


@dataclass
class ErrorFrame:
    function: str
    filename: str
    lineno: Optional[int] = None
    abs_path: Optional[str] = ""
    module: Optional[str] = ""
    in_app: bool = True


@dataclass
class ErrorEvent:
    event_id: str
    timestamp: str
    platform: str
    release: Optional[str] = None
    environment: Optional[str] = None
    level: str = "error"
    message: Optional[str] = None
    exception_type: Optional[str] = None
    exception_value: Optional[str] = None
    frames: List[ErrorFrame] = field(default_factory=list)
    tags: Optional[Dict[str, str]] = None
    trace_id: Optional[str] = None
    span_id: Optional[str] = None
    url: Optional[str] = None

    def to_json(self) -> Dict[str, Any]:
        payload: Dict[str, Any] = {
            "event_id": self.event_id,
            "timestamp": self.timestamp,
            "platform": self.platform,
            "level": self.level,
            "exception": {
                "type": self.exception_type or "",
                "value": self.exception_value or "",
                "stacktrace": {"frames": [_frame_json(f) for f in self.frames]},
            },
        }
        for name, value in (
            ("release", self.release),
            ("environment", self.environment),
            ("message", self.message),
            ("url", self.url),
        ):
            if value:
                payload[name] = value
        if self.tags:
            payload["tags"] = self.tags
        if self.trace_id and self.span_id:
            payload["contexts"] = {"trace": {"trace_id": self.trace_id, "span_id": self.span_id}}
        return payload


def new_event_id() -> str:
    return uuid.uuid4().hex


def _frame_json(frame: ErrorFrame) -> Dict[str, Any]:
    payload: Dict[str, Any] = {
        "function": frame.function,
        "filename": frame.filename,
        "in_app": frame.in_app,
    }
    if frame.lineno is not None:
        payload["lineno"] = frame.lineno
    if frame.abs_path:
        payload["abs_path"] = frame.abs_path
    if frame.module:
        payload["module"] = frame.module
    return payload


def _module_for_frame(frame: Any) -> str:
    try:
        module = frame.f_globals.get("__name__", "")
    except Exception:  # noqa: BLE001
        return ""
    return module or ""


def _in_app(module: str, filename: str) -> bool:
    if module == "lazyobserve" or module.startswith("lazyobserve."):
        return False
    return True


def collect_frames(
    tb: Any,
) -> List[ErrorFrame]:
    """Collect a Python stacktrace as an outermost-first frame list (Go parity)."""
    frames: List[ErrorFrame] = []
    current = tb
    while current is not None:
        frame = current.tb_frame
        module = _module_for_frame(frame)
        code = frame.f_code
        lineno = current.tb_lineno
        filename = code.co_filename
        frames.append(
            ErrorFrame(
                function=code.co_name,
                filename=os.path.basename(filename),
                lineno=lineno,
                abs_path=filename,
                module=module,
                in_app=_in_app(module, filename),
            )
        )
        current = current.tb_next
    return frames


def exception_type_name(exc: BaseException) -> str:
    name = type(exc).__name__
    return name or "Error"


def trace_context_from_current() -> Tuple[Optional[str], Optional[str]]:
    span = otel_trace.get_current_span()
    span_context = span.get_span_context()
    if not span_context.is_valid:
        return None, None
    return format(span_context.trace_id, "032x"), format(span_context.span_id, "016x")


class ErrorTransport:
    """Queue-backed sender for error events (at-least-once, bounded, non-blocking)."""

    def __init__(
        self,
        endpoint: str,
        token: str,
        logger: logging.Logger,
        queue_capacity: int = 100,
        max_attempts: int = 3,
        retry_initial: float = 0.1,
        retry_max: float = 1.0,
        warn: Optional[DeliveryWarn] = None,
    ) -> None:
        self.endpoint = endpoint.rstrip("/") + "/api/events/errors"
        self.token = token
        self.logger = logger
        self.delivery = DeliveryQueue(
            send=self._send,
            capacity=queue_capacity,
            batch_size=_ERROR_BATCH_SIZE,
            flush_interval=_ERROR_FLUSH_INTERVAL,
            max_attempts=max_attempts,
            retry_initial=retry_initial,
            retry_max=retry_max,
            warn=warn,
            name="error-events",
        )

    def enqueue(self, event: ErrorEvent) -> bool:
        return self.delivery.enqueue(event)

    def close(self, timeout: Optional[float] = None) -> Optional[BaseException]:
        return self.delivery.close(timeout)

    def dropped(self) -> int:
        return self.delivery.dropped()

    def _send(self, batch: List[ErrorEvent], _timeout: Optional[float]) -> None:
        payload = json.dumps(
            {"events": [event.to_json() for event in batch]}, separators=(",", ":")
        ).encode("utf-8")
        request = urllib.request.Request(
            self.endpoint,
            data=payload,
            method="POST",
            headers={
                "Authorization": "Bearer " + self.token,
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=_ERROR_SEND_TIMEOUT) as response:
                response.read()
        except urllib.error.HTTPError as exc:
            body = exc.read(4096).decode("utf-8", "replace").strip()
            raise RuntimeError(
                f"send error events: HTTP {exc.code}: {body}"
            ) from exc
        except urllib.error.URLError as exc:
            raise RuntimeError(f"send error events: {exc.reason}") from exc


OnDrop = Callable[[str, Optional[BaseException], int], None]

__all__ = [
    "ErrorEvent",
    "ErrorFrame",
    "ErrorTransport",
    "collect_frames",
    "exception_type_name",
    "new_event_id",
    "trace_context_from_current",
]
