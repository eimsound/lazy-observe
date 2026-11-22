# lazy-observe Python SDK

Explicit Django + DRF instrumentation for the lazy-observe observability
platform, feature-aligned with the Go Fiber SDK: HTTP metrics/traces and 5xx
Issue capture, correlated logs, explicit DB spans, and LLM call tracking.
Experimental, opt-in [continuous profiling](#continuous-profiling-experimental) is
available on a narrow set of interpreters. Importing the package installs no hooks
and changes no global OpenTelemetry providers.

## Three-line setup

```python
import lazy_observe

lo = lazy_observe.init({"dsn": "http://host:3100/p/myapp@token", "service_name": "checkout-api"})
```

Add the middleware to Django `settings.py`:

```python
MIDDLEWARE = ["lazy_observe.DjangoMiddleware", ...]
REST_FRAMEWORK = {
    "EXCEPTION_HANDLER": "lazy_observe.drf.exception_handler",
}
LOGGING = {
    "version": 1,
    "disable_existing_loggers": False,
    "handlers": {"lazy_observe": {"class": "lazy_observe.LazyObserveLogHandler"}},
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
| `lazy_observe.DjangoMiddleware` | `lo.Fiber()` — server span, `http.server.request.duration`, `http.server.active_requests`, traceparent extraction, unhandled-exception → HTTP 500 + error event + Issue |
| `lazy_observe.drf.exception_handler` | Fiber 5xx capture — API exceptions resolving to 5xx are queued for Issue aggregation |
| `LazyObserveLogHandler` | `lo.Slog()` — injects `service.name`, `trace_id`, `span_id`, exports OTLP logs |
| `lo.django_db()` (context manager) | `lo.WrapDB` — explicit `connection.execute_wrapper`, DB CLIENT spans, `db.client.operation.duration`, optional SQL capture |
| `lo.llm_transport()` (httpx transport) | `lo.LLMTransport` — OpenAI/Anthropic/Ollama spans with `gen_ai.*` attributes and usage |
| `lo.capture_exception(exc)` | `CaptureException(ctx, err)` — error event with stack + trace correlation |
| runtime metrics | `process.runtime.python.threads` / `.memory.rss` / `.gc.count` |
| `profiling=True` (experimental) | Continuous CPU profiling (pprof, `type=cpu`), off by default; Linux + free-threaded CPython 3.15 without the GIL only; see below |
| `lo.shutdown()` / diagnostics | drains bounded queues, flushes OTel, returns last failure |

All signals are opt-in switches (`metrics`/`logs`/`traces` follow the Go matrix:
all three false means all enabled). `Diagnostics().dropped_telemetry` exposes
bounded-delivery losses; `lazy_observe_sdk_dropped_telemetry` is exported as a
low-cardinality metric.

See the `examples/django/` project for a runnable Django + DRF app, and
`docs/sdk/python.md` for the full configuration table and parity notes.

## Continuous profiling (experimental)

Off by default. Opt in with `profiling=True`:

```python
lo = lazy_observe.init({
    "dsn": "http://host:3100/p/myapp@token",
    "service_name": "checkout-api",
    "profiling": True,
    "profiling_interval": 60,  # seconds between window starts (default 60)
})
```

- **Supported**: Linux, CPython 3.15+ **free-threaded** build (`python3.15t`), and
  `sys._is_gil_enabled()` is `False` at run time. The rest of the SDK still supports CPython 3.9+.
- **Everywhere else it only logs one warning and does not sample**: CPython 3.9–3.14, the
  regular 3.15 GIL build, a 3.15t whose GIL was switched back on (`PYTHON_GIL=1`, `-X gil=1`,
  or a C extension that needs the GIL), macOS and Windows. The warning carries a stable
  `reason=<code>` and a link to the [per-code explanation](https://github.com/eimsound/lazy-observe/blob/main/docs/sdk/python.md#continuous-profiling).
  Sampling also stops for good if the GIL comes back, or the sampler keeps failing.
- **Rhythm**: every `profiling_interval` seconds (default 60) one 10-second window is sampled
  at 100 Hz (CPU time, all threads) and uploaded to `/api/profiles`.
- **Overhead** (3.15.2t, measured): the sampling thread uses about 1.75% of one core with
  one thread and about 9% with 64 threads while a window runs; averaged over the default
  60-second interval about one sixth of that. RSS grows by 15–19 MiB.
- **No greenlet**: code running inside a greenlet is invisible to the sampler. gevent and
  eventlet make more than 50% of the ticks fail, so profiling turns itself off
  (`sample_failure_rate`). Libraries that use greenlets locally, such as SQLAlchemy asyncio,
  do not trigger that, but the CPU spent inside their greenlets will be missing from the flame graph.
- **The flame graph is a sum** over all processes and instances of the service; it cannot be split by
  process or instance.

## Development

The interpreter and the virtualenv come from [uv](https://docs.astral.sh/uv/); no system Python
or pip is involved. The standard entry is the repository's mise task, which pins uv, creates
`sdk/python/.venv`, installs the `[test]` extras and runs pytest. `LO_SDK_PYTHON_VERSION`
picks the interpreter (default `3.13`; the CI matrix uses `3.9`, `3.13`, `3.15`, `3.15t`
and the non-blocking pre-release `3.16`):

```sh
mise run sdk-python
LO_SDK_PYTHON_VERSION=3.15t mise run sdk-python
```

The equivalent by hand, from `sdk/python`:

```sh
uv venv --python 3.15t .venv
uv pip install --python .venv/bin/python -e ".[test]"
.venv/bin/python -m pytest -q tests
```

Only 3.15t actually samples; on 3.9, 3.13 and the 3.15 GIL build the profiling tests check that the
switch warns and samples nothing, and the real-sampler tests are skipped.

`pyproject.toml` sets `[tool.uv] exclude-newer = "7 days"`: uv only resolves package versions that
have been on PyPI for at least seven days, the same minimum release age as the repository's
`scripts/release-age.env`. It applies to `uv pip install` and to the build backend that `uv build`
resolves. Release artifacts are built the same way; the local dry run verifies the version in
`pyproject.toml` against the tag and runs `uv build` with a uv-managed Python 3.13
(`--managed-python`, never the system interpreter) into `sdk/python/dist`:

```sh
SDK_TAG=sdk-python-v0.1.0 mise run sdk-python-release
```

To run the real-platform journey:

```sh
LO_E2E_PLATFORM_URL=http://127.0.0.1:3100 \
LO_E2E_TOKEN=lo_... \
LO_E2E_ADMIN_PASSWORD=... \
  ./e2e.sh
```

`mise run sdk-python` and the CI `Python SDK` matrix only run pytest. The real-platform
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
