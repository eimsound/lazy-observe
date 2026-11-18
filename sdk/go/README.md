# lazy-observe Go SDK

Explicit Fiber, `slog`, and `database/sql` instrumentation backed by isolated OpenTelemetry providers. Importing this package installs no hooks and changes no global state.

## Three-line setup

```go
lo := lazyobserve.Init(lazyobserve.Config{DSN: "http://host:3100/p/myapp@token"})
app.Use(lo.Fiber())          // HTTP metrics + traces + panic capture
slog.SetDefault(lo.Slog())   // logs + trace correlation
```

`Init` never panics. Check `lo.Err()` after construction: an error means `lo` is a safe degraded instance. Always flush before process exit:

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
defer lo.Shutdown(ctx)
```

## Configuration

| Field | Default | Meaning |
|---|---|---|
| `DSN` | required | `http(s)://host:port/p/<slug>@<token>`; token is sent only as `Authorization: Bearer <token>`. The token is a project API token (`lo_...`) and can only ingest: every read route answers it with `403 ingest_only_token`, so querying needs a user API token (`lou_...`) plus `X-LO-Project` ([deployment guide](https://github.com/eimsound/lazy-observe/blob/main/docs/deployment.md#query-api-mcp-and-grafana)) |
| `ServiceName` | executable basename | OTel `service.name` and every `slog` record |
| `Metrics`, `Logs`, `Traces` | all enabled | Signal matrix described below |
| `SampleRate` | `1.0` | Parent-based head sampling ratio; zero means “unset” and resolves to `1.0`, while explicit effective values are `(0, 1]` |
| `TeeToStderr` | `nil` (enabled) | Pointer to `false` disables the stderr text-handler tee |
| `CaptureDBStatement` | `false` | Include SQL text as `db.query.text`; bound parameter values are never captured |
| `CaptureLLMContent` | `false` | Capture prompt/completion span attributes, truncated to 4 KiB each |
| `Release`, `Environment` | empty | Deployment metadata attached to error events |
| `Tags` | empty | Application tags copied at initialization and attached to error events |
| `ExportInterval` | `5s` | Metrics and logs export interval; shorter values are useful for tests |
| `Profiling` | `nil` (enabled) | Pointer to `false` disables continuous pprof CPU, heap, and goroutine collection |
| `ProfilingInterval` | `60s` | Interval between CPU sampling window starts |
| `Delivery` | zero-value defaults | Bounds custom error/profile queues and their retry budget; see Delivery reliability below |

Go `bool` has no “unset” value, so signal switches use a compact matrix rule: all three false means the zero-config default (all on); once any field is true, the three values are explicit. Examples: `Metrics: true` enables only metrics; `Logs: true, Traces: true` disables only metrics. This preserves the required `Config{DSN: ...}` default while allowing every non-empty subset. To disable all telemetry, do not call `Init`/install middleware.

## Signals

- `lo.Fiber()` creates server spans, `http.server.request.duration` and `http.server.active_requests`, route/method/status attributes, and a correlated error log plus stack on panic. Panics and handler errors that resolve to an HTTP 5xx status are also queued for Issue aggregation. A recovered panic returns HTTP 500 and is not re-panicked.
- `lo.Slog()` adds `service.name` and extracts `trace_id`/`span_id` from the supplied context. Use `InfoContext`, `ErrorContext`, and friends inside request handlers.
- `lo.WrapDB(driverName, dsn)` replaces `sql.Open`, creates DB client spans, and registers `database/sql` pool metrics. It does not register or patch drivers.
- `lo.LLMTransport(base)` explicitly wraps an `http.RoundTripper` for OpenAI-compatible `/chat/completions`, Anthropic `/v1/messages`, and Ollama `/api/chat` calls. It records `gen_ai.*` model and token attributes, including usage from final SSE chunks, without patching a provider client.
- Metrics-enabled clients also report goroutines, heap allocation, and GC count.
- Profiling collects a 10-second CPU window every 60 seconds by default (10s/60s duty cycle), followed by heap and goroutine snapshots, and queues gzip pprof payloads with a stable upload ID. Go pprof itself typically adds about 1–5% overhead while CPU sampling is active.

### Fiber request buffers and `Immutable`

