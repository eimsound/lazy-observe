# lazy-observe Python SDK

Explicit Django + DRF instrumentation for the lazy-observe observability
platform, feature-aligned with the Go Fiber SDK: HTTP metrics/traces and 5xx
Issue capture, correlated logs, explicit DB spans, and LLM call tracking.
(*CPU/pprof profiling is not implemented in Python — no pprof-producing CPU
sampler supports modern CPython; see sdk/docs/sdk/python.md.*) Importing it
installs no hooks and changes no global OpenTelemetry providers.

## Three-line setup

```python
import lazyobserve

lo = lazyobserve.init({"dsn": "http://host:3100/p/myapp@token", "service_name": "checkout-api"})
```

Add the middleware to Django `settings.py`:

```python
MIDDLEWARE = ["lazyobserve.DjangoMiddleware", ...]
REST_FRAMEWORK = {
    "EXCEPTION_HANDLER": "lazyobserve.drf.exception_handler",
}
LOGGING = {
    "version": 1,
    "disable_existing_loggers": False,
    "handlers": {"lazy_observe": {"class": "lazyobserve.LazyObserveLogHandler"}},
    "loggers": {"": {"handlers": ["lazy_observe"], "level": "INFO"}},
}
```

Shut the SDK down cleanly on process exit:

```python
lo.shutdown(timeout=10.0)
```

`init` never raises. Check `lo.err()` — on invalid configuration `lo` is a safe
degraded instance.

## Signals

| Integration | Parity with Go SDK |
| --- | --- |
| `lazyobserve.DjangoMiddleware` | `lo.Fiber()` — server span, `http.server.request.duration`, `http.server.active_requests`, traceparent extraction, unhandled-exception → HTTP 500 + error event + Issue |
| `lazyobserve.drf.exception_handler` | Fiber 5xx capture — API exceptions resolving to 5xx are queued for Issue aggregation |
| `LazyObserveLogHandler` | `lo.Slog()` — injects `service.name`, `trace_id`, `span_id`, exports OTLP logs |
| `lo.django_db()` (context manager) | `lo.WrapDB` — explicit `connection.execute_wrapper`, DB CLIENT spans, `db.client.operation.duration`, optional SQL capture |
| `lo.llm_transport()` (httpx transport) | `lo.LLMTransport` — OpenAI/Anthropic/Ollama spans with `gen_ai.*` attributes and usage |
| `lo.capture_exception(exc)` | `CaptureException(ctx, err)` — error event with stack + trace correlation |
| runtime metrics | `process.runtime.python.threads` / `.memory.rss` / `.gc.count` |
| CPU profiling | not implemented (no pprof-producing sampler for modern CPython; see docs) |
| `lo.shutdown()` / diagnostics | drains bounded queues, flushes OTel, returns last failure |

All signals are opt-in switches (`metrics`/`logs`/`traces` follow the Go matrix:
all three false means all enabled). `Diagnostics().dropped_telemetry` exposes
bounded-delivery losses; `lazy_observe_sdk_dropped_telemetry` is exported as a
low-cardinality metric.

See the `examples/django/` project for a runnable Django + DRF app, and
`docs/sdk/python.md` for the full configuration table and parity notes.

## Development

```sh
uv venv --python 3.11 .venv
uv pip install -e ".[test]"
.venv/bin/python -m pytest tests/
LO_E2E_PLATFORM_URL=http://127.0.0.1:3100 \
LO_E2E_TOKEN=lo_... \
LO_E2E_ADMIN_PASSWORD=... \
  ./e2e.sh
```

`mise run sdk-python` and the CI `Python SDK` job only run pytest. The real-platform
journey is `./e2e.sh`. The standard way to run it is `mise run sdk-journeys` from the
repository root: it starts a throwaway platform (temporary data dir, free ports, known
admin password) and runs the Go, Node, Python and Browser journeys in turn; the CI
`SDK journeys` job runs the same task. To run only this journey, point it at an
already running lazy-observe platform:

- `LO_E2E_PLATFORM_URL`: base URL of the running platform.
- `LO_E2E_TOKEN`: the `default` project's API token (`lo_…`). It only builds the
  ingest DSN; project tokens are ingest-only (ADR 0010) and get `403` on read routes.
- `LO_E2E_ADMIN_PASSWORD`: the `admin` password. Every read in the journey uses the
  admin session cookie plus `X-LO-Project: default`.
- `LO_E2E_PYTHON` (optional): interpreter with the `[test]` extras installed;
  defaults to `.venv/bin/python`.
