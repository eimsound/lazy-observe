# Node SDK

The Node SDK explicitly instruments Express, Pino, Winston, runtime metrics, and platform error events. Importing `@lazy-observe/node` does not patch modules or set a global OpenTelemetry provider.

## Install

The package currently lives in the repository pnpm workspace and is not documented as a published registry release. From `sdk/node`:

```sh
mise exec -- pnpm install --frozen-lockfile
mise exec -- pnpm build
```

Workspace examples resolve `@lazy-observe/node` directly. A registry installation command should be added only after a package is published.

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

Install `expressError()` after routes and before the application's final error handler. Shut down providers during process termination:

```ts
await lo.shutdown();
```

## Configuration

| Option | Default | Behavior |
| --- | --- | --- |
| `dsn` | required | `<origin>/p/<project>@<token>` |
| `serviceName` | `process.title` | OTel service name and logger field |
| `serviceVersion` | unset | OTel service version |
| `metrics`, `logs`, `traces` | true independently | Enable or disable each signal |
| `sampleRate` | `1` | Parent-based head sampling, `0` through `1` |
| `teeToStdout` | true | Also write Pino/Winston records locally |
| `exportIntervalMs` | 5000 | Metrics and log export interval |
| `runtimeMetricsIntervalMs` | 1000 | Event loop, heap, and active-handle collection interval |
| `captureUnhandledErrors` | false | Report unhandled rejections and observe uncaught exceptions |
| `profiling` | true | Collect and upload V8 CPU and sampled-heap pprof profiles |
| `profilingInterval` | 60000 | Cycle interval in milliseconds; each cycle includes a 10-second CPU window |
| `release`, `environment`, `tags` | unset | Fields copied to error events |

The Express middleware extracts W3C `traceparent` explicitly and records request count, duration, and active requests. Runtime collection uses `perf_hooks` and an unreferenced timer.

## Pino and Winston

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

Setting `captureUnhandledErrors: true` installs process listeners. It reports `unhandledRejection`; it observes `uncaughtException` without suppressing Node's normal fatal behavior. Prefer explicit handling when possible.

## Continuous profiling and replay

Profiling starts by default and can be configured explicitly:

```ts
const lo = init({
  dsn,
  serviceName: "checkout-api",
  profiling: true,
  profilingInterval: 60_000
});
```

The SDK uses a Node inspector session without opening a debug port. Each cycle samples CPU at 1 ms for 10 seconds and takes one sampled-heap profile, converts both to pprof, gzips them, and uploads them to `/api/profiles`. Upload failures back off to 15 minutes; inspector setup failure disables profiling after one warning. `shutdown()` stops the profiler. Set `profiling: false` when the process cannot tolerate profiling or when another inspector profiler owns the session.

Session Replay is browser-only.

See the runnable [Express example](../../sdk/node/examples/express/src/main.ts).
