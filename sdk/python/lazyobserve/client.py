"""Core client: assembles isolated OpenTelemetry providers, runtime metrics,
and error transport, mirroring the Go SDK's lazyobserve.go."""

from __future__ import annotations

import contextlib
import gc
import logging
import os
import platform
import resource
import sys
import threading
import time
from typing import Any, Dict, List, Optional

from opentelemetry import metrics as _otel_metrics
from opentelemetry import trace as _otel_trace
from opentelemetry.sdk import resources as _otel_resources
from opentelemetry.sdk._logs import LoggerProvider as _LogsLoggerProvider
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor as _BatchLogRecordProcessor
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator as _TraceContextTextMapPropagator

_INSTRUMENTATION_NAME = "io.lazyobserve.python"

_OTEL_TRACE_QUEUE = 2048
_OTEL_TRACE_BATCH = 512
_OTEL_LOG_QUEUE = 2048
_OTEL_LOG_BATCH = 512


class Diagnostics:
    """Process-local snapshot of bounded delivery losses."""

    dropped_telemetry: int


def _detect_engine_class(engine: str) -> str:
    value = (engine or "").lower()
    if "postgres" in value:
        return "postgresql"
    if "mysql" in value or "mariadb" in value:
        return "mysql"
    if "sqlite" in value:
        return "sqlite"
    if "oracle" in value:
        return "oracle"
    if "mssql" in value or "sqlserver" in value:
        return "mssql"
    return value or "database"


def _threads_count() -> int:
    try:
        return threading.active_count()
    except Exception:  # noqa: BLE001
        return 0


def _memory_rss() -> int:
    try:
        usage = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
        if platform.system() == "Darwin":
            return int(usage)
        return int(usage) * 1024
    except Exception:  # noqa: BLE001
        return 0


def _gc_total() -> int:
    try:
        stats = gc.get_stats()
        return sum(gen.get("collections", 0) for gen in stats)
    except Exception:  # noqa: BLE001
        return 0


