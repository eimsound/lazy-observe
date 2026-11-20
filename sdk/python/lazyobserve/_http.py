"""Unified internal HTTP transport for SDK telemetry uploads.

Both error-event delivery (``POST /api/events/errors``) and continuous-profiling
uploads (``POST /api/profiles``) share :class:`InternalHTTPTransport`, backed by
an isolated ``httpx.Client(follow_redirects=False)`` connection pool owned by
:class:`lazyobserve.client.Client` and cleanly closed during ``Client.shutdown``.

Centralizes:
- Non-redirecting pooled HTTP transport (isolated from global ``urllib`` state
  and never following 3xx redirects that could downgrade POST or leak
  ``Authorization: Bearer <token>`` cross-origin);
- Permanent 4xx classification (excluding 408/429) -> :class:`PermanentHTTPError`
  so callers drop immediately as ``server_rejected``;
- ``Retry-After`` parsing (delta-seconds or HTTP-date, capped) and jittered
  exponential backoff computation;
- Payload size bounds (200 KiB per error event, 10 MiB per profile) and bounded
  response draining (preserving HTTP keep-alive without unbounded memory reads);
- Send timeouts and fork-safe client reset.
"""

from __future__ import annotations

import datetime
import email.utils
import math
import random
import threading
from typing import Any, Dict, Optional

import httpx

ERROR_EVENT_SIZE_LIMIT = 200 << 10
PROFILE_PAYLOAD_SIZE_LIMIT = 10 << 20
DEFAULT_ERROR_SEND_TIMEOUT = 5.0
DEFAULT_PROFILE_SEND_TIMEOUT = 15.0
MAX_RETRY_AFTER = 30.0

_MAX_RESPONSE_READ_BYTES = 64 << 10
_ERROR_BODY_SNIPPET_BYTES = 4096


class RetryableError(RuntimeError):
    """Transient delivery failure that may carry a server-provided Retry-After delay."""

    def __init__(self, message: str, *, retry_after: Optional[float] = None) -> None:
        super().__init__(message)
        self.retry_after = retry_after


class PermanentHTTPError(RuntimeError):
    """Non-retryable 4xx response (excluding 408/429) mapped to ``server_rejected``."""

    def __init__(self, status_code: int, message: str, *, body: str = "") -> None:
        super().__init__(message)
        self.status_code = status_code
        self.body = body


class PayloadTooLargeError(ValueError):
    """Client-side payload size violation mapped to ``server_rejected``."""


def is_permanent_http_status(status_code: int) -> bool:
    """Return True for permanent 4xx statuses (all 4xx except 408 and 429)."""
    return 400 <= status_code < 500 and status_code not in (408, 429)


def check_payload_size(size: int, *, limit: int, label: str) -> Optional[PayloadTooLargeError]:
    """Return a :class:`PayloadTooLargeError` if ``size`` exceeds ``limit`` bytes."""
    if size > limit:
        return PayloadTooLargeError(f"{label} exceeds {limit} bytes")
    return None


def parse_retry_after(
    header_value: Optional[str], *, max_delay: float = MAX_RETRY_AFTER
) -> Optional[float]:
    """Parse an HTTP ``Retry-After`` header (delta-seconds or HTTP-date), capped."""
    if not header_value:
        return None
    raw = header_value.strip()
    if not raw:
        return None
    try:
        seconds = float(raw)
    except ValueError:
        try:
            dt = email.utils.parsedate_to_datetime(raw)
            if dt.tzinfo is None:
                dt = dt.replace(tzinfo=datetime.timezone.utc)
            seconds = (dt - datetime.datetime.now(datetime.timezone.utc)).total_seconds()
        except Exception:  # noqa: BLE001
            return None
    if not math.isfinite(seconds) or seconds <= 0:
        return None
    return min(seconds, max_delay)


def compute_retry_delay(
    retry_index: int,
    retry_initial: float,
    retry_max: float,
    err: Optional[BaseException] = None,
    *,
    max_retry_after: float = MAX_RETRY_AFTER,
) -> float:
    """Compute jittered exponential backoff, respecting an optional ``Retry-After``."""
    delay = max(0.0, retry_initial)
    cap = retry_max if retry_max >= delay else delay
    for _ in range(retry_index):
        if delay >= cap / 2:
            delay = cap
            break
        delay *= 2.0
    delay = min(delay, cap)
    if delay > 0:
        delay *= random.uniform(0.5, 1.0)
    retry_after = getattr(err, "retry_after", None) if err is not None else None
    if isinstance(retry_after, (int, float)) and math.isfinite(retry_after) and retry_after > 0:
        delay = max(delay, min(float(retry_after), max_retry_after))
    return delay


