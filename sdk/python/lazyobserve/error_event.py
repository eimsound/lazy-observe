"""Error event capture and platform delivery (POST /api/events/errors).

The JSON schema mirrors the Go SDK's errorEvent and the platform's
errortrack.Event wire format exactly.
"""

from __future__ import annotations

import json
import logging
import os
import sys
import sysconfig
import uuid
from dataclasses import dataclass, field, replace
from typing import Any, Callable, Dict, List, Optional, Tuple

from opentelemetry import trace as otel_trace

from ._http import (
    DEFAULT_ERROR_SEND_TIMEOUT,
    ERROR_EVENT_SIZE_LIMIT,
    InternalHTTPTransport,
    PayloadTooLargeError,
    PermanentHTTPError,
)
from .delivery import DeliveryQueue, DeliveryWarn

_ERROR_BATCH_SIZE = 20
_ERROR_FLUSH_INTERVAL = 0.1
_ERROR_SEND_TIMEOUT = DEFAULT_ERROR_SEND_TIMEOUT
_EVENT_SIZE_LIMIT = ERROR_EVENT_SIZE_LIMIT

_MAX_MESSAGE_BYTES = 16 << 10
_MAX_EXCEPTION_VALUE_BYTES = 16 << 10
_MAX_TAG_VALUE_BYTES = 1024
_MAX_URL_BYTES = 2048
_MAX_FRAMES = 200

_NON_APP_MODULE_PREFIXES = (
    "lazyobserve",
    "django",
    "rest_framework",
    "opentelemetry",
    "httpx",
    "httpcore",
    "urllib3",
    "requests",
    "asgiref",
    "sqlparse",
    "gunicorn",
    "uvicorn",
    "werkzeug",
    "starlette",
    "fastapi",
    "flask",
    "celery",
    "pytest",
    "_pytest",
    "pluggy",
    "wsgiref",
)


def _stdlib_dirs() -> Tuple[str, ...]:
    dirs: List[str] = []
    for key in ("stdlib", "platstdlib"):
        try:
            path = sysconfig.get_path(key)
        except Exception:  # noqa: BLE001
            path = None
        if path:
            dirs.append(os.path.normcase(os.path.realpath(path)))
    try:
        os_file = getattr(os, "__file__", None)
        if os_file:
            dirs.append(os.path.normcase(os.path.realpath(os.path.dirname(os_file))))
    except Exception:  # noqa: BLE001
        pass
    unique: List[str] = []
    for entry in dirs:
        if entry and entry not in unique:
            unique.append(entry)
    return tuple(unique)


_STDLIB_DIRS = _stdlib_dirs()
_STDLIB_MODULES = frozenset(getattr(sys, "stdlib_module_names", ())) - {"test", "__main__"}


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


def _matches_module_prefix(module: str, prefixes: Tuple[str, ...]) -> bool:
    for prefix in prefixes:
        if module == prefix or module.startswith(prefix + "."):
            return True
    return False


def _is_non_app_path(filename: str) -> bool:
    if not filename:
        return False
    if filename.startswith("<frozen"):
        return True
    normalized_slash = filename.replace("\\", "/").lower()
    if (
        "/site-packages/" in normalized_slash
        or "/dist-packages/" in normalized_slash
        or normalized_slash.startswith("site-packages/")
        or normalized_slash.startswith("dist-packages/")
    ):
        return True
    if os.path.isabs(filename):
        try:
            real_path = os.path.normcase(os.path.realpath(filename))
        except Exception:  # noqa: BLE001
            real_path = os.path.normcase(os.path.normpath(filename))
        for stdlib_dir in _STDLIB_DIRS:
            if real_path == stdlib_dir or real_path.startswith(stdlib_dir + os.sep):
                return True
    return False


def _in_app(module: str, filename: str) -> bool:
    if module:
        if _matches_module_prefix(module, _NON_APP_MODULE_PREFIXES):
            return False
        top_level = module.split(".", 1)[0]
        if top_level in _STDLIB_MODULES:
            return False
    if _is_non_app_path(filename):
        return False
    return True


def _truncate_utf8(value: Optional[str], max_bytes: int) -> Optional[str]:
    if value is None:
        return None
    if max_bytes <= 0:
        return ""
    raw = value.encode("utf-8")
    if len(raw) <= max_bytes:
        return value
    return raw[:max_bytes].decode("utf-8", "ignore")


def _trim_frames(
    frames: List[ErrorFrame],
    limit: int,
    *,
    strip_abs_path: bool = False,
) -> List[ErrorFrame]:
    if limit <= 0 or not frames:
        return []
    if len(frames) > limit:
        head = limit // 2
        tail = limit - head
        selected = frames[:head] + (frames[-tail:] if tail > 0 else [])
    else:
        selected = frames
    if not strip_abs_path:
        return list(selected)
    return [replace(frame, abs_path="") for frame in selected]


