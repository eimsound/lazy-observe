# Python SDK (Django + DRF)

The Python SDK provides explicit Django/DRF instrumentation for lazy-observe,
feature-aligned with the [Go/Fiber SDK](./go.md). Importing `lazyobserve`
installs no hooks, changes no global OpenTelemetry providers, and never calls
`set_meter_provider` / `set_tracer_provider` / `set_logger_provider`.

## Install

The package lives in the repository as `sdk/python` and is not yet published to
a registry. Consume the local checkout from another project:

```sh
pip install /absolute/path/to/lazy-observe/sdk/python
# or, with uv:
uv pip install -e /absolute/path/to/lazy-observe/sdk/python
```

The `django` extra adds Django + Django REST Framework (only needed for the
adapters and the example; the core SDK runs without them):

```sh
uv pip install -e "/abs/lazy-observe/sdk/python[django]"
```

## Initialize and shut down

```python
import lazyobserve

lo = lazyobserve.init({
    "dsn": "http://127.0.0.1:3100/p/default@TOKEN",
    "service_name": "checkout-api",
})
if lo.err():
    logging.error("lazy-observe initialization failed; telemetry is degraded", error=lo.err())
```

The DSN must be `http(s)://host[:port]/p/<slug>@<token>` without query or
fragment data; parsing rules match the Go SDK exactly. The token is sent only as
`Authorization: Bearer <token>`.

`init` registers the client as the registry's active instance so that
registry-bound integrations (`lazyobserve.DjangoMiddleware`,
`lazyobserve.drf.exception_handler`, `LazyObserveLogHandler`) resolve it.

Shut down before process exit to drain bounded queues and flush exporters:

```python
lo.shutdown(timeout=10.0)
```

## Configuration

