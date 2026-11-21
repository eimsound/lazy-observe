# Node SDK

The Node SDK instruments Express, Pino, Winston, runtime metrics, and platform error events explicitly. Importing `@lazy-observe/node` does not patch modules or set a global OpenTelemetry provider.

## Install

The package lives in the repository pnpm workspace and is not yet published to a package registry. From `sdk/node`, run:

```sh
mise exec -- pnpm install --frozen-lockfile
mise exec -- pnpm build
```

Workspace examples resolve `@lazy-observe/node` directly. Add a registry installation command only after publishing the package.

## Express setup

```ts
import express from "express";
import { init } from "@lazy-observe/node";

const lo = init({
  dsn: "http://127.0.0.1:3100/p/default@TOKEN",
  serviceName: "checkout-api"
});
const app = express();
app.use(lo.express());
const logger = lo.pino();

app.get("/", (_req, res) => {
  logger.info("hello from express");
  res.json({ ok: true });
});

app.use(lo.expressError());
app.use((error: Error, _req, res, _next) => {
  res.status(500).json({ error: error.message });
});
```

Install `expressError()` after your routes and before the application's final error handler. Shut down providers during process termination:

```ts
await lo.shutdown();
```

## Configuration

| Option | Default | Behavior |
| --- | --- | --- |
| `dsn` | required | `<origin>/p/<project>@<token>`. This `lo_...` project API token can only ingest. Read routes reject it with `403 ingest_only_token`. Querying requires a `lou_...` user API token and the `X-LO-Project` header. See the [deployment guide](../deployment.md#query-api-mcp-and-grafana). |
| `serviceName` | `process.title` | OpenTelemetry service name and logger field. |
| `serviceVersion` | unset | OpenTelemetry service version. |
| `metrics`, `logs`, `traces` | `true` independently | Enable or disable each signal. |
| `sampleRate` | `1` | Parent-based head sampling, from `0` through `1`. |
| `teeToStdout` | `true` | Also write Pino and Winston records locally. |
| `exportIntervalMs` | `5000` | Metrics and log export interval in milliseconds. |
| `runtimeMetricsIntervalMs` | `1000` | Event loop, heap, and active-handle collection interval in milliseconds. |
| `captureUnhandledErrors` | `false` | Report unhandled rejections and observe uncaught exceptions. |
| `profiling` | `true` | Collect and upload V8 CPU and sampled-heap pprof profiles. |
| `profilingInterval` | `60000` | Cycle interval in milliseconds. Each cycle includes a 10-second CPU window. |
| `release`, `environment`, `tags` | unset | Fields copied to error events. |

The Express middleware extracts W3C `traceparent` explicitly and records request count, duration, and active requests. Runtime collection uses `perf_hooks` and an unreferenced timer.

## Pino and Winston

`pino` and `winston-transport` are optional peer dependencies. The SDK loads them lazily only when you call `lo.pino()` or `lo.winston()`. Install them in your application only when you use those integrations.

```ts
const pinoLogger = lo.pino({ level: "info" });
```

```ts
import winston from "winston";

const winstonLogger = winston.createLogger({
  transports: [lo.winston()]
});
```

Both integrations add `service.name`, `trace_id`, and `span_id` when context is available and export OTLP logs.

## Error reporting

`expressError()` records the exception on the active request span and sends a platform error event. Report handled errors directly when needed:

```ts
try {
  await chargeCard();
} catch (error) {
  await lo.reportError(error, { url: "/checkout" });
  throw error;
}
```

Before queuing an error event, the built-in `sanitize` pipeline strips URL credentials, query strings (`?...`), and fragments (`#...`) by default. It also caps long messages, tags, and stack frames, and trims each event to fit the 200 KiB gateway budget. The bounded `DeliveryQueue` splits batches by byte budget, drops permanent 4xx rejections immediately as `server_rejected`, and retries transient failures (`408`, `429`, `5xx`, timeouts, and network errors) using `Retry-After` or Full Jitter exponential backoff.

Setting `captureUnhandledErrors: true` installs an `uncaughtExceptionMonitor` listener. It observes both `uncaughtException` and unhandled promise rejections under Node's default throw behavior without suppressing Node's normal fatal exit semantics. Prefer explicit error handling when possible.

## Continuous profiling and replay

Profiling starts by default. You can configure it explicitly:

```ts
const lo = init({
  dsn,
  serviceName: "checkout-api",
  profiling: true,
  profilingInterval: 60_000
});
```

The SDK uses a Node inspector session without opening a debug port. Each cycle samples CPU at 1 ms for 10 seconds and takes one sampled-heap profile, converts both to pprof, gzips them, and uploads them to `/api/profiles`. Upload failures back off up to 15 minutes. An inspector setup failure disables profiling after one warning. Calling `shutdown()` stops the profiler. Set `profiling: false` when the process cannot tolerate profiling or when another inspector profiler owns the session.

Session replay is browser-only.

See the runnable [Express example](https://github.com/eimsound/lazy-observe/blob/main/sdk/node/examples/express/src/main.ts).
