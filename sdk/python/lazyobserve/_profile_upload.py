"""Upload one sampling window to ``POST /api/profiles``.

Wire contract mirrors the Go SDK (``sdk/go/profiling.go``): multipart fields
``service``, ``upload_id``, ``type``, ``start``, ``end`` plus a gzip pprof file
part named ``profile``; ``Authorization: Bearer <token>``. Delivery reuses
``DeliveryQueue`` so the sampling thread never blocks on the network.

Degradation follows ``sdk/AGENTS.md``: non-transient 4xx (excluding 408/429) is
a silent, non-retryable drop counted as ``server_rejected``; 408, 429, 5xx and
network errors are retried by the queue budget.
"""

from __future__ import annotations

import datetime
import secrets
from dataclasses import dataclass
from typing import Any, List, Optional, Tuple

from ._http import (
    DEFAULT_PROFILE_SEND_TIMEOUT,
    PROFILE_PAYLOAD_SIZE_LIMIT,
    InternalHTTPTransport,
    PayloadTooLargeError,
    PermanentHTTPError,
    check_payload_size,
)
from .delivery import DeliveryQueue, DeliveryWarn

PROFILE_QUEUE_CAPACITY = 6
PROFILE_SEND_TIMEOUT = DEFAULT_PROFILE_SEND_TIMEOUT
MAX_PROFILE_SIZE = PROFILE_PAYLOAD_SIZE_LIMIT
_PROFILE_FLUSH_INTERVAL = 0.1
_PROFILE_PATH = "/api/profiles"
_PROFILE_FILE_NAME = "profile.pb.gz"


@dataclass(frozen=True)
class CollectedProfile:
    """One uploadable window: gzip pprof bytes plus its ingest metadata."""

    upload_id: str
    type_name: str
    start: datetime.datetime
    end: datetime.datetime
    data: bytes


def new_upload_id() -> str:
    """Return a 32-hex upload id for one sampling window.

    Every window draws 128 bits from the OS CSPRNG, matching the Go SDK's
    ``newEventID``. ``secrets``/``os.urandom`` is fork-safe, so forked workers
    do not repeat ids and no reseeding is required.
    """
    return secrets.token_hex(16)


class ProfileUploader:
    """Queue-backed sender for collected profiling windows."""

    def __init__(
        self,
        endpoint: str,
        token: str,
        service: str,
        queue_capacity: int = PROFILE_QUEUE_CAPACITY,
        max_attempts: int = 3,
        retry_initial: float = 0.1,
        retry_max: float = 1.0,
        warn: Optional[DeliveryWarn] = None,
        send_timeout: float = PROFILE_SEND_TIMEOUT,
        http_transport: Optional[InternalHTTPTransport] = None,
        transport: Optional[Any] = None,
        name: str = "profile",
    ) -> None:
        self.url = endpoint.rstrip("/") + _PROFILE_PATH
        self.token = token
        self.service = service
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
            batch_size=1,
            flush_interval=_PROFILE_FLUSH_INTERVAL,
            max_attempts=max_attempts,
            retry_initial=retry_initial,
            retry_max=retry_max,
            warn=warn,
            name=name,
            # ProfilingController drops the whole uploader in a forked child and
            # builds a new one; reviving this queue as well would leak a worker.
            reinit_on_fork=False,
        )

    def enqueue(self, profile: CollectedProfile) -> bool:
        if isinstance(profile, CollectedProfile):
            size_err = check_payload_size(len(profile.data), limit=MAX_PROFILE_SIZE, label="profile")
            if size_err is not None:
                self.delivery.record_drop("server_rejected", size_err, 1)
                return False
        return self.delivery.enqueue(profile)

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

    def _send(self, batch: List[CollectedProfile], _timeout: Optional[float]) -> None:
        for profile in batch:
            self._send_one(profile)

    def _send_one(self, profile: CollectedProfile) -> None:
        size_err = check_payload_size(len(profile.data), limit=MAX_PROFILE_SIZE, label="profile")
        if size_err is not None:
            self.delivery.record_drop("server_rejected", size_err, 1)
            return
        body, boundary = _encode_multipart(self.service, profile)
        try:
            self._http.post(
                self.url,
                content=body,
                headers={
                    "Authorization": "Bearer " + self.token,
                    "Content-Type": f"multipart/form-data; boundary={boundary}",
                    "Content-Length": str(len(body)),
                },
                timeout=self.send_timeout,
                action="upload profile",
            )
        except (PermanentHTTPError, PayloadTooLargeError) as exc:
            self.delivery.record_drop("server_rejected", exc, 1)
            return


def _encode_multipart(service: str, profile: CollectedProfile) -> Tuple[bytes, str]:
    boundary = "lazyobserve" + secrets.token_hex(16)
    body = bytearray()
    fields = (
        ("service", service),
        ("upload_id", profile.upload_id),
        ("type", profile.type_name),
        ("start", _format_timestamp(profile.start)),
        ("end", _format_timestamp(profile.end)),
    )
    for name, value in fields:
        body += f'--{boundary}\r\nContent-Disposition: form-data; name="{name}"\r\n\r\n'.encode()
        body += value.encode("utf-8") + b"\r\n"
    body += (
        f'--{boundary}\r\nContent-Disposition: form-data; name="profile"; '
        f'filename="{_PROFILE_FILE_NAME}"\r\n'
        "Content-Type: application/octet-stream\r\n\r\n"
    ).encode()
    body += profile.data + b"\r\n"
    body += f"--{boundary}--\r\n".encode()
    return bytes(body), boundary


def _format_timestamp(value: datetime.datetime) -> str:
    if value.tzinfo is None:
        value = value.replace(tzinfo=datetime.timezone.utc)
    return value.astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
