"""Test helpers: build a Client backed by in-memory OTel providers so no
network traffic is generated."""

from __future__ import annotations

import logging

from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from lazyobserve.client import Client
from lazyobserve.config import Config, DeliveryConfig, normalize_config, parsed_dsn


def make_client(service_name: str = "test-service", **overrides) -> Client:
    fields: dict = {
        "dsn": "http://127.0.0.1:3100/p/default@TOKEN",
        "service_name": service_name,
        "metrics": True,
        "traces": True,
        "logs": True,
        "delivery": DeliveryConfig(error_queue_capacity=100, max_attempts=1, retry_initial=0.0),
    }
    fields.update(overrides)
    cfg = Config(**fields)
    cfg = normalize_config(cfg)
    parsed = parsed_dsn(cfg)

    exporter = InMemorySpanExporter()
    tracer_provider = TracerProvider()
    tracer_provider.add_span_processor(SimpleSpanProcessor(exporter))
    meter_provider = MeterProvider()

    client = Client(
        config=cfg,
        meter=meter_provider,
        tracer_provider=tracer_provider,
        logger_provider=None,
        logger=logging.getLogger("lazyobserve-test"),
        init_err=None,
    )
    client._test_span_exporter = exporter  # type: ignore[attr-defined]
    return client


def last_span(client: Client):
    spans = client._test_span_exporter.get_finished_spans()  # type: ignore[attr-defined]
    return spans[-1] if spans else None
