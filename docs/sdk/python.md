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
| `tags` | empty | Application tags copied at initialization and attached to error events |
| `export_interval` | `5s` | Metrics/log export interval |
| `delivery` | zero-value defaults | Error queue bounds and retry budget |

Signal matrix: all three of `metrics`/`logs`/`traces` false means all enabled;
once any is true the three values form an explicit enabled set. To disable all
telemetry, do not call `init`.

## Django

Add `lazyobserve.DjangoMiddleware` to `MIDDLEWARE`. The middleware:

- extracts W3C `traceparent` and creates a SERVER span (`url.path`,
  `http.request.method`, `url.scheme`, `server.address`, protocol version);
- records `http.server.request.duration` and `http.server.active_requests`;
- recovers unhandled exceptions as HTTP 500 with an error log and queues the
  exception as a platform error event (Issue aggregation). Setting
  `config.re_raise_on_500 = True` re-raises instead (preserves the Django DEBUG
  experience; default matches the Fiber behavior of returning 500);
- attaches `http.route` (resolver route) and `http.response.status_code`.

```python
MIDDLEWARE = [
    "lazyobserve.DjangoMiddleware",
    "django.middleware.common.CommonMiddleware",
]
```

A 5xx response that is directly written (no exception) remains visible in
metrics/traces but does not create an Issue — identical to Fiber.

## DRF

Configure the exception handler so API exceptions that resolve to 5xx are
queued for Issue aggregation with request context (method/route/status,
query-free URL):

```python
REST_FRAMEWORK = {
    "EXCEPTION_HANDLER": "lazyobserve.drf.exception_handler",
}
```

Non-API exceptions are re-raised so the Django 500 path (and the middleware)
captures them exactly once. 4xx API errors (validation, auth) never become
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
- `process.runtime.python.memory.rss` — resident set size (`getrusage`)
- `process.runtime.python.gc.count` — cumulative GC cycles (`gc.get_stats()`)

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
`lazy_observe_sdk_dropped_telemetry` with `signal`/`reason` attributes. There is
no disk spool.

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
| pprof cpu+heap+goroutine | not implemented (no pprof-producing sampler on modern CPython) |
| `lo.Shutdown(ctx)` | `lo.shutdown(timeout)` |
| `Diagnostics().DroppedTelemetry` | `Diagnostics().dropped_telemetry` |
| `django_db` / `LLM` content flags | `capture_db_statement` / `capture_llm_content` |

## Direct dependencies and licenses

| Dependency | Use | License |
| --- | --- | --- |
| `opentelemetry-api` / `-sdk` | API, SDK | Apache-2.0 |
| `opentelemetry-exporter-otlp-proto-http` | OTLP/HTTP exporters | Apache-2.0 |
| `httpx` / `httpcore` | LLM transport wrapping | BSD-3-Clause |
| `Django` | middleware / DB wrapper (optional) | BSD-3-Clause |
| `djangorestframework` | DRF handler (optional) | BSD-3-Clause |
| `protobuf` | OTLP protobuf (pinned by `opentelemetry-exporter-otlp-proto-http`) | BSD-3-Clause |