def _read_bounded_body(response: httpx.Response) -> bytes:
    """Drain up to 64 KiB (preserving keep-alive) while retaining up to 4 KiB for diagnostics."""
    snippet = bytearray()
    total = 0
    for chunk in response.iter_bytes():
        if not chunk:
            continue
        total += len(chunk)
        if len(snippet) < _ERROR_BODY_SNIPPET_BYTES:
            need = _ERROR_BODY_SNIPPET_BYTES - len(snippet)
            snippet.extend(chunk[:need])
        if total >= _MAX_RESPONSE_READ_BYTES:
            break
    return bytes(snippet)


class InternalHTTPTransport:
    """Pooled, no-redirect ``httpx.Client`` wrapper for internal SDK POST uploads."""

    def __init__(
        self,
        *,
        transport: Optional[httpx.BaseTransport] = None,
        default_timeout: float = DEFAULT_ERROR_SEND_TIMEOUT,
    ) -> None:
        self._transport = transport
        self._default_timeout = default_timeout
        self._lock = threading.Lock()
        self._client: Optional[httpx.Client] = None
        self._closed = False
        self._in_flight = 0

    def _acquire_client(self) -> httpx.Client:
        with self._lock:
            if self._closed:
                raise RuntimeError("internal HTTP transport is closed")
            if self._client is None:
                kwargs: Dict[str, Any] = {"follow_redirects": False}
                if self._transport is not None:
                    kwargs["transport"] = self._transport
                self._client = httpx.Client(**kwargs)
            self._in_flight += 1
            return self._client

    def _release_client(self) -> None:
        to_close: Optional[httpx.Client] = None
        with self._lock:
            self._in_flight = max(0, self._in_flight - 1)
            if self._closed and self._in_flight == 0 and self._client is not None:
                to_close = self._client
                self._client = None
        if to_close is not None:
            to_close.close()

    def post(
        self,
        url: str,
        *,
        content: bytes,
        headers: Dict[str, str],
        timeout: Optional[float] = None,
        action: str = "send telemetry",
        max_payload_bytes: Optional[int] = None,
    ) -> int:
        """POST ``content`` to ``url`` using the pooled no-redirect client.

        Returns the 2xx status code on success.
        Raises :class:`PayloadTooLargeError` if ``content`` exceeds ``max_payload_bytes``,
        :class:`PermanentHTTPError` for non-transient 4xx (excluding 408/429), and
        :class:`RetryableError` for 3xx redirects, 408/429, 5xx, or transport failures.
        """
        if max_payload_bytes is not None:
            size_err = check_payload_size(len(content), limit=max_payload_bytes, label=action)
            if size_err is not None:
                raise size_err

        effective_timeout = self._default_timeout if timeout is None else timeout
        client = self._acquire_client()
        try:
            try:
                with client.stream(
                    "POST",
                    url,
                    content=content,
                    headers=headers,
                    timeout=effective_timeout,
                ) as response:
                    status_code = response.status_code
                    body_bytes = _read_bounded_body(response)
                    retry_after_header = response.headers.get("Retry-After")
            except httpx.HTTPError as exc:
                raise RetryableError(f"{action}: {exc}") from exc
        finally:
            self._release_client()

        if 200 <= status_code < 300:
            return status_code

        snippet = body_bytes.decode("utf-8", "replace").strip()
        message = (
            f"{action}: HTTP {status_code}: {snippet}"
            if snippet
            else f"{action}: HTTP {status_code}"
        )
        if is_permanent_http_status(status_code):
            raise PermanentHTTPError(status_code, message, body=snippet)

        retry_after = parse_retry_after(retry_after_header)
        raise RetryableError(message, retry_after=retry_after)

    def close(self) -> None:
        """Close the underlying ``httpx.Client`` connection pool. Idempotent.

        If a request is currently in flight, marks the transport closed (blocking
        new requests) and defers closing the underlying pool until the last
        in-flight request completes so in-flight uploads are not aborted mid-socket.
        """
        to_close: Optional[httpx.Client] = None
        with self._lock:
            self._closed = True
            if self._in_flight == 0 and self._client is not None:
                to_close = self._client
                self._client = None
        if to_close is not None:
            to_close.close()

    @property
    def closed(self) -> bool:
        return self._closed

    def _reinit_after_fork(self) -> None:
        """Reset lock and drop inherited pooled client in a forked child without closing parent sockets."""
        self._lock = threading.Lock()
        self._client = None
        self._in_flight = 0


__all__ = [
    "DEFAULT_ERROR_SEND_TIMEOUT",
    "DEFAULT_PROFILE_SEND_TIMEOUT",
    "ERROR_EVENT_SIZE_LIMIT",
    "InternalHTTPTransport",
    "MAX_RETRY_AFTER",
    "PROFILE_PAYLOAD_SIZE_LIMIT",
    "PayloadTooLargeError",
    "PermanentHTTPError",
    "RetryableError",
    "check_payload_size",
    "compute_retry_delay",
    "is_permanent_http_status",
    "parse_retry_after",
]