def _trim_tags(
    tags: Optional[Dict[str, str]],
    max_value_bytes: int,
    *,
    max_count: Optional[int] = None,
    keep_core_only: bool = False,
) -> Optional[Dict[str, str]]:
    if not tags:
        return None
    result: Dict[str, str] = {}
    for key, val in tags.items():
        if keep_core_only and key != "service.name" and not key.startswith("http."):
            continue
        if max_count is not None and len(result) >= max_count:
            break
        result[str(key)] = _truncate_utf8(str(val), max_value_bytes) or ""
    return result or None


def _encoded_event_size(event: ErrorEvent) -> int:
    return len(json.dumps(event.to_json(), separators=(",", ":")).encode("utf-8"))


def fit_event_to_size_limit(
    event: ErrorEvent, *, limit: int = _EVENT_SIZE_LIMIT
) -> Optional[ErrorEvent]:
    """Truncate fields and frames so the serialized event fits within ``limit`` bytes.

    Returns ``None`` if the event still exceeds ``limit`` after progressive trimming.
    """
    candidate = replace(
        event,
        message=_truncate_utf8(event.message, _MAX_MESSAGE_BYTES),
        exception_value=_truncate_utf8(event.exception_value, _MAX_EXCEPTION_VALUE_BYTES),
        url=_truncate_utf8(event.url, _MAX_URL_BYTES),
        tags=_trim_tags(event.tags, _MAX_TAG_VALUE_BYTES),
        frames=_trim_frames(event.frames, _MAX_FRAMES),
    )
    if _encoded_event_size(candidate) <= limit:
        return candidate

    stages = (
        (4096, 4096, 512, 1024, 100, True, None, False),
        (1024, 1024, 256, 512, 32, True, 32, False),
        (256, 256, 128, 256, 8, True, 16, True),
        (0, 256, 64, 256, 0, True, 8, True),
    )
    for msg_b, val_b, tag_b, url_b, frame_n, strip_abs, tag_n, core_only in stages:
        candidate = replace(
            candidate,
            message=_truncate_utf8(candidate.message, msg_b) or None,
            exception_value=_truncate_utf8(candidate.exception_value, val_b) or None,
            url=_truncate_utf8(candidate.url, url_b) or None,
            tags=_trim_tags(
                candidate.tags, tag_b, max_count=tag_n, keep_core_only=core_only
            ),
            frames=_trim_frames(candidate.frames, frame_n, strip_abs_path=strip_abs),
        )
        if _encoded_event_size(candidate) <= limit:
            return candidate
    return None


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
        http_transport: Optional[InternalHTTPTransport] = None,
        transport: Optional[Any] = None,
        send_timeout: float = _ERROR_SEND_TIMEOUT,
    ) -> None:
        self.endpoint = endpoint.rstrip("/") + "/api/events/errors"
        self.token = token
        self.logger = logger
        self.send_timeout = send_timeout
        if http_transport is not None:
            self._http = http_transport
            self._owns_http = False
        else:
            self._http = InternalHTTPTransport(transport=transport, default_timeout=send_timeout)
            self._owns_http = True
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
        if isinstance(event, ErrorEvent):
            fitted = fit_event_to_size_limit(event)
            if fitted is None:
                self.delivery.record_drop(
                    "server_rejected",
                    PayloadTooLargeError(f"error event exceeds {_EVENT_SIZE_LIMIT} bytes"),
                    1,
                )
                return False
            event = fitted
        return self.delivery.enqueue(event)

    def close(self, timeout: Optional[float] = None) -> Optional[BaseException]:
        err = self.delivery.close(timeout)
        if self._owns_http:
            try:
                self._http.close()
            except Exception:  # noqa: BLE001
                pass
        return err

    def dropped(self) -> int:
        return self.delivery.dropped()

    def _send(self, batch: List[ErrorEvent], _timeout: Optional[float]) -> None:
        valid_payloads: List[Dict[str, Any]] = []
        oversized = 0
        for event in batch:
            fitted = fit_event_to_size_limit(event) if isinstance(event, ErrorEvent) else event
            if fitted is None:
                oversized += 1
                continue
            valid_payloads.append(fitted.to_json())
        if oversized:
            self.delivery.record_drop(
                "server_rejected",
                PayloadTooLargeError(f"error event exceeds {_EVENT_SIZE_LIMIT} bytes"),
                oversized,
            )
        if not valid_payloads:
            return
        payload = json.dumps({"events": valid_payloads}, separators=(",", ":")).encode("utf-8")
        try:
            self._http.post(
                self.endpoint,
                content=payload,
                headers={
                    "Authorization": "Bearer " + self.token,
                    "Content-Type": "application/json",
                },
                timeout=self.send_timeout,
                action="send error events",
            )
        except (PermanentHTTPError, PayloadTooLargeError) as exc:
            self.delivery.record_drop("server_rejected", exc, len(valid_payloads))
            return


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