class Client:
    """An isolated lazy-observe SDK instance. Never replaces global
    OpenTelemetry providers or installs process-wide hooks."""

    def __init__(
        self,
        config: Any,
        meter: _otel_metrics.MeterProvider,
        tracer_provider: Any,
        logger_provider: Any,
        logger: logging.Logger,
        error_transport: Any = None,
        dropped_counter: Any = None,
        init_err: Optional[BaseException] = None,
    ) -> None:
        self.config = config
        self.meter_provider = meter
        self.tracer_provider = tracer_provider
        self.logger_provider = logger_provider
        self.logger = logger
        self.propagator: Any = _TraceContextTextMapPropagator()
        self.error_transport = error_transport
        self._dropped_counter = dropped_counter
        self._init_err = init_err
        #: The continuous-profiling controller when ``Config.profiling`` is on.
        self.profiling: Any = None

        self._dropped_telemetry = 0
        self._drop_lock = threading.Lock()

    # -- public API ---------------------------------------------------------

    def err(self) -> Optional[BaseException]:
        """Report why init() returned a degraded instance."""
        return self._init_err

    @property
    def degraded(self) -> bool:
        return self._init_err is not None

    def meter(self) -> _otel_metrics.Meter:
        return self.meter_provider.get_meter(_INSTRUMENTATION_NAME)

    def tracer(self) -> Any:
        return self.tracer_provider.get_tracer(_INSTRUMENTATION_NAME)

    def logger_factory(self) -> logging.Logger:
        from .logging import ClientLogger

        return ClientLogger(self).logger

    def diagnostics(self) -> Diagnostics:
        result = Diagnostics()
        result.dropped_telemetry = self._dropped_telemetry
        return result

    def record_telemetry_drop(self, signal: str, reason: str, count: int) -> None:
        if count <= 0:
            return
        with self._drop_lock:
            self._dropped_telemetry += count
        if self._dropped_counter is not None:
            self._dropped_counter.add(
                count,
                {"signal": signal, "reason": reason},
            )

    # -- error events -------------------------------------------------------

    def capture_exception(
        self,
        exc: Optional[BaseException],
        *,
        tags: Optional[Dict[str, str]] = None,
        url: Optional[str] = None,
        context: Any = None,
        omit_stack: bool = False,
        exception_type: Optional[str] = None,
    ) -> str:
        """Queue exc for Issue aggregation, non-blocking. Empty string when
        exc is None or the SDK is degraded."""
        if exc is None or self.error_transport is None:
            return ""
        from .error_event import (
            ErrorEvent,
            collect_frames,
            exception_type_name,
            new_event_id,
            trace_context_from_current,
        )

        event_id = new_event_id()
        frames = [] if omit_stack else collect_frames(exc.__traceback__)
        tags = self._merged_tags(tags)
        trace_id, span_id = trace_context_from_current()
        from . import _registry

        event = ErrorEvent(
            event_id=event_id,
            timestamp=_now_rfc3339(),
            platform="python",
            release=self.config.release or None,
            environment=self.config.environment or None,
            level="error",
            message=str(exc) or None,
            exception_type=exception_type or exception_type_name(exc),
            exception_value=str(exc) or None,
            frames=frames,
            tags=tags,
            trace_id=trace_id,
            span_id=span_id,
            url=url,
        )
        self.error_transport.enqueue(event)
        return event_id

    def _merged_tags(self, event_tags: Optional[Dict[str, str]]) -> Dict[str, str]:
        tags: Dict[str, str] = dict(self.config.tags or {})
        tags["service.name"] = self.config.service_name
        if event_tags:
            tags.update(event_tags)
        return tags

    # -- Django / DRF -------------------------------------------------------

    def django_middleware(self, get_response: Any = None) -> Any:
        """Return the Django middleware (call with get_response, or as a class
        factory). Parity with Go's ``lo.Fiber()``."""
        from .django import build_middleware

        if get_response is not None:
            return build_middleware(self)(get_response)
        return build_middleware(self)

    def drf_exception_handler(self) -> Any:
        """Return a DRF exception handler that records 5xx errors (parity with
        the Fiber 5xx capture)."""
        from .drf import build_exception_handler

        return build_exception_handler(self)

    def django_db(self, using: str = "default") -> Any:
        """Context manager that instruments Django DB executions for the given
        connection alias. Parity with Go's ``lo.WrapDB`` (explicit, no hooks)."""
        from .db import django_db_wrapper

        return django_db_wrapper(self, using)

    def llm_transport(self, base: Any = None, *, client_kind: str = "sync") -> Any:
        """Return a wrapped httpx transport for OpenAI-compatible, Anthropic,
        and Ollama calls. Parity with Go's ``lo.LLMTransport``."""
        from .llm import build_llm_transport

        return build_llm_transport(self, base=base, client_kind=client_kind)

    # -- lifecycle ----------------------------------------------------------

    def shutdown(self, timeout: float = 10.0) -> Optional[BaseException]:
        """Flush all enabled signals and release SDK resources.

        Like Go's ``Shutdown(ctx)``, every step draws on one deadline: error transport,
        then profiling, then the OTel providers. A step that finds the budget spent is
        still run (so it can drop what it holds and release its resources) and its
        result is a ``TimeoutError``; shutdown never raises and never takes longer than
        ``timeout``, whatever a step does (except while the interpreter is finalizing,
        where no thread can be started and the steps run inline). Returns the first
        error, if any.
        """
        errs: List[BaseException] = []
        deadline = time.monotonic() + max(0.0, timeout)

        def remaining() -> float:
            return max(0.0, deadline - time.monotonic())

        def step(name: str, fn: Any) -> None:
            result = _run_bounded(name, fn, remaining())
            if result:
                errs.append(result)

        if self.error_transport is not None:
            transport = self.error_transport
            step("error transport", lambda: transport.close(remaining()))
        if self.profiling is not None:
            # After the error queue (short, user-triggered) and before the OTel
            # providers, as in Go: a profiling window must not eat their budget.
            profiling = self.profiling
            step("profiling", lambda: profiling.shutdown(remaining()))
        for provider, name in (
            (self.logger_provider, "logs"),
            (self.tracer_provider, "traces"),
            (self.meter_provider, "metrics"),
        ):
            # A signal that is off holds None or an API no-op provider, which has no
            # shutdown: nothing to stop.
            stop = getattr(provider, "shutdown", None)
            if not callable(stop):
                continue
            step(name, stop)
        return errs[0] if errs else None

    def __repr__(self) -> str:
        return f"<Client degraded={self.degraded} service={self.config.service_name!r}>"


