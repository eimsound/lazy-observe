# Python SDK for Django and DRF

The Python SDK provides explicit Django and Django REST Framework (DRF) instrumentation for lazy-observe, aligned with the [Go/Fiber SDK](go.md). Importing `lazy_observe` installs no hooks, changes no global OpenTelemetry providers, and never calls `set_meter_provider`, `set_tracer_provider`, or `set_logger_provider`.

## Install

The package supports Python 3.9 and newer. It lives in the repository as `sdk/python` and is not yet published to a package registry. Install the local checkout from another project:

```sh
pip install /absolute/path/to/lazy-observe/sdk/python
# or, with uv:
uv pip install -e /absolute/path/to/lazy-observe/sdk/python
```

The `django` extra adds Django and Django REST Framework. You need it only for the framework adapters and the example; the core SDK runs without it:

```sh
uv pip install -e "/abs/lazy-observe/sdk/python[django]"
```

## Initialize and shut down

```python
import lazy_observe

lo = lazy_observe.init({
    "dsn": "http://127.0.0.1:3100/p/default@TOKEN",
    "service_name": "checkout-api",
})
if lo.err():
    logging.error("lazy-observe initialization failed; telemetry is degraded", error=lo.err())
```

The DSN must match `http(s)://host[:port]/p/<slug>@<token>` with no query string or fragment. Parsing rules match the Go SDK. The SDK sends the token only in `Authorization: Bearer <token>`.

`init` registers the client as the active registry instance so that registry-bound integrations (`lazy_observe.DjangoMiddleware`, `lazy_observe.drf.exception_handler`, and `LazyObserveLogHandler`) resolve it.

Shut down before process exit to drain bounded queues and flush exporters:

```python
lo.shutdown(timeout=10.0)
```

## Configuration