| Field | Default | Behavior |
| --- | --- | --- |
| `dsn` | required | Gateway, project, and project API token. The token is a `lo_...` project API token and can only ingest: read routes answer it with `403 ingest_only_token`, so querying needs a `lou_...` user API token plus the `X-LO-Project` header ([deployment guide](../deployment.md#query-api-mcp-and-grafana)) |
| `service_name` | executable basename | OTel `service.name` and log field |
| `metrics`, `logs`, `traces` | all enabled | Signal matrix below |
| `sample_rate` | `1` | Parent-based head sampling; `(0, 1]` |
| `tee_to_stderr` | `true` | Also write records locally through the log handler |
| `capture_db_statement` | `false` | Include SQL text on DB spans; bound parameters never captured |
| `capture_llm_content` | `false` | Capture `gen_ai.prompt` / `gen_ai.completion`, truncated to 4 KiB |
| `release`, `environment` | empty | Deployment metadata attached to error events |
| `tags` | empty | Application tags deep-copied at initialization and attached to error events (subsequent caller mutations do not affect the running SDK) |
| `export_interval` | `5s` | Metrics/log export interval |
| `delivery` | zero-value defaults | Error queue bounds and retry budget (accepts `DeliveryConfig` or a nested `dict`) |
| `profiling` | `false` | **Experimental.** Opt in to [continuous profiling](#continuous-profiling); only samples on Linux with a free-threaded CPython 3.15+ whose GIL is off |
| `profiling_interval` | `60` (seconds) | Time between the starts of two 10-second profiling windows; `<= 0`, NaN and infinity fall back to 60; below 10 the windows run back to back |

When `lazyobserve.init` receives a `dict` or keyword arguments containing unrecognized top-level or `delivery.*` keys, the SDK logs a single warning on the `lazyobserve` logger listing the ignored fields instead of silently swallowing typos.

Signal matrix: all three of `metrics`/`logs`/`traces` false means all enabled;
once any is true the three values form an explicit enabled set. To disable all
telemetry, do not call `init`.

## Django

Add `lazyobserve.DjangoMiddleware` to `MIDDLEWARE`. The middleware:

- extracts W3C `traceparent` and creates a SERVER span (`url.path`,
  `http.request.method`, `url.scheme`, `server.address`, protocol version);
- records `http.server.request.duration` and `http.server.active_requests`;
- observes unhandled view exceptions in `process_exception` (logs with `exc_info`
  and queues the exception as a platform error event for Issue aggregation) and
  returns `None` so Django's native 500 pipeline (`got_request_exception`,
  `DEBUG` error page, custom `handler500`) stays intact; in direct non-Django
  middleware calls, `config.re_raise_on_500 = True` re-raises instead of
  returning HTTP 500;
- attaches low-cardinality `http.route` (resolver route, or `"unmatched"` when
  unmatched) and `http.response.status_code`, and updates the span name once the
  route resolves.

```python
MIDDLEWARE = [
    "lazyobserve.DjangoMiddleware",
    "django.middleware.common.CommonMiddleware",
]
```

A 5xx response that is directly written (no exception) remains visible in
metrics/traces but does not create an Issue — identical to Fiber.

### WSGI, ASGI, and streaming support boundaries

- **WSGI and ASGI**: `DjangoMiddleware` (and `lo.django_middleware()`) declares
  `sync_capable = True` and `async_capable = True`. Under ASGI, when Django
  supplies an async `get_response`, the middleware executes natively on the
  event loop without `sync_to_async` thread hopping.
- **`StreamingHttpResponse`**: both synchronous and asynchronous
  `StreamingHttpResponse` iterators are wrapped so the SERVER span,
  `http.server.request.duration`, and `http.server.active_requests` remain open
  until the stream finishes (or the response is closed). The OpenTelemetry
  request context is detached when the middleware returns the response object so
  it never leaks across requests.
- **Database wrapper**: `lo.django_db()` uses Django's synchronous
  `connection.execute_wrapper` and applies to synchronous ORM/cursor execution
  (including async views that run ORM queries inside `sync_to_async`).

## DRF

Configure the exception handler so API exceptions that resolve to 5xx are
queued for Issue aggregation with request context (method/route/status,
query-free URL):

```python
REST_FRAMEWORK = {
    "EXCEPTION_HANDLER": "lazyobserve.drf.exception_handler",
}
```

Non-API exceptions return `None` per DRF's exception-handler contract so
`APIView.raise_uncaught_exception` lets Django's 500 path (and the middleware)
capture them exactly once. 4xx API errors (validation, auth) never become
Issues.

## Logs

Configuring the handler in Django's `LOGGING` exports records as OTLP logs with
`service.name` and, when a span is active in the emitting context, `trace_id`
and `span_id` injected. Non-string/int/float/bool extras are ignored.

```python
LOGGING = {
    "version": 1,
    "disable_existing_loggers": False,
    "handlers": {"lazy_observe": {"class": "lazyobserve.LazyObserveLogHandler"}},
    "loggers": {"": {"handlers": ["lazy_observe"], "level": "INFO"}},
}
```

The handler also tees to stderr unless configured otherwise.

## Database

Use the explicit Django mechanism `connection.execute_wrapper` to instrument a
code block — no drivers are patched:

```python
with lo.django_db():
    ...  # ORM / cursor queries inside this block get DB CLIENT spans
```

Span attributes include `db.system.name` (derived from `ENGINE`); SQL text is
captured only when `capture_db_statement` is enabled, and bound parameter values
are never captured. Duration is recorded on `db.client.operation.duration`.

## LLM calls

Wrap an httpx transport explicitly — provider SDKs are not patched:

```python
import httpx
client = httpx.Client(transport=lo.llm_transport())
# pass through http_client= to openai / anthropic SDKs
```

Recognized call shapes: OpenAI-compatible `/chat/completions`, Anthropic
`/v1/messages`, Ollama `/api/chat`. Spans carry `gen_ai.operation.name`
(`chat`), `gen_ai.provider.name` (+ deprecated `gen_ai.system`),
request/response model, and input/output token counts from the response. When
the server reports no usage (for example streaming without
`stream_options.include_usage`), `gen_ai.usage.missing=true` is recorded and no
value is guessed. Enable `capture_llm_content` only after reviewing privacy
requirements; prompts/completions are truncated to 4 KiB and the application is
responsible for redaction.

Use `lo.llm_transport(client_kind="async")` for an
`httpx.AsyncBaseTransport`.

## Error reporting

Outside middleware/DRF, report handled errors explicitly:

```python
event_id = lo.capture_exception(exc, tags={"http.route": "/checkout"})
```

Error events carry the Python traceback (outermost-first, matching Go), release,
environment, tags, `service.name`, and the active `trace_id`/`span_id`.
Middleware adds method, route, status, and a query-free request URL. Request
bodies, headers, cookies, authorization values, and query strings are never
captured.

Sending is non-blocking through a bounded queue (batch up to 20 events, 100ms
flush).

## Sampling

`sample_rate` selects parent-based head sampling (`TraceIdRatioBased` under a
`ParentBased` sampler). Traces and logs use SDK-native bounded batch processors
(queue 2048, export batch 512); the SDK adds no duplicate queue.

## Runtime metrics

When metrics are enabled, the SDK collects:

- `process.runtime.python.threads` — `threading.active_count()`
- `process.runtime.python.memory.rss` — live resident set size from `/proc/self/statm` on Linux, falling back to `resource.getrusage` on other Unix platforms (returns `0` when unavailable, e.g. Windows)
- `process.runtime.python.gc.count` — cumulative GC cycles (`gc.get_stats()`)

## Continuous profiling

> **Experimental, off by default.** Continuous profiling in the Python SDK relies on
> undocumented CPython 3.15 internals and only works on Linux with the free-threaded
> build (`python3.15t`) running without the GIL. Everywhere else, turning it on logs one
> warning and samples nothing; the application is never affected.

Continuous profiling samples the call stacks of your own process on a fixed schedule and
uploads CPU profiles (`type=cpu`, gzip pprof) to the platform's `POST /api/profiles`. The
wire contract is the same one the Go SDK uses, so no server or protocol change is needed.

### Enable it

```python
lo = lazyobserve.init({
    "dsn": "http://127.0.0.1:3100/p/default@TOKEN",
    "service_name": "checkout-api",
    "profiling": True,          # default False
    "profiling_interval": 60,   # seconds between window starts; default 60
})
```

`init` returns immediately; the availability probe and the sampling loop run in a daemon
thread named `lazyobserve-profiling`. `lo.shutdown(timeout)` stops sampling at once. A
window that already ran at least 5 seconds is uploaded within the shutdown budget; a shorter
one is dropped. Prefork servers (gunicorn, uWSGI) are covered: each worker process restarts its own
sampler after `fork` and starts from a fresh window, so samples collected by a preloading
master are not uploaded again by every worker.

### Support range

Sampling happens only when **all** of these hold:

- the platform is Linux;
- the interpreter is CPython 3.15 or newer **and** a free-threaded build (`python3.15t`,
  `Py_GIL_DISABLED`);
- `sys._is_gil_enabled()` is `False` at run time.

Anything else, including 3.9–3.14, the regular 3.15 GIL build, a 3.15t whose GIL was switched
back on, macOS and Windows, only logs a warning (see [Warning codes](#warning-codes)) and
does not sample. The rest of the SDK still supports CPython 3.9 and newer.

Why the GIL matters: an in-process sampler must hold the GIL to read stacks, so on a GIL build
it can only see a thread while that thread waits or after a forced switch (5 ms by default).
Short CPU bursts, such as a typical web request, are then recorded as waiting (for example in
`select` or `recv`) instead of the code that burned the CPU. A misleading flame graph is worse
than none, so the SDK refuses to sample there. Covering GIL builds needs an out-of-process
sampler, which is not implemented.

The GIL can also come back while the process runs, for example when a newly imported C
extension does not declare free-threaded support. The SDK re-checks before every window and
stops for good if the GIL is on.

### What it samples

- CPU time only (`mode=cpu`), all threads of the process, at 100 Hz. The SDK's own sampling
  thread is excluded.
- Every `profiling_interval` seconds (default 60) one 10-second window is recorded, encoded
  and queued for upload. The window length is fixed. With `profiling_interval` below 10 the
  windows run back to back.
- Upload uses the bounded in-memory delivery queue (capacity 6) with retries. Non-transient 4xx
  uploads (excluding 408/429) and profiles exceeding the 10 MiB limit are not retried. Failed
  ticks and dropped uploads are counted in `Diagnostics().dropped_telemetry` with
  `signal="profile"` and reasons such as `sample_failed`, `queue_full`, `retry_exhausted`,
  `shutdown_budget` and `server_rejected`.
- The flame graph on the platform is the **sum of all processes and instances** of the service
  in the selected time range. Windows are not labeled with a pid or host, so you cannot split
  the view by process or instance.

### Measured overhead

Measured on CPython 3.15.2t, 100 Hz, stack depth 30, 4-second runs, three rounds each, on a shared
machine (load 1.3–4.5):

| Busy + idle threads | Sampling thread CPU, while a window runs |
| --- | ---: |
| 1 + 0 | 1.75% of one core |
| 1 + 15 | 3.60% of one core |
| 1 + 63 | 9.03% of one core |
| 4 + 0 | 2.77% of one core |

- The cost grows with the number of threads: each tick reads one `/proc/<pid>/task/<tid>/stat`
  per thread.
- It is only paid inside the 10-second windows, so with the default 60-second interval the
  average is about one sixth of the numbers above.
- Importing the sampler raises RSS by 15–19 MiB.
- Throughput of a busy thread changed by at most 3.2% averaged over three rounds and about
  5% in any single round, comparable to how much the unprofiled baseline itself varied
  between rounds (up to 4.8%), so it cannot be told apart from measurement noise.
- Turning profiling on adds about 9 ms to `init`, because the probe runs in the background.

These are measurements of one workload on one machine, not guarantees. Measure your own
service before enabling it broadly.

### Greenlets are not supported

When a thread is running inside a non-main greenlet, CPython's sampler cannot walk the stack:
the sample fails with `Incomplete sample: did not reach base frame`. This is independent of
the sampling mode and of 3.15 versus 3.15t. The SDK does not detect greenlet; you decide.

- **gevent, eventlet**: nearly every tick fails, so the failure rate exceeds 50% and profiling
  turns itself off with `sample_failure_rate`.
- **Libraries that use greenlets locally, such as SQLAlchemy asyncio**: the failure rate stays
  below the threshold (8–29% measured), so profiling stays on, but CPU spent inside the
  greenlet is **invisible** in the flame graph. In the measurement, query-path CPU that was
  36–44% of the truth showed up as 1–5% of the samples. Treat flame graphs of such services
  as incomplete.

### Warning codes

When profiling cannot run, the SDK logs one warning through the `lazyobserve` logger, at most
once per code per process (a forked child process may warn once again):

```
lazyobserve continuous profiling is off: reason=<code> (<detail>); see <link to this section>
```

Match on the code, not on the detail text. Nothing is sampled or uploaded after a warning, and
a stopped sampler never restarts within the same process, because the causes do not go away.

| Code | When it appears | What to do |
| --- | --- | --- |
| `python_too_old` | CPython older than 3.15 (3.9–3.14); `profiling.sampling` does not exist | Run on `python3.15t`, or leave `profiling` off |
| `unsupported_platform` | Not Linux (macOS, Windows) | Run the profiled service on Linux |
| `gil_build` | A regular build with the GIL, including the standard 3.15 | Use the free-threaded `python3.15t` build |
| `gil_enabled` | A free-threaded build, but `sys._is_gil_enabled()` is true: `PYTHON_GIL=1`, `-X gil=1`, or a C extension that does not support free threading switched the GIL on. Also raised when the GIL turns on between windows and sampling stops. | Remove the flag, or the extension that re-enables the GIL |
| `probe_failed` | The startup probe found that this interpreter's `profiling.sampling` does not match what the SDK expects: the import failed, the `sample()` signature or the CPU-mode constant changed, or a 0.1-second trial sample failed. Typical after a CPython upgrade, because the interface is undocumented. | Report the detail text; keep `profiling` off until the SDK supports that CPython |
| `too_many_failures` | Three windows in a row failed, counting both sampling and encoding errors. | Investigate the debug log of the `lazyobserve` logger |
| `sample_failure_rate` | More than 50% of the sampling ticks in one window failed (judged only with at least 100 attempts, about 1 s at 100 Hz). Typical of gevent and eventlet. | Do not use profiling together with greenlet-based concurrency |
| `internal_error` | Anything else unexpected, for example the profiling thread could not start or an unclassified stop reason. The detail carries the exception. | Report the detail text |

Application behavior is never changed by any of these: the SDK never raises into your code,
and a failing sampler only turns itself off.

## Delivery reliability

Error-event delivery is in-memory, bounded, non-blocking, and
at-least-once. Defaults:

| Field | Default |
| --- | ---: |
| error queue capacity | 100 events |
| max attempts | 3 |
| retry initial | 100ms |
| retry max | 1s |

A full queue drops new items and logs a local warning; retry exhaustion drops
the batch after the finite budget. `Diagnostics().dropped_telemetry` exposes the
process-local total, and metrics-enabled clients export
`lazy_observe_sdk_dropped_telemetry` with `signal`/`reason` attributes. Dropped
error events are counted with `signal="error"` and the reasons `queue_full`,
`retry_exhausted`, `shutdown_budget` and `server_rejected` (events still queued or
retrying when the `shutdown` budget runs out, captured after `shutdown`, rejected
with non-transient 4xx, or exceeding the 200 KiB per-event limit), the same as the
Go SDK. Once the budget has run out the queue starts no new request: neither
the first attempt for a batch it already holds nor a retry whose delay has just
ended. That batch is counted as `shutdown_budget` too; a request already in
flight is left to finish. There is no disk spool.

## Parity matrix (Go/Fiber -> Python/Django+DRF)

| Go SDK | Python SDK |
| --- | --- |
| `lazyobserve.Init(Config)` | `lazyobserve.init(cfg)` |
| `lo.Err()` / degraded instance | `lo.err()` / `lo.degraded` |
| `signalMatrix` all-false default | same rule on `metrics`/`logs`/`traces` |
| `lo.Fiber()` | `lazyobserve.DjangoMiddleware` |
| Fiber 5xx capture | `lazyobserve.drf.exception_handler` |
| `lo.Slog()` correlating handler | `LazyObserveLogHandler` |
| `lo.WrapDB(driver, dsn)` | `lo.django_db()` context manager |
| `lo.LLMTransport(base)` | `lo.llm_transport(base=...)` |
| `CaptureException(ctx, err)` | `lo.capture_exception(exc, ...)` |
| `process.runtime.go.*` | `process.runtime.python.*` |
| pprof cpu+heap+goroutine | experimental, opt-in CPU profile only (`profiling=True`), Linux + free-threaded CPython 3.15 without the GIL; see [Continuous profiling](#continuous-profiling) |
| `lo.Shutdown(ctx)` | `lo.shutdown(timeout)` |
| `Diagnostics().DroppedTelemetry` | `Diagnostics().dropped_telemetry` |
| `django_db` / `LLM` content flags | `capture_db_statement` / `capture_llm_content` |

## Direct dependencies and licenses

| Dependency | Use | License |
| --- | --- | --- |
| `opentelemetry-api` / `-sdk` | API, SDK (`>=1.30,<2.0`) | Apache-2.0 |
| `opentelemetry-exporter-otlp-proto-http` | OTLP/HTTP exporters (`>=1.30,<2.0`) | Apache-2.0 |
| `httpx` | Internal telemetry HTTP transport and LLM transport wrapping (`>=0.24,<1.0`) | BSD-3-Clause |
| `Django` | middleware / DB wrapper (optional) | BSD-3-Clause |
| `djangorestframework` | DRF handler (optional) | BSD-3-Clause |
| `protobuf` | OTLP protobuf (pinned by `opentelemetry-exporter-otlp-proto-http`) | BSD-3-Clause |
