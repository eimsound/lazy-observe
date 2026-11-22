"""Configuration for the lazy-observe Python SDK."""

from __future__ import annotations

import copy
import logging
import math
import os
from dataclasses import dataclass, field
from typing import Any, Dict, Mapping, Optional, Sequence

from .dsn import DSN

_logger = logging.getLogger("lazy_observe")

ERROR_QUEUE_SIZE = 100
DELIVERY_MAX_ATTEMPTS = 3
DELIVERY_RETRY_INITIAL = 0.1
DELIVERY_RETRY_MAX = 1.0
#: Default gap between the starts of two profiling windows (Go: ``ProfilingInterval``).
PROFILING_INTERVAL = 60.0
COMPRESSION_NONE = "none"
COMPRESSION_GZIP = "gzip"


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

    dsn: str = field(default="", repr=False)
    service_name: str = ""
    metrics: bool = False
    logs: bool = False
    traces: bool = False
    sample_rate: float = 0.0
    compression: str = ""
    tee_to_stderr: bool = True
    capture_db_statement: bool = False
    capture_llm_content: bool = False
    re_raise_on_500: bool = False
    release: str = ""
    environment: str = ""
    tags: Optional[Dict[str, str]] = None
    export_interval: float = 5.0
    delivery: DeliveryConfig = field(default_factory=DeliveryConfig)
    #: Experimental continuous CPU profiling (off by default). Samples only on Linux
    #: with a free-threaded CPython 3.15+ whose GIL is off; anywhere else switching it
    #: on logs one warning with a reason code and samples nothing.
    profiling: bool = False
    #: Seconds between the starts of two 10 s profiling windows; ``<= 0`` (or not a
    #: finite number) selects the default, below 10 the windows run back to back.
    profiling_interval: float = PROFILING_INTERVAL

    def __post_init__(self) -> None:
        if isinstance(self.tags, dict):
            self.tags = copy.deepcopy(self.tags)


class InvalidConfigError(ValueError):
    """Raised for invalid SDK configuration."""


def warn_unknown_fields(
    source: str,
    unknown: Sequence[str],
    logger: Optional[logging.Logger] = None,
) -> None:
    """Emit a single clear warning listing unrecognized configuration keys."""
    if not unknown:
        return
    target = logger if logger is not None else _logger
    target.warning(
        "%s: ignoring unknown config field(s): %s",
        source,
        ", ".join(sorted(str(k) for k in unknown)),
    )


def config_from_mapping(
    data: Mapping[str, Any],
    *,
    source: str = "lazy_observe.init",
) -> Config:
    """Construct a :class:`Config` from a mapping, deserializing nested ``delivery``
    dicts and warning once on any unknown top-level or nested keys."""
    known = Config.__dataclass_fields__
    unknown: list[str] = [str(k) for k in data if k not in known]
    filtered: Dict[str, Any] = {k: v for k, v in data.items() if k in known}

    if "delivery" in filtered and isinstance(filtered["delivery"], dict):
        raw_delivery = filtered["delivery"]
        delivery_known = DeliveryConfig.__dataclass_fields__
        unknown.extend(f"delivery.{k}" for k in raw_delivery if k not in delivery_known)
        filtered["delivery"] = DeliveryConfig(
            **{k: v for k, v in raw_delivery.items() if k in delivery_known}
        )

    warn_unknown_fields(source, unknown)
    return Config(**filtered)


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
    cfg.profiling = bool(cfg.profiling)
    cfg.re_raise_on_500 = bool(cfg.re_raise_on_500)
    if not (cfg.profiling_interval > 0 and math.isfinite(cfg.profiling_interval)):
        cfg.profiling_interval = PROFILING_INTERVAL  # also catches NaN and +-inf
    cfg.compression = normalize_compression(cfg.compression)
    cfg.delivery = _coerce_delivery_config(cfg.delivery)
    normalize_delivery(cfg.delivery)
    if cfg.tags is None:
        cfg.tags = {}
    elif isinstance(cfg.tags, dict):
        cfg.tags = copy.deepcopy(cfg.tags)
    else:
        raise InvalidConfigError("tags must be a dict")
    return cfg


def normalize_compression(value: object) -> str:
    if value is None:
        return ""
    if not isinstance(value, str):
        raise InvalidConfigError("compression must be a string")
    normalized = value.strip().lower()
    if normalized in ("", COMPRESSION_NONE, COMPRESSION_GZIP):
        return normalized
    raise InvalidConfigError(
        f"invalid OTLP compression {value!r}: must be '', 'none', or 'gzip'"
    )


def resolve_signal_compression(
    explicit: str,
    signal: str,
    env: Optional[Mapping[str, str]] = None,
) -> str:
    explicit_norm = normalize_compression(explicit)
    if explicit_norm:
        return explicit_norm
    environ = os.environ if env is None else env
    signal_var = f"OTEL_EXPORTER_OTLP_{signal.upper()}_COMPRESSION"
    raw_signal = environ.get(signal_var, "").strip()
    if raw_signal:
        try:
            return normalize_compression(raw_signal)
        except InvalidConfigError as exc:
            raise InvalidConfigError(f"{signal_var}: {exc}") from exc
    raw_shared = environ.get("OTEL_EXPORTER_OTLP_COMPRESSION", "").strip()
    if raw_shared:
        try:
            return normalize_compression(raw_shared)
        except InvalidConfigError as exc:
            raise InvalidConfigError(
                f"OTEL_EXPORTER_OTLP_COMPRESSION: {exc}"
            ) from exc
    return COMPRESSION_NONE


def _coerce_delivery_config(value: object) -> DeliveryConfig:
    if value is None:
        return DeliveryConfig()
    if isinstance(value, DeliveryConfig):
        return copy.deepcopy(value)
    if isinstance(value, dict):
        known = DeliveryConfig.__dataclass_fields__
        unknown = [f"delivery.{k}" for k in value if k not in known]
        warn_unknown_fields("lazy_observe.config", unknown)
        try:
            return DeliveryConfig(**{k: v for k, v in value.items() if k in known})
        except TypeError as exc:
            raise InvalidConfigError(f"invalid delivery configuration: {exc}") from exc
    raise InvalidConfigError("delivery must be a DeliveryConfig or dict")


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