def _run_bounded(name: str, fn: Any, budget: float) -> Optional[BaseException]:
    """Run one shutdown step on a daemon thread and wait at most ``budget`` for it.

    Python has no ctx to cancel with, and not every step takes a timeout (the OTel
    providers do not), so the wait is what enforces the shared deadline. A step that
    outlives its budget is left to finish in the background; it cannot hold the
    caller. Never raises.

    While the interpreter is finalizing (``shutdown`` called from a ``__del__`` or a
    finalizer at teardown) no thread can be started: 3.12+ raises, 3.9-3.11 block in
    ``start()`` for good. The step then runs inline, unbounded, as it did before the
    shared deadline existed. ``atexit`` handlers run before that and keep the bound.
    """
    outcome: List[Optional[BaseException]] = [None]

    def target() -> None:
        try:
            outcome[0] = fn()
        except Exception as exc:  # noqa: BLE001 - shutdown reports, it never raises
            outcome[0] = RuntimeError(f"{name} shutdown: {exc}")

    if sys.is_finalizing():
        target()
        return outcome[0]
    thread = threading.Thread(target=target, name=f"lazyobserve-shutdown-{name}", daemon=True)
    try:
        thread.start()
    except RuntimeError:  # can't start a thread (finalizing, or out of threads)
        target()
        return outcome[0]
    thread.join(budget)
    if thread.is_alive():
        return TimeoutError(f"{name} shutdown: budget exhausted")
    return outcome[0]


def _now_rfc3339() -> str:
    return _time_now_utc().strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def _time_now_utc():
    import datetime

    return datetime.datetime.now(datetime.timezone.utc)


def _headers(token: str) -> Dict[str, str]:
    return {"Authorization": f"Bearer {token}"}


def build_client(config: Any, parsed: Any) -> Client:
    """Assemble a healthy Client. Raises on construction failure."""
    from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk.metrics import MeterProvider
    from opentelemetry.sdk.metrics._internal.export import PeriodicExportingMetricReader
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor
    from opentelemetry.sdk.trace.sampling import ParentBased, TraceIdRatioBased

    from .config import signal_matrix

    metrics_on, logs_on, traces_on = signal_matrix(config)

    resource = _otel_resources.Resource.create(
        {
            _otel_resources.SERVICE_NAME: config.service_name,
            "lazy_observe.project": parsed.project,
        }
    )

    headers = _headers(parsed.token)
    created: List[Any] = []

    meter_provider: Any = None
    tracer_provider: Any = None
    if metrics_on:
        metric_exporter = OTLPMetricExporter(
            endpoint=parsed.endpoint + "/v1/metrics",
            headers=headers,
        )
        reader = PeriodicExportingMetricReader(
            metric_exporter, export_interval_millis=int(config.export_interval * 1000)
        )
        meter_provider = MeterProvider(resource=resource, metric_readers=[reader])
        created.append(meter_provider)

    if traces_on:
        trace_exporter = OTLPSpanExporter(
            endpoint=parsed.endpoint + "/v1/traces", headers=headers
        )
        processor = BatchSpanProcessor(
            trace_exporter,
            max_queue_size=_OTEL_TRACE_QUEUE,
            max_export_batch_size=_OTEL_TRACE_BATCH,
        )
        tracer_provider = TracerProvider(
            resource=resource,
            sampler=ParentBased(TraceIdRatioBased(config.sample_rate)),
            active_span_processor=processor,
        )
        created.append(tracer_provider)

    logger_provider: Any = None
    if logs_on:
        from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter

        log_exporter = OTLPLogExporter(
            endpoint=parsed.endpoint + "/v1/logs", headers=headers
        )
        log_processor = _BatchLogRecordProcessor(
            log_exporter,
            max_queue_size=_OTEL_LOG_QUEUE,
            max_export_batch_size=_OTEL_LOG_BATCH,
            schedule_delay_millis=int(config.export_interval * 1000),
        )
        logger_provider = _LogsLoggerProvider(resource=resource)
        logger_provider.add_log_record_processor(log_processor)
        created.append(logger_provider)

    import logging as _stdlib_logging

    logger = _stdlib_logging.getLogger("lazyobserve")

    client = Client(
        config=config,
        meter=meter_provider or _otel_metrics.NoOpMeterProvider(),
        tracer_provider=tracer_provider or _otel_trace.NoOpTracerProvider(),
        logger_provider=logger_provider,
        logger=logger,
    )
    if meter_provider is not None:
        _register_runtime_metrics(client)
        client._dropped_counter = _register_dropped_counter(client)

    from .delivery import default_warn_factory
    from .error_event import ErrorTransport

    client.error_transport = ErrorTransport(
        endpoint=parsed.endpoint,
        token=parsed.token,
        logger=logger,
        queue_capacity=config.delivery.error_queue_capacity,
        max_attempts=config.delivery.max_attempts,
        retry_initial=config.delivery.retry_initial,
        retry_max=config.delivery.retry_max,
        warn=default_warn_factory(logger, "error"),
    )
    if config.profiling:
        # Experimental and optional: whatever goes wrong here must not degrade the
        # client. The probe (a 0.1 s trial sample) runs on the profiling thread.
        from . import _profiling

        try:
            client.profiling = _profiling.start_profiling(client, parsed, config, logger)
        except Exception as exc:  # noqa: BLE001
            _profiling.report_start_failure(logger, exc)
    return client