Fiber v2 without `fiber.Config{Immutable: true}` returns strings from `c.Method()`, `c.Path()`, `c.Hostname()`, `c.Get()`, `c.Params()`, and similar accessors that borrow pooled request buffers; the next request overwrites them in place. `lo.Fiber()` copies every request value it keeps past the handler (span attributes and name, metric attributes, error-event tags, panic log fields, incoming `traceparent`/`tracestate`) at the moment it reads it, so its telemetry is correct whether or not your app enables `Immutable`. You do not need to turn on `Immutable` for the SDK.

Your own handlers still follow Fiber's rule: if you put a request value into a span attribute, metric attribute, log record, map, or goroutine, copy it first, for example `strings.Clone(c.Params("id"))`.

**Fixed:** earlier versions of `lo.Fiber()` kept the borrowed strings, so in apps without `Immutable` the exported `http.request.method`, `url.path`, `server.address`, `url.scheme`, trace state, metric attributes, and error tags could show another request's values once Fiber reused the buffer.

The SDK never calls `otel.SetTracerProvider`, `otel.SetMeterProvider`, `slog.SetDefault`, or registers drivers in `init`; applications opt into every integration explicitly.

## Error reporting

Fiber panic and returned 5xx errors are reported automatically. A handler that
directly writes a 5xx response without returning an error remains a performance
failure only because there is no exception to report. Client errors below 500
are not turned into Issues.

Capture application errors outside Fiber explicitly:

```go
eventID := lo.CaptureException(ctx, err)
```

Error events include the Go call stack, configured release/environment/tags,
`service.name`, and the active `trace_id`/`span_id`. Fiber adds the HTTP method,
route, status, and a URL with its query removed. Headers, cookies, authorization
values, request bodies, and query parameters are never captured.

Sending is non-blocking through a bounded queue and batches up to 20 events.
`Shutdown` drains that queue within the caller context and returns the last
terminal export failure; always retain and check the shutdown call shown above.

## Delivery reliability

Custom error and profile delivery is in-memory, bounded, non-blocking, and
at-least-once. The zero-value `DeliveryConfig` selects these defaults:

| Field | Default |
|---|---:|
| `ErrorQueueCapacity` | `100` events |
| `ProfileQueueCapacity` | `6` profiles |
| `MaxAttempts` | `3` total attempts |
| `RetryInitial` | `100ms` |
| `RetryMax` | `1s` |

Queue capacity includes the batch currently being exported. A full queue drops
new telemetry and logs a local warning; retry exhaustion drops the failed batch
only after the finite budget. `lo.Diagnostics().DroppedTelemetry` exposes the
process-local total. Metrics-enabled clients also export the low-cardinality
counter `lazy_observe_sdk_dropped_telemetry` with `signal` and `reason`
attributes. Profiles reuse the same stable `upload_id` on every attempt.

OTel traces and logs use their SDK-native bounded batch processors (queue 2048,
maximum export batch 512); lazy-observe does not wrap them in a duplicate queue.
There is no disk spool.

## LLM calls

Pass an explicitly wrapped transport to an OpenAI-compatible, Anthropic, Ollama, or plain HTTP client:

```go
httpClient := &http.Client{Transport: lo.LLMTransport(nil)}
// For a provider SDK, supply httpClient through that SDK's client option.
```

By default the span contains operation, current `gen_ai.provider.name` (plus deprecated `gen_ai.system` for compatibility), request/response model, and input/output token counts only. OpenAI streaming usage is recorded only when the server returns it (for example with `stream_options.include_usage`); otherwise `gen_ai.usage.missing=true` is recorded and no value is guessed.

Set `CaptureLLMContent: true` only after reviewing your privacy requirements. This records `gen_ai.prompt` and `gen_ai.completion`, each truncated to 4 KiB. **The application is responsible for redacting secrets, personal data, and other sensitive content before it reaches the SDK.**

See the runnable [`examples/fiber`](examples/fiber/) project for nested application and database spans, correlated logs, fast/slow/error performance routes, a concurrent traffic generator, SQLite, panic capture, profiling, and an explicitly wrapped `/llm` call.

## Direct dependencies and licenses

| Module | Use | License |
|---|---|---|
| `github.com/XSAM/otelsql` | explicit `database/sql` wrapping | Apache-2.0 |
| `github.com/gofiber/fiber/v2` | Fiber v2 middleware | MIT |
| `go.opentelemetry.io/contrib/bridges/otelslog` | slog-to-OTel bridge | Apache-2.0 |
| `go.opentelemetry.io/otel` and OTel SDK/exporters | API, SDK, OTLP/HTTP | Apache-2.0 |
| `modernc.org/sqlite` | example-only SQLite driver | BSD-3-Clause |