| Field | Default | Behavior |
| --- | --- | --- |
| `dsn` | required | Gateway, project, and project API token. This `lo_...` project API token can only ingest. Read routes reject it with `403 ingest_only_token`. Querying requires a `lou_...` user API token and the `X-LO-Project` header. See the [deployment guide](../deployment.md#query-api-mcp-and-grafana). |
| `service_name` | executable basename | OpenTelemetry `service.name` and log field. |
| `metrics`, `logs`, `traces` | all enabled | Follows the signal matrix below. |
| `sample_rate` | `1` | Parent-based head sampling, `(0, 1]`. |
| `compression` | `"none"` | OTLP/HTTP payload compression (`""`, `"none"`, or `"gzip"`). When empty, honors `OTEL_EXPORTER_OTLP_<SIGNAL>_COMPRESSION` then `OTEL_EXPORTER_OTLP_COMPRESSION`, defaulting to `"none"`. Explicit config overrides env vars. |
| `tee_to_stderr` | `true` | Also write records locally through the log handler. |
| `capture_db_statement` | `false` | Include SQL text on database spans. Bound parameters are never captured. |
| `capture_llm_content` | `false` | Capture `gen_ai.prompt` and `gen_ai.completion`, truncated to 4 KiB. |
| `release`, `environment` | empty | Deployment metadata attached to error events. |
| `tags` | empty | Application tags deep-copied at initialization and attached to error events. Later caller mutations do not affect the running SDK. |
| `export_interval` | `5s` | Metrics and log export interval. |
| `delivery` | zero-value defaults | Error queue bounds and retry budget. Accepts `DeliveryConfig` or a nested `dict`. |
| `profiling` | `false` | **Experimental.** Opt in to [continuous profiling](#continuous-profiling). Samples only on Linux with free-threaded CPython 3.15 or newer when the GIL is off. |
| `profiling_interval` | `60` seconds | Time between the starts of two 10-second profiling windows. Values `<= 0`, NaN, and infinity fall back to `60`. Below `10`, windows run back to back. |

When `lazy_observe.init` receives a `dict` or keyword arguments containing unrecognized top-level or `delivery.*` keys, the SDK logs a single warning on the `lazy_observe` logger listing the ignored fields instead of silently ignoring typos.

For the signal matrix, setting all three of `metrics`, `logs`, and `traces` to `false` enables all three signals. Once any flag is `true`, the three values form an explicit enabled set. To disable all telemetry, do not call `init`.

## Django

Add `lazy_observe.DjangoMiddleware` to `MIDDLEWARE`. The middleware does the following:

- Extracts W3C `traceparent` and creates a SERVER span with `url.path`, `http.request.method`, `url.scheme`, `server.address`, and protocol version.
- Records `http.server.request.duration` and `http.server.active_requests`.
- Observes unhandled view exceptions in `process_exception`, logs them with `exc_info`, queues each exception as a platform error event for Issue aggregation, and returns `None` so Django's native 500 pipeline (`got_request_exception`, `DEBUG` error page, and custom `handler500`) stays intact. In direct non-Django middleware calls, setting `config.re_raise_on_500 = True` re-raises instead of returning HTTP 500.
- Attaches low-cardinality `http.route` (the resolver route, or `"unmatched"` when unmatched) and `http.response.status_code`, and updates the span name once the route resolves.

```python
MIDDLEWARE = [
    "lazy_observe.DjangoMiddleware",
    "django.middleware.common.CommonMiddleware",
]
```

A 5xx response written directly without an exception appears in metrics and traces, and does not create an Issue. This matches Fiber.

### WSGI, ASGI, and streaming support boundaries

- **WSGI and ASGI.** `DjangoMiddleware` and `lo.django_middleware()` declare `sync_capable = True` and `async_capable = True`. Under ASGI, when Django supplies an async `get_response`, the middleware executes natively on the event loop without `sync_to_async` thread hopping.
- **`StreamingHttpResponse`.** The middleware wraps both synchronous and asynchronous `StreamingHttpResponse` iterators so the SERVER span, `http.server.request.duration`, and `http.server.active_requests` stay open until the stream finishes or the response closes. The OpenTelemetry request context detaches when the middleware returns the response object so it never leaks across requests.
- **Database wrapper.** `lo.django_db()` uses Django's synchronous `connection.execute_wrapper` and applies to synchronous ORM and cursor execution, including async views that run ORM queries inside `sync_to_async`.

## DRF

Configure the exception handler so API exceptions resolving to 5xx are queued for Issue aggregation with request context (method, route, status, and query-free URL):

```python
REST_FRAMEWORK = {
    "EXCEPTION_HANDLER": "lazy_observe.drf.exception_handler",
}
```

The handler returns `None` for non-API exceptions per DRF's exception-handler contract, so `APIView.raise_uncaught_exception` lets Django's 500 path and the middleware capture them once. 4xx API errors, such as validation or authentication errors, never become Issues.

## Logs

Configuring the handler in Django's `LOGGING` exports records as OTLP logs with `service.name`. When a span is active in the emitting context, the handler injects `trace_id` and `span_id`. It ignores extras whose values are not string, int, float, or bool.

```python
LOGGING = {
    "version": 1,
    "disable_existing_loggers": False,
    "handlers": {"lazy_observe": {"class": "lazy_observe.LazyObserveLogHandler"}},
    "loggers": {"": {"handlers": ["lazy_observe"], "level": "INFO"}},
}
```

The handler also tees records to stderr unless configured otherwise.

## Database

Use Django's explicit `connection.execute_wrapper` mechanism to instrument a code block without patching drivers:

```python
with lo.django_db():
    ...  # ORM / cursor queries inside this block get DB CLIENT spans
```

Span attributes include `db.system.name`, derived from `ENGINE`. The SDK captures SQL text only when `capture_db_statement` is enabled, and never captures bound parameter values. It records duration on `db.client.operation.duration`.

## LLM calls

Wrap an `httpx` transport explicitly without patching provider SDKs:

```python
import httpx
client = httpx.Client(transport=lo.llm_transport())
# pass through http_client= to openai / anthropic SDKs
```

The wrapper recognizes OpenAI-compatible `/chat/completions`, Anthropic `/v1/messages`, and Ollama `/api/chat`. Spans carry `gen_ai.operation.name` (`chat`), `gen_ai.provider.name` plus deprecated `gen_ai.system`, request and response models, and input and output token counts from the response. When the server reports no usage, for example during streaming without `stream_options.include_usage`, the wrapper records `gen_ai.usage.missing=true` and does not guess a value. Enable `capture_llm_content` only after reviewing your privacy requirements. Prompts and completions are truncated to 4 KiB, and the application is responsible for redaction.

Use `lo.llm_transport(client_kind="async")` for an `httpx.AsyncBaseTransport`.

## Error reporting

Outside middleware and DRF, report handled errors explicitly:

```python
event_id = lo.capture_exception(exc, tags={"http.route": "/checkout"})
```

Error events carry the Python traceback in outermost-first order, matching Go, along with release, environment, tags, `service.name`, and the active `trace_id` and `span_id`. Middleware adds method, route, status, and a query-free request URL. The SDK never captures request bodies, headers, cookies, authorization values, or query strings.

Delivery is non-blocking through a bounded queue that batches up to 20 events with a 100 ms flush interval.

## Sampling

`sample_rate` selects parent-based head sampling using `TraceIdRatioBased` under a `ParentBased` sampler. Traces and logs use SDK-native bounded batch processors with a queue size of 2048 and an export batch size of 512. The SDK adds no duplicate queue.

## Runtime metrics

When metrics are enabled, the SDK collects three runtime metrics:

- `process.runtime.python.threads` reads `threading.active_count()`.
- `process.runtime.python.memory.rss` reads live resident set size from `/proc/self/statm` on Linux, falls back to `resource.getrusage` on other Unix platforms, and returns `0` when unavailable, such as on Windows.
- `process.runtime.python.gc.count` reads cumulative GC cycles from `gc.get_stats()`.

## Continuous profiling

> **Experimental, off by default.** Continuous profiling in the Python SDK relies on undocumented CPython 3.15 internals and works only on Linux with the free-threaded build (`python3.15t`) running without the GIL. On all other platforms and builds, turning it on logs one warning and samples nothing. Application behavior is never affected.

Continuous profiling samples the call stacks of your process on a fixed schedule and uploads CPU profiles (`type=cpu`, gzip pprof) to `POST /api/profiles`. It uses the same wire contract as the Go SDK, so no server or protocol change is needed.

### Enable continuous profiling

```python
lo = lazy_observe.init({
    "dsn": "http://127.0.0.1:3100/p/default@TOKEN",
    "service_name": "checkout-api",
    "profiling": True,          # default False
    "profiling_interval": 60,   # seconds between window starts; default 60
})
```

`init` returns immediately. The availability probe and the sampling loop run in a daemon thread named `lazy_observe-profiling`. `lo.shutdown(timeout)` stops sampling at once. The SDK uploads a window that has already run for at least 5 seconds within the shutdown budget and drops any shorter window. Prefork servers such as gunicorn and uWSGI are supported. Each worker process restarts its own sampler after `fork` with a fresh window, so samples collected by a preloading master are not uploaded again by every worker.

### Support range

Sampling runs only when all of these conditions hold:

- The platform is Linux.
- The interpreter is CPython 3.15 or newer and uses the free-threaded build (`python3.15t`, `Py_GIL_DISABLED`).
- `sys._is_gil_enabled()` is `False` at runtime.

Every other environment, including CPython 3.9 to 3.14, the standard 3.15 GIL build, a 3.15t build whose GIL was switched back on, macOS, and Windows, logs a warning and does not sample. See [Warning codes](#warning-codes). The rest of the SDK still supports CPython 3.9 and newer.

An in-process sampler must hold the GIL to read stacks. On a GIL build, it can observe a thread only while that thread waits or after a forced switch (5 ms by default). Short CPU bursts, such as a typical web request, appear as waiting in calls like `select` or `recv` instead of the code using CPU. Because a misleading flame graph is worse than none, the SDK refuses to sample on GIL builds. Covering GIL builds requires an out-of-process sampler, which is not implemented.

The GIL can also return while the process runs, for example when the process imports a C extension that does not declare free-threaded support. The SDK checks again before every window and stops permanently if the GIL is enabled.

### What it samples

- The profiler samples CPU time only (`mode=cpu`) across all threads of the process at 100 Hz, excluding the SDK's own sampling thread.
- Every `profiling_interval` seconds (default 60), the profiler records one fixed 10-second window, encodes it, and queues it for upload. When `profiling_interval` is below 10, windows run back to back.
- Uploads use a bounded in-memory delivery queue with capacity 6 and retries. The SDK does not retry non-transient 4xx responses (excluding 408 and 429) or profiles exceeding the 10 MiB limit. The SDK counts failed ticks and dropped uploads in `Diagnostics().dropped_telemetry` with `signal="profile"` and reasons `sample_failed`, `queue_full`, `retry_exhausted`, `shutdown_budget`, and `server_rejected`.
- The flame graph on the platform is the sum of all processes and instances of the service in the selected time range. Because windows carry no pid or host label, you cannot split the view by process or instance.

### Measured overhead

Measured on CPython 3.15.2t at 100 Hz, stack depth 30, 4-second runs, three rounds each, on a shared machine with load 1.3 to 4.5:

| Busy + idle threads | Sampling thread CPU, while a window runs |
| --- | ---: |
| 1 + 0 | 1.75% of one core |
| 1 + 15 | 3.60% of one core |
| 1 + 63 | 9.03% of one core |
| 4 + 0 | 2.77% of one core |

- The cost grows with thread count because each tick reads one `/proc/<pid>/task/<tid>/stat` file per thread.
- The sampling cost applies only during the 10-second windows, so at the default 60-second interval the average is about one sixth of the numbers above.
- Importing the sampler increases RSS by 15 to 19 MiB.
- Throughput of a busy thread changed by at most 3.2% averaged over three rounds and about 5% in any single round. That range matches the variance of the unprofiled baseline between rounds (up to 4.8%), so it cannot be separated from measurement noise.
- Turning profiling on adds about 9 ms to `init`, because the probe runs in the background.

These numbers reflect one workload on one machine and are not guarantees. Measure your own service before enabling profiling broadly.

### Greenlets are not supported

When a thread runs inside a non-main greenlet, CPython's sampler cannot walk the stack and fails with `Incomplete sample: did not reach base frame`. This occurs regardless of sampling mode and on both 3.15 and 3.15t. The SDK does not detect greenlet; you decide whether to enable profiling.

- **gevent and eventlet.** Nearly every tick fails, so the failure rate exceeds 50% and profiling turns itself off with `sample_failure_rate`.
- **Libraries that use greenlets locally, such as SQLAlchemy asyncio.** The failure rate stays below the threshold (8% to 29% measured), so profiling stays on, but CPU time spent inside the greenlet does not appear in the flame graph. In testing, query-path CPU that was 36% to 44% of actual CPU time appeared as 1% to 5% of samples. Treat flame graphs of those services as incomplete.

### Warning codes

When profiling cannot run, the SDK logs one warning through the `lazy_observe` logger, at most once per code per process. A forked child process may warn once again:

```
lazy_observe continuous profiling is off: reason=<code> (<detail>); see <link to this section>
```

Match on the code, not on the detail text. The SDK samples and uploads nothing after a warning. A stopped sampler never restarts within the same process because the underlying causes do not clear.

| Code | When it appears | What to do |
| --- | --- | --- |
| `python_too_old` | CPython is older than 3.15 (3.9 to 3.14), so `profiling.sampling` does not exist. | Run on `python3.15t`, or leave `profiling` off. |
| `unsupported_platform` | Platform is not Linux (macOS or Windows). | Run the profiled service on Linux. |
| `gil_build` | Regular build with the GIL, including standard 3.15. | Use the free-threaded `python3.15t` build. |
| `gil_enabled` | Free-threaded build where `sys._is_gil_enabled()` is true because of `PYTHON_GIL=1`, `-X gil=1`, or a C extension that does not support free threading. Also raised when the GIL turns on between windows and sampling stops. | Remove the flag or the extension that re-enables the GIL. |
| `probe_failed` | The startup probe found that this interpreter's `profiling.sampling` does not match what the SDK expects. The import failed, the `sample()` signature or CPU-mode constant changed, or a 0.1-second trial sample failed. This is typical after a CPython upgrade because the interface is undocumented. | Report the detail text and keep `profiling` off until the SDK supports that CPython version. |
| `too_many_failures` | Three windows in a row failed, counting both sampling and encoding errors. | Inspect the debug log of the `lazy_observe` logger. |
| `sample_failure_rate` | More than 50% of sampling ticks in one window failed, evaluated only with at least 100 attempts (about 1 second at 100 Hz). Typical of gevent and eventlet. | Do not combine profiling with greenlet-based concurrency. |
| `internal_error` | Another unexpected failure occurred, such as the profiling thread failing to start or an unclassified stop reason. The detail carries the exception. | Report the detail text. |

None of these conditions change application behavior. The SDK never raises into your code, and a failing sampler turns itself off.

## Delivery reliability

Error-event delivery is in-memory, bounded, non-blocking, and at-least-once. Defaults:

| Field | Default |
| --- | ---: |
| error queue capacity | 100 events |
| max attempts | 3 |
| retry initial | 100ms |
| retry max | 1s |

A full queue drops new items and logs a local warning. Retry exhaustion drops the batch after the finite budget runs out. `Diagnostics().dropped_telemetry` exposes the process-local total, and metrics-enabled clients export `lazy_observe_sdk_dropped_telemetry` with `signal` and `reason` attributes.

The SDK counts dropped error events with `signal="error"` and the reasons `queue_full`, `retry_exhausted`, `shutdown_budget`, and `server_rejected`, matching the Go SDK. Those reasons cover events still queued or retrying when the `shutdown` budget runs out, events captured after `shutdown`, batches rejected with non-transient 4xx responses, and events exceeding the 200 KiB per-event limit. Once the shutdown budget runs out, the queue starts no new request, neither the first attempt for a held batch nor a retry whose delay just ended. That batch also counts as `shutdown_budget`. Any request already in flight finishes. There is no disk spool.

## Parity with the Go SDK

| Go SDK | Python SDK |
| --- | --- |
| `lazyobserve.Init(Config)` | `lazy_observe.init(cfg)` |
| `lo.Err()` / degraded instance | `lo.err()` / `lo.degraded` |
| `signalMatrix` all-false default | same rule on `metrics`/`logs`/`traces` |
| `lo.Fiber()` | `lazy_observe.DjangoMiddleware` |
| Fiber 5xx capture | `lazy_observe.drf.exception_handler` |
| `lo.Slog()` correlating handler | `LazyObserveLogHandler` |
| `lo.WrapDB(driver, dsn)` | `lo.django_db()` context manager |
| `lo.LLMTransport(base)` | `lo.llm_transport(base=...)` |
| `CaptureException(ctx, err)` | `lo.capture_exception(exc, ...)` |
| `process.runtime.go.*` | `process.runtime.python.*` |
| pprof CPU, heap, and goroutine | experimental, opt-in CPU profile only (`profiling=True`), on Linux with free-threaded CPython 3.15 or newer and the GIL disabled; see [Continuous profiling](#continuous-profiling) |
| `lo.Shutdown(ctx)` | `lo.shutdown(timeout)` |
| `Diagnostics().DroppedTelemetry` | `Diagnostics().dropped_telemetry` |
| `django_db` / `LLM` content flags | `capture_db_statement` / `capture_llm_content` |

## Direct dependencies and licenses

| Dependency | Use | License |
| --- | --- | --- |
| `opentelemetry-api` / `-sdk` | API, SDK (`>=1.30,<2.0`) | Apache-2.0 |
| `opentelemetry-exporter-otlp-proto-http` | OTLP/HTTP exporters (`>=1.30,<2.0`) | Apache-2.0 |
| `httpx` | Internal telemetry HTTP transport and LLM transport wrapping (`>=0.24,<1.0`) | BSD-3-Clause |
| `Django` | Middleware and database wrapper (optional) | BSD-3-Clause |
| `djangorestframework` | DRF handler (optional) | BSD-3-Clause |
| `protobuf` | OTLP protobuf (pinned by `opentelemetry-exporter-otlp-proto-http`) | BSD-3-Clause |