def _register_runtime_metrics(client: Client) -> None:
    from opentelemetry.metrics import Observation

    meter = client.meter()

    def threads_cb(_options: Any):
        return [Observation(_threads_count(), {})]

    def rss_cb(_options: Any):
        return [Observation(_memory_rss(), {})]

    def gc_cb(_options: Any):
        return [Observation(_gc_total(), {})]

    meter.create_observable_gauge(
        "process.runtime.python.threads",
        callbacks=[threads_cb],
        description="Number of threads that currently exist.",
        unit="{thread}",
    )
    meter.create_observable_gauge(
        "process.runtime.python.memory.rss",
        callbacks=[rss_cb],
        description="Resident set size of the Python process.",
        unit="By",
    )
    meter.create_observable_counter(
        "process.runtime.python.gc.count",
        callbacks=[gc_cb],
        description="Completed Python garbage collection cycles.",
        unit="{cycle}",
    )


def _register_dropped_counter(client: Client) -> Any:
    return client.meter().create_counter(
        "lazy_observe_sdk_dropped_telemetry",
        unit="{item}",
        description="Telemetry items dropped by bounded SDK delivery.",
    )


def noop_logger(config: Any) -> logging.Logger:
    import logging as _stdlib_logging

    logger = _stdlib_logging.getLogger("lazyobserve")
    if not logger.handlers:
        handler = logging.StreamHandler()
        handler.setFormatter(
            logging.Formatter("[%(asctime)s lazyobserve] %(levelname)s %(message)s")
        )
        logger.addHandler(handler)
    return logger


def degraded_client(config: Any, err: BaseException) -> Client:
    return Client(
        config=config,
        meter=_otel_metrics.NoOpMeterProvider(),
        tracer_provider=_otel_trace.NoOpTracerProvider(),
        logger_provider=None,
        logger=noop_logger(config),
        init_err=err,
    )


def init_client(config: Any) -> Client:
    """Create the SDK. On invalid configuration returns a safe degraded
    instance; inspect ``err()`` to report the construction failure."""
    from .config import InvalidConfigError, normalize_config, parsed_dsn

    try:
        normalized = normalize_config(config)
        parsed = parsed_dsn(normalized)
        return build_client(normalized, parsed)
    except (InvalidConfigError, ValueError) as exc:
        return degraded_client(config, exc)
    except Exception as exc:  # noqa: BLE001
        return degraded_client(config, exc)


ContextDict = Dict[str, str]
