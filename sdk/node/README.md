# lazy-observe Node SDK

Explicit Express, Pino, and Winston instrumentation backed by manually assembled OpenTelemetry providers. Importing `@lazy-observe/node` installs no hooks, patches no modules, and changes no global provider; all runtime work starts in `init()`.

## Requirements

Node.js `>=22.15` (declared in `engines.node`). The SDK is built on OpenTelemetry JS 3.x (`@opentelemetry/api` 1.10, stable SDK packages 3.1, OTLP exporters 0.301), whose packages all require Node.js `>=22.15.0`. Node.js 20 and Node.js 22.0–22.14 are not supported.

The SDK uses `@opentelemetry/api` 1.10 and its built-in Logs API; it no longer depends on `@opentelemetry/api-logs` or `@opentelemetry/sdk-trace-node`. The OTel 3.x trace SDK no longer reads `OTEL_BSP_*` environment variables as batch span processor fallbacks; sampling has always been configured only through `sampleRate`.

## Three-line setup

```ts
import { init } from "@lazy-observe/node";
const lo = init({ dsn: "http://host:3100/p/myapp@token" });
app.use(lo.express());
const logger = lo.pino();
```

Install the error middleware after routes and before the application's final error handler:

```ts
app.use(lo.expressError());
app.use((error, req, res, next) => res.status(500).json({ error: error.message }));
```

Flush before exit with an explicit caller budget:

```ts
await lo.shutdown({ timeoutMs: 10_000 });
```

## Configuration

