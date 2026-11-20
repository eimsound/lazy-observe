# lazy-observe Go SDK

Explicit Fiber v3, `slog`, and `database/sql` instrumentation backed by isolated OpenTelemetry providers. Importing this package installs no hooks and changes no global state.

> **Breaking: Fiber v3 only.** `lo.Fiber()` now returns a Fiber v3 handler (`func(fiber.Ctx) error`, module `github.com/gofiber/fiber/v3`). Fiber v2 (`*fiber.Ctx`, `github.com/gofiber/fiber/v2`) is no longer supported and there is no v2 compatibility layer: an application still on v2 cannot pass `lo.Fiber()` to `app.Use`. Migrate the application to Fiber v3 first, following the official [v2 to v3 migration guide](https://docs.gofiber.io/whats_new#-migration-guide). The SDK has not been released yet, so no earlier tagged version is affected. Migration points that touch SDK usage are listed under [Fiber v3 migration notes](#fiber-v3-migration-notes).

## Three-line setup

```go
lo := lazyobserve.Init(lazyobserve.Config{DSN: "http://host:3100/p/myapp@token"})
app.Use(lo.Fiber())          // HTTP metrics + traces + panic capture
slog.SetDefault(lo.Slog())   // logs + trace correlation
```

`Init` never panics. Check `lo.Err()` after construction: an error means `lo` is a safe degraded instance. Always flush before process exit:

```go
defer func() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := lo.Shutdown(ctx); err != nil {
		slog.Error("lazy-observe shutdown failed", "error", err)
	}
}()
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
| `ProfilingCPU` | `nil` (enabled) | Pointer to `false` disables CPU pprof sampling while continuing heap and goroutine snapshots |
| `ProfilingInterval` | `60s` | Base interval between profiling cycle starts (jittered on initial start and each cycle) |
| `Delivery` | zero-value defaults | Bounds custom error/profile queues and their retry budget; see Delivery reliability below |

Go `bool` has no “unset” value, so signal switches use a compact matrix rule: all three false means the zero-config default (all on); once any field is true, the three values are explicit. Examples: `Metrics: true` enables only metrics; `Logs: true, Traces: true` disables only metrics. This preserves the required `Config{DSN: ...}` default while allowing every non-empty subset. To disable all telemetry, do not call `Init`/install middleware.

## Signals

- `lo.Fiber()` creates server spans, `http.server.request.duration` and `http.server.active_requests`, route/method/status attributes, and a correlated error log plus stack on panic. Panics and handler errors that resolve to an HTTP 5xx status are also queued for Issue aggregation. By default a recovered panic returns HTTP 500 and is not re-panicked. Use `lo.FiberWithOptions(lazyobserve.FiberOptions{StatusFromError, RepanicOnPanic, RedactPath})` to classify returned errors without calling `ErrorHandler` inside the middleware, re-panic to an outer recovery middleware, or redact sensitive path segments before telemetry capture.
- `lo.Slog()` adds `service.name` and extracts `trace_id`/`span_id` from the supplied context. Use `InfoContext`, `ErrorContext`, and friends inside request handlers.
- `lo.WrapDB(driverName, dsn)` replaces `sql.Open`, creates DB client spans, and registers `database/sql` pool metrics that unregister on `Shutdown`. Use `lo.WrapDBContext(ctx, driverName, dsn)` for short-lived or rotated pools so pool metric registrations unregister as soon as `ctx` is cancelled. Neither registers or patches drivers.
- `lo.LLMTransport(base)` explicitly wraps an `http.RoundTripper` for OpenAI-compatible `/chat/completions`, Anthropic `/v1/messages`, and Ollama `/api/chat` calls. It records `gen_ai.*` model and token attributes, including usage from final SSE chunks, without patching a provider client.
- Metrics-enabled clients also report goroutines, heap allocation, and GC count.
- Profiling collects a 10-second CPU window every 60 seconds by default (10s/60s duty cycle, with randomized initial and periodic jitter), followed by heap and goroutine snapshots, and queues gzip pprof payloads with a stable upload ID. Set `ProfilingCPU` to `&false` to skip CPU sampling while keeping heap and goroutine snapshots. If another profiler already owns `pprof.StartCPUProfile`, the SDK emits a one-time warning and continues collecting snapshots. If `Shutdown` interrupts a CPU window, it is uploaded only when it already ran at least 5 seconds; a shorter window is discarded and not counted as dropped telemetry, while the heap and goroutine snapshots are still collected; queued payloads are uploaded within the `Shutdown` context deadline. Go pprof itself typically adds about 1–5% overhead while CPU sampling is active.

### Fiber request buffers and `Immutable`

Fiber v3 without `fiber.Config{Immutable: true}` returns strings from `c.Method()`, `c.Path()`, `c.Hostname()`, `c.Get()`, `c.Params()`, and similar accessors that borrow pooled request buffers; the next request overwrites them in place. `lo.Fiber()` copies every request value it keeps past the handler (span attributes and name, metric attributes, error-event tags, panic log fields, incoming `traceparent`/`tracestate`) at the moment it reads it, so its telemetry is correct whether or not your app enables `Immutable`. You do not need to turn on `Immutable` for the SDK.

Your own handlers still follow Fiber's rule: if you put a request value into a span attribute, metric attribute, log record, map, or goroutine, copy it first, for example `strings.Clone(c.Params("id"))`.

**Fixed:** earlier versions of `lo.Fiber()` kept the borrowed strings, so in apps without `Immutable` the exported `http.request.method`, `url.path`, `server.address`, `url.scheme`, trace state, metric attributes, and error tags could show another request's values once Fiber reused the buffer.

### Fiber v3 migration notes

These are the Fiber v2 to v3 changes an application meets when it adopts `lo.Fiber()`; the full list is in the official [migration guide](https://docs.gofiber.io/whats_new#-migration-guide).

- Handlers take the `fiber.Ctx` interface instead of `*fiber.Ctx`.
- `c.UserContext()` and `c.SetUserContext()` are gone. Use `c.Context()` and `c.SetContext()`; `lo.Fiber()` stores the server span there, so `slog.InfoContext(c.Context(), ...)` and `tracer.Start(c.Context(), ...)` keep working. `c.Context()` is safe to keep after the handler returns. The `fiber.Ctx` value itself implements `context.Context` but is pooled and reused, and it never reports cancellation, so do not hand `c` to a goroutine or a client that outlives the request.
- v2 `c.Context()` (the `*fasthttp.RequestCtx`) is now `c.RequestCtx()`.
- `c.BodyParser`, `c.QueryParser` and friends are `c.Bind().Body()`, `c.Bind().Query()`; `c.QueryBool` and the other typed query helpers are `fiber.Query[bool](c, "key", false)`.
- `c.Protocol()` now returns the HTTP version (`HTTP/1.1`); the request scheme (`http` or `https`) is `c.Scheme()`. `lo.Fiber()` uses `Scheme()` for `url.scheme`.
- Proxy headers are ignored unless the app trusts the peer. v2 trusted every peer by default (`EnableTrustedProxyCheck: false`), so `c.Hostname()` read `X-Forwarded-Host` and the scheme came from `X-Forwarded-Proto`, `X-Forwarded-Protocol`, `X-Forwarded-Ssl` or `X-Url-Scheme`. In v3, `c.Hostname()`, `c.Scheme()` and `c.IP()` read them only when `fiber.Config{TrustProxy: true}` and `TrustProxyConfig` trust the connecting address (`c.IP()` also needs `ProxyHeader`). Behind a reverse proxy without that configuration, `server.address` and the host in error-event URLs are the `Host` the proxy sent, not `X-Forwarded-Host`, and `url.scheme` ignores spoofed `X-Forwarded-Proto` headers and reflects TLS only.
- Every `GET` route also answers `HEAD` by running the same chain, so `HEAD` requests now produce spans and `http.server.request.duration` points under the `GET` route's `http.route`. Set `fiber.Config{DisableHeadAutoRegister: true}` to keep v2 routing.
- `DisableStartupMessage` moved from `fiber.Config` to `fiber.ListenConfig`: `app.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true})`.
- `app.Test(req, msTimeout)` is `app.Test(req, fiber.TestConfig{Timeout: d, FailOnTimeout: true})`; a zero `Timeout` waits without limit (v2 `-1`).
- `Ctx.Body()` decodes `Content-Encoding` itself in v3 and, for a coding it cannot undo, answers 415 or 501 and returns the error text in place of the bytes. Read `c.Request().Body()` if you must see the raw bytes of such a request.

Fiber v3 request strings follow the same zero-copy rule as v2: without `Immutable` they are valid only until the handler returns, and `lo.Fiber()` still copies everything it keeps. The v3 `c.App().GetString` and `c.App().GetBytes` helpers detach a value only when `Immutable` is on, so use `strings.Clone` or `utils.CopyString` when your app does not enable it.

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
new telemetry, updates drop counters/metrics on every drop, and emits a
rate-limited local warning (reporting suppressed drop counts on the next window)
so high-QPS outages do not flood host logs. Both error-event and profile uploads
share a dedicated ingest HTTP client isolated from `http.DefaultClient`, classify
permanent `4xx` (dropping immediately) vs retryable `408`/`429`/`5xx`, honor
`Retry-After`, apply Full Jitter exponential backoff, and cap response body
reads/drains. Retry exhaustion drops the failed batch only after the finite
budget. `lo.Diagnostics().DroppedTelemetry` exposes the process-local total.
Metrics-enabled clients also export the low-cardinality counter
`lazy_observe_sdk_dropped_telemetry` with `signal` and `reason` attributes.
Profiles reuse the same stable `upload_id` on every attempt.

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
| `github.com/gofiber/fiber/v3` | Fiber v3 middleware | MIT |
| `go.opentelemetry.io/contrib/bridges/otelslog` | slog-to-OTel bridge | Apache-2.0 |
| `go.opentelemetry.io/otel` and OTel SDK/exporters | API, SDK, OTLP/HTTP | Apache-2.0 |

The runnable [`examples/fiber`](examples/fiber/) project has its own `go.mod` so example-only dependencies (`modernc.org/sqlite`) stay out of the `github.com/eimsound/lazy-observe/sdk/go` module graph.
