"""Configuration for the lazy-observe Python SDK."""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from typing import Dict, Optional

from .dsn import DSN

ERROR_QUEUE_SIZE = 100
DELIVERY_MAX_ATTEMPTS = 3
DELIVERY_RETRY_INITIAL = 0.1
DELIVERY_RETRY_MAX = 1.0


@dataclass
class DeliveryConfig:
    """Bounds custom error delivery (zero values select defaults)."""

    error_queue_capacity: int = 0
    max_attempts: int = 0
    retry_initial: float = 0.0
    retry_max: float = 0.0


@dataclass
class Config:
    """Controls SDK construction.

    ``metrics``/``logs``/``traces`` follow the Go SDK's signal matrix: all three
    false means all signals enabled; otherwise the three booleans form an
    explicit enabled set.
    """

    dsn: str = ""
    service_name: str = ""
    metrics: bool = False
    logs: bool = False
    traces: bool = False
    sample_rate: float = 0.0
    tee_to_stderr: bool = True
    capture_db_statement: bool = False
    capture_llm_content: bool = False
    release: str = ""
    environment: str = ""
    tags: Optional[Dict[str, str]] = None
    export_interval: float = 5.0
    delivery: DeliveryConfig = field(default_factory=DeliveryConfig)


class InvalidConfigError(ValueError):
    """Raised for invalid SDK configuration."""


def default_service_name() -> str:
    prog = os.path.basename(sys_argv0())
    if prog and prog != ".":
        return prog
    return "unknown_service"


def sys_argv0() -> str:
    import sys

    return sys.argv[0] if sys.argv else ""


def signal_matrix(cfg: Config) -> tuple[bool, bool, bool]:
    if not cfg.metrics and not cfg.logs and not cfg.traces:
        return True, True, True
    return bool(cfg.metrics), bool(cfg.logs), bool(cfg.traces)


def normalize_config(cfg: Config) -> Config:
    if not cfg.dsn:
        raise InvalidConfigError("DSN is required")
    if not cfg.service_name:
        cfg.service_name = default_service_name()
    if cfg.sample_rate == 0:
        cfg.sample_rate = 1.0
    if not 0 <= cfg.sample_rate <= 1:
        raise InvalidConfigError("sample rate must be between 0 and 1")
    if cfg.export_interval <= 0:
        cfg.export_interval = 5.0
    normalize_delivery(cfg.delivery)
    if cfg.tags is None:
        cfg.tags = {}
    return cfg


def normalize_delivery(cfg: DeliveryConfig) -> None:
    if (
        cfg.error_queue_capacity < 0
        or cfg.max_attempts < 0
        or cfg.retry_initial < 0
        or cfg.retry_max < 0
    ):
        raise InvalidConfigError(
            "delivery capacities, attempts, and retry durations cannot be negative"
        )
    if cfg.error_queue_capacity == 0:
        cfg.error_queue_capacity = ERROR_QUEUE_SIZE
    if cfg.max_attempts == 0:
        cfg.max_attempts = DELIVERY_MAX_ATTEMPTS
    if cfg.retry_initial == 0:
        cfg.retry_initial = DELIVERY_RETRY_INITIAL
    if cfg.retry_max == 0:
        cfg.retry_max = DELIVERY_RETRY_MAX
    if cfg.retry_max < cfg.retry_initial:
        raise InvalidConfigError("delivery retry max cannot be shorter than retry initial")


def parsed_dsn(cfg: Config) -> DSN:
    from .dsn import parse_dsn

    return parse_dsn(cfg.dsn)