| Field | Default | Meaning |
|---|---|---|
| `dsn` | required | `http(s)://host:port/p/<slug>@<token>`; the token is sent as `Authorization: Bearer <token>`. The token is a project API token (`lo_...`) and can only ingest: every read route answers it with `403 ingest_only_token`, so querying needs a user API token (`lou_...`) plus `X-LO-Project` ([deployment guide](https://github.com/eimsound/lazy-observe/blob/main/docs/deployment.md#query-api-mcp-and-grafana)) |
| `serviceName` | `process.title` | OTel `service.name` and logger field |
| `serviceVersion` | unset | OTel `service.version` |
| `metrics`, `logs`, `traces` | `true` each | Independent signal switches |
| `sampleRate` | `1` | Parent-based head sampling ratio, from `0` to `1` |
| `compression` | `"none"` | OTLP/HTTP payload compression (`""`, `"none"`, or `"gzip"`). When empty or unset, honors `OTEL_EXPORTER_OTLP_<SIGNAL>_COMPRESSION` then `OTEL_EXPORTER_OTLP_COMPRESSION`, defaulting to `"none"`; explicit config overrides env vars |
| `teeToStdout` | `true` | Pino/Winston records are also written to stdout |
| `exportIntervalMs` | `5000` | Metrics and logs export interval |
| `runtimeMetricsIntervalMs` | `1000` | Explicit event-loop, heap, and active-handle collection interval |
| `captureUnhandledErrors` | `false` | Report `unhandledRejection` and observe `uncaughtException` without suppressing it |
| `captureLLMContent` | `false` | Capture prompt/completion span attributes, truncated to 4 KiB each |
| `profiling` | `true` | Continuously collect and upload sampled CPU and heap pprof profiles |
| `profilingInterval` | `60000` | Profiling cycle interval in milliseconds; each cycle contains a 10 second CPU window |
| `release`, `environment`, `tags` | unset | Fields attached to error events |
| `delivery.errorQueueCapacity` | `100` | Maximum queued plus in-flight custom error events |
| `delivery.profileQueueCapacity` | `6` | Maximum queued plus in-flight profiles |
| `delivery.maxAttempts` | `3` | Total attempts for each custom delivery batch |
| `delivery.retryInitialMs`, `delivery.retryMaxMs` | `100`, `2000` | Capped exponential retry delay |
| `shutdownTimeoutMs` | `10000` | Default shutdown budget when the caller omits `timeoutMs` |

## Signals and integrations

- `lo.express()` creates a server span plus `http.server.request.count`, `http.server.request.duration`, and `http.server.active_requests`. Route, method, status, scheme, host, and protocol attributes follow current HTTP semantic conventions.
- `lo.expressError()` records the exception on the request span and sends error protocol v1 events with `platform: "node"`. V8 frames are outermost-first and dependency frames have `in_app: false`.
- `lo.pino(options?)` returns a configured Pino logger. `lo.winston()` returns a Winston transport. Both add `service.name`, `trace_id`, and `span_id`, export through OTLP logs, and tee to stdout by default.
- Runtime metrics cover mean event-loop delay, heap used/total, and active handles. Collection uses `perf_hooks` and an explicit unref'd timer.
- Continuous profiling uses Node's inspector sampler without opening a debug port: 1ms CPU sampling for 10 seconds per cycle plus one sampled-heap profile. Collection and upload are decoupled; profiles retain one stable `upload_id` across retries. Inspector setup failure disables profiling with one warning.
- `lo.reportError(error)` constructs and enqueues an application error, then resolves without waiting for export.
- `lo.wrapLLMFetch(fetch?)` explicitly wraps a Fetch implementation for OpenAI-compatible `/chat/completions`, Anthropic `/v1/messages`, and Ollama `/api/chat` calls. It records `gen_ai.*` model and token attributes, including usage from final SSE chunks, without patching Fetch or a provider SDK.

### LLM calls

The returned function can be passed to the official OpenAI client's `fetch` option or called directly:

```ts
const tracedFetch = lo.wrapLLMFetch();
const openai = new OpenAI({ apiKey: process.env.OPENAI_API_KEY, fetch: tracedFetch });
```

By default spans contain operation, current `gen_ai.provider.name` (plus deprecated `gen_ai.system` for compatibility), request/response model, and input/output token counts only. OpenAI streaming usage is recorded only when returned by the server (for example with `stream_options.include_usage`); otherwise `gen_ai.usage.missing=true` is recorded and no value is guessed.

Set `captureLLMContent: true` only after reviewing your privacy requirements. This records `gen_ai.prompt` and `gen_ai.completion`, each truncated to 4 KiB. **The application is responsible for redacting secrets, personal data, and other sensitive content before it reaches the SDK.**

### Browser-to-server trace continuity

The Express middleware explicitly extracts the W3C `traceparent` request header. A request instrumented by `@lazy-observe/browser` therefore keeps the browser trace ID; the Express server span uses the browser span as its remote parent. No HTTP library is patched.

### Winston

```ts
import winston from "winston";
const logger = winston.createLogger({ transports: [lo.winston()] });
```

See `examples/express` for a complete runnable application.

## Delivery reliability

Custom error and profile delivery is bounded, non-blocking, in-memory, and
at-least-once. Capacity includes an active export. A full queue drops new work;
retry exhaustion drops the retained batch only after three attempts. Both emit
a local warning. `lo.diagnostics().droppedTelemetry` exposes the local total;
when metrics are enabled, the same drops are exported as
`lazy_observe_sdk_dropped_telemetry` with only `signal` and `reason` attributes.

`shutdown({timeoutMs})` stops accepting new custom telemetry, drains both queues
within the caller budget, and rejects with the latest terminal export error or
timeout. OTel traces and logs keep their native bounded processors (queue 2048,
maximum export batch 512), without another wrapper queue. There is no disk spool.

## Public API

`init`, `parseDSN`, `parseV8Stack`, `extractTraceparent`, and the configuration,
diagnostic, shutdown, DSN, error, and stack-frame types are exported. A client
exposes `express`, `expressError`, `pino`, `winston`, `wrapLLMFetch`,
`reportError`, `diagnostics`, `signals`, and `shutdown`.

## Dependency policy

There is no `@opentelemetry/auto-instrumentations`, `require-in-the-middle`, or equivalent module loader. Direct production dependencies and licenses are recorded in [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).
