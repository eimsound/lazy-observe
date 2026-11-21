# Go SDK

The Go SDK provides explicit integrations for Fiber v3, `slog`, `database/sql`, runtime metrics, and pprof. Importing the package installs no hooks and sets no global OpenTelemetry providers.

> **Breaking change.** The Go SDK supports Fiber v3 only. `lo.Fiber()` returns a Fiber v3 handler (`github.com/gofiber/fiber/v3`, `func(fiber.Ctx) error`). The SDK does not support Fiber v2 and provides no compatibility layer, so a Fiber v2 application cannot pass `lo.Fiber()` to `app.Use`. Migrate the application to Fiber v3 first using the [Fiber v3 migration notes](https://github.com/eimsound/lazy-observe/blob/main/sdk/go/README.md#fiber-v3-migration-notes). Because the SDK has no tagged release yet, no previously released version changes.

## Install

The Go SDK is the nested module `github.com/eimsound/lazy-observe/sdk/go` inside this repository, released with `sdk/go/vX.Y.Z` tags:

```sh
go get github.com/eimsound/lazy-observe/sdk/go@latest
```

Until the first SDK tag is published, point the module path at a local checkout:

```sh
go mod edit \
  -replace github.com/eimsound/lazy-observe/sdk/go=/absolute/path/to/lazy-observe/sdk/go
```

For development inside this repository, run the Fiber example from its standalone module directory `sdk/go/examples/fiber`. Its `go.mod` isolates example-only dependencies, such as `modernc.org/sqlite`, from `sdk/go/go.mod`.

## Initialize and shut down

```go
lo := lazyobserve.Init(lazyobserve.Config{
    DSN:         "http://127.0.0.1:3100/p/default@TOKEN",
    ServiceName: "checkout-api",
})
if err := lo.Err(); err != nil {
    slog.Error("lazy-observe initialization failed", "error", err)
}

defer func() {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := lo.Shutdown(ctx); err != nil {
        slog.Error("lazy-observe shutdown failed", "error", err)
    }
}()
```

`Init` returns a safe degraded client instead of panicking on invalid configuration. The DSN must match `http(s)://host[:port]/p/<project>@<token>` with no query string or fragment.

## Fiber and logs

```go
app := fiber.New()
app.Use(lo.Fiber())
slog.SetDefault(lo.Slog())

app.Get("/orders/:id", func(c fiber.Ctx) error {
    slog.InfoContext(c.Context(), "loading order", "id", c.Params("id"))
    return c.SendStatus(fiber.StatusNoContent)
})
```

`Fiber()` extracts W3C `traceparent`, creates server spans, records request duration and active-request metrics, and recovers panics as HTTP 500 with a correlated stack log. The middleware submits recovered panics and returned handler errors with a 5xx effective status asynchronously as error events, and the platform aggregates them into Issues. `url.scheme` reads Fiber v3 `Ctx.Scheme()`, honoring forwarded scheme headers only when the application trusts the peer through `fiber.Config{TrustProxy: true}`. Use context-aware `slog` methods to attach `trace_id` and `span_id`.

For tighter host isolation, call `lo.FiberWithOptions(lazyobserve.FiberOptions{...})`:

```go
app.Use(lo.FiberWithOptions(lazyobserve.FiberOptions{
    // Map domain errors to status codes without invoking Fiber's ErrorHandler inside the middleware.
    StatusFromError: func(err error) int { ... },
    // Re-panic after recording spans, metrics, logs, and error events when an outer recovery middleware owns HTTP 500 responses.
    RepanicOnPanic:  true,
    // Sanitize sensitive path segments before recording url.path, panic logs, and error-event URLs.
    RedactPath:      func(path string) string { ... },
}))
```

The SDK does not call `slog.SetDefault`. The application chooses whether to set the default logger.

## Database instrumentation

```go
db, err := lo.WrapDB("postgres", os.Getenv("DATABASE_URL"))
if err != nil {
    return err
}
defer db.Close()

row := db.QueryRowContext(c.Context(), "SELECT status FROM orders WHERE id = $1", id)
```

`WrapDB` replaces `sql.Open`, creates client spans, and registers pool metrics without patching or registering drivers. `lo.Shutdown` unregisters its `metric.Registration`. For short-lived or dynamically rotated database pools, call `lo.WrapDBContext(poolCtx, driverName, dsn)`. That call unregisters pool metrics when `poolCtx` is cancelled and on `Shutdown`, preventing callback leaks. Import your database driver as usual. SQL text capture is disabled by default. Setting `CaptureDBStatement: true` captures query text and never captures bound parameter values.

## Configuration and capability switches

| Field | Default | Behavior |
| --- | --- | --- |
| `DSN` | required | Gateway, project, and project API token. This `lo_...` project API token can only ingest. Read routes reject it with `403 ingest_only_token`. Querying requires a `lou_...` user API token and the `X-LO-Project` header. See the [deployment guide](../deployment.md#query-api-mcp-and-grafana). |
| `ServiceName` | executable basename | OpenTelemetry `service.name` and `slog` field. |
| `Metrics`, `Logs`, `Traces` | all enabled when all are false | Once any flag is true, the three booleans form an explicit enabled set. |
| `SampleRate` | `1` | Parent-based head sampling. Zero means "unset" and resolves to `1`. Explicit effective values are `(0, 1]`. |
| `TeeToStderr` | enabled | Pointer to `false` disables local text output. |
| `CaptureDBStatement` | `false` | Include SQL statement text in database spans. |
| `Release`, `Environment` | empty | Deployment metadata attached to error events. |
| `Tags` | empty | Application tags copied during initialization and attached to error events. |
| `ExportInterval` | 5 seconds | Periodic metrics and log export interval. |
| `Profiling` | enabled | Pointer to `false` disables CPU, heap, and goroutine profiling. |
| `ProfilingCPU` | enabled | Pointer to `false` disables CPU pprof sampling while keeping heap and goroutine snapshots, for cases where another profiler owns `runtime/pprof.StartCPUProfile`. |
| `ProfilingInterval` | 60 seconds | Base interval between profiling cycle starts, jittered on initial start and on each cycle. |

Enable only metrics and traces:

```go
lo := lazyobserve.Init(lazyobserve.Config{
    DSN:     dsn,
    Metrics: true,
    Traces:  true,
})
```

Because the zero-value matrix enables all three signals, disable all signals by not initializing the SDK.

## Error reporting

Fiber reports recovered panics and handler errors whose effective status is 500 or greater. Returning a 4xx error does not create an Issue. Writing a 5xx response directly without returning an error appears in metrics and traces, not as an exception event.

Report errors outside Fiber with the core API:

```go
eventID := lo.CaptureException(ctx, err)
```

Each error event carries an outermost-first Go stack, configured release, environment, and tags, `service.name`, and the active `trace_id` and `span_id`. Fiber adds the HTTP method, route, status, and a query-free request URL. The SDK does not capture request bodies, headers, cookies, authorization values, or URL query strings.

Error delivery uses a bounded non-blocking queue and sends batches of up to 20 events. Before enqueueing an event, the SDK trims it to fit the gateway's 200 KiB single-event budget by capping the message, exception type and value, release, environment, URL, tags, and stack frames in UTF-8-safe steps. When the queue is full, the SDK drops the event, updates `Diagnostics().DroppedTelemetry` and the drop metric, and emits a rate-limited local warning without delaying requests or flooding logs. `Shutdown` drains the queue and reports the latest export failure.

## Continuous profiling

Profiling starts with `Init` unless disabled:

```go
enabled := true
lo := lazyobserve.Init(lazyobserve.Config{
    DSN:               dsn,
    ServiceName:       "checkout-api",
    Profiling:         &enabled,
    ProfilingInterval: time.Minute,
})
```

Each cycle records a 10-second CPU window unless `ProfilingCPU` is `false`, collects heap and goroutine snapshots, and uploads gzip pprof payloads to `/api/profiles`. Both the initial start and subsequent cycle intervals include randomized jitter to prevent synchronized spikes across a fleet. If another profiler already owns `pprof.StartCPUProfile`, the SDK logs one warning and continues collecting heap and goroutine snapshots.

Profile uploads share the SDK's dedicated ingest HTTP client and bounded `Delivery` retry budget. That budget enforces `MaxAttempts`, Full Jitter exponential backoff, and server `Retry-After` headers. The client drops permanent 4xx responses and payloads over 10 MiB immediately and never fails application requests. If `Shutdown` interrupts a CPU window, the SDK uploads that window only when it has run for at least 5 seconds. The SDK discards shorter windows and does not count them as dropped telemetry. Heap and goroutine snapshots run immediately, and the SDK uploads all queued payloads within the `Shutdown` context deadline.

See the runnable [Fiber example](https://github.com/eimsound/lazy-observe/blob/main/sdk/go/examples/fiber/README.md) for a nested order trace, database child spans, correlated logs, controlled fast, slow, and error routes, and a concurrent load generator that populates the Performance page.
