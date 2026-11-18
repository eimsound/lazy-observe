# Go SDK

The Go SDK provides explicit Fiber v2, `slog`, `database/sql`, runtime metric, and pprof integrations. Importing it installs no hooks or global OpenTelemetry providers.

## Install

The Go SDK is the nested module `github.com/eimsound/lazy-observe/sdk/go`
inside this repository, released with `sdk/go/vX.Y.Z` tags:

```sh
go get github.com/eimsound/lazy-observe/sdk/go@latest
```

Until the first SDK tag is published, point the module path at a local checkout:

```sh
go mod edit \
  -replace github.com/eimsound/lazy-observe/sdk/go=/absolute/path/to/lazy-observe/sdk/go
```

For development inside this repository, run examples from `sdk/go`, whose
`go.mod` declares the SDK module.

## Initialize and shut down

```go
lo := lazyobserve.Init(lazyobserve.Config{
    DSN:         "http://127.0.0.1:3100/p/default@TOKEN",
    ServiceName: "checkout-api",
})
if err := lo.Err(); err != nil {
    slog.Error("lazy-observe initialization failed", "error", err)
}

ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
defer func() { _ = lo.Shutdown(ctx) }()
```

`Init` returns a safe degraded client instead of panicking on invalid configuration. The DSN must be `http(s)://host[:port]/p/<project>@<token>` without query or fragment data.

## Fiber and logs

```go
app := fiber.New()
app.Use(lo.Fiber())
slog.SetDefault(lo.Slog())

app.Get("/orders/:id", func(c *fiber.Ctx) error {
    slog.InfoContext(c.UserContext(), "loading order", "id", c.Params("id"))
    return c.SendStatus(fiber.StatusNoContent)
})
```

`Fiber()` extracts W3C `traceparent`, creates server spans, records request
duration and active-request metrics, and recovers panics as HTTP 500 with a
correlated stack log. Panics and returned handler errors whose effective status
is 5xx are asynchronously submitted as error events and aggregated into Issues.
Use context-aware slog methods to attach `trace_id` and `span_id`.

The SDK does not call `slog.SetDefault`; the application chooses whether to do so.

## Database instrumentation

```go
db, err := lo.WrapDB("postgres", os.Getenv("DATABASE_URL"))
if err != nil {
    return err
}
defer db.Close()

row := db.QueryRowContext(c.UserContext(), "SELECT status FROM orders WHERE id = $1", id)
```

`WrapDB` replaces `sql.Open`, creates client spans, and registers pool metrics without patching or registering drivers. Import the chosen database driver normally. SQL text is disabled by default; setting `CaptureDBStatement: true` captures query text but never bound parameter values.

## Configuration and capability switches

| Field | Default | Behavior |
| --- | --- | --- |
| `DSN` | required | Gateway, project, and project API token. The token is a `lo_...` project API token and can only ingest: read routes answer it with `403 ingest_only_token`, so querying needs a `lou_...` user API token plus the `X-LO-Project` header ([deployment guide](../deployment.md#query-api-mcp-and-grafana)) |
| `ServiceName` | executable basename | OTel `service.name` and slog field |
| `Metrics`, `Logs`, `Traces` | all enabled when all are false | Once any is true, the three booleans form an explicit enabled set |
| `SampleRate` | `1` | Parent-based head sampling; zero means “unset” and resolves to `1`, while explicit effective values are `(0, 1]` |
| `TeeToStderr` | enabled | Pointer to false disables local text output |
| `CaptureDBStatement` | false | Include SQL statement text in DB spans |
| `Release`, `Environment` | empty | Deployment metadata attached to error events |
| `Tags` | empty | Application tags copied during initialization and attached to error events |
| `ExportInterval` | 5 seconds | Periodic metrics/log export |
| `Profiling` | enabled | Pointer to false disables CPU, heap, and goroutine profiling |
| `ProfilingInterval` | 60 seconds | Time between CPU sampling window starts |

Example: only metrics and traces:

```go
lo := lazyobserve.Init(lazyobserve.Config{
    DSN:     dsn,
    Metrics: true,
    Traces:  true,
})
```

Because the zero-value matrix enables all three signals, disabling every signal means not initializing the SDK.

## Error reporting

Fiber reports recovered panics and handler errors with an effective status of
500 or greater. Returning a 4xx error does not create an Issue. Directly writing
a 5xx response without returning an error remains visible in metrics and traces,
but does not create a fabricated exception event.

Use the core API outside Fiber:

```go
eventID := lo.CaptureException(ctx, err)
```

The event carries an outermost-first Go stack, configured
release/environment/tags, `service.name`, and the active `trace_id` and
`span_id`. Fiber adds method, route, status, and a query-free request URL.
Request bodies, headers, cookies, authorization values, and URL queries are not
captured.

Error sending uses a bounded non-blocking queue and batches up to 20 events.
Queue saturation drops the event and emits a local warning instead of delaying
the request. `Shutdown` drains the queue and reports the latest export failure.

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

Each cycle records a 10-second CPU window followed by heap and goroutine snapshots, then uploads the three gzip pprof payloads to `/api/profiles`. Failed uploads back off up to 15 minutes and do not fail application requests.

See the runnable [Fiber example](../../sdk/go/examples/fiber/) for a nested order trace, database child spans, correlated logs, controlled fast/slow/error routes, and a concurrent load generator that populates the Performance page.
