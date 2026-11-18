# lazy-observe browser SDK

Framework-independent error capture, Web Vitals, distributed tracing and privacy-first Session Replay for a self-hosted lazy-observe gateway.

## Quick start

```ts
import { init } from "@lazy-observe/browser";

init({
  dsn: "http://host:3100/p/myapp@token",
  replay: { onErrorOnly: true }
});
```

Importing either package has no global side effects. `init()` installs the configured browser listeners and wrappers, and `client.close()` restores them. Every ingest request authenticates with `Authorization: Bearer <token>`; tokens are never put in URLs. Because the Beacon API cannot set that required header, unload delivery uses `fetch` with `keepalive` instead of `navigator.sendBeacon`.

## Configuration

| Option | Default | Meaning |
|---|---:|---|
| `dsn` | required | `<origin>/p/<project>@<token>` |
| `errors` | `true` | Capture `error` and `unhandledrejection`, batch and upload |
| `vitals` | `true` | Report LCP, CLS, INP, TTFB and FCP |
| `fetchTracing` | `true` | Patch fetch and XHR; use `{autoPatch:false}` for explicit `wrapFetch()` only |
| `fetchTracing.allowlist` | `[]` | Extra cross-origin URL prefixes or regular expressions; same-origin is always allowed |
| `replay` | enabled | `false` disables replay; see sampling below |
| `release`, `environment` | unset | Copied to error events |
| `beforeSend` | unset | Modify an error event, or return null/undefined to drop it |
| `errorLimitPerMinute` | `60` | Per-page storm protection |
| `errorBatchSize` | `10` | Flush queue at this many events |
| `errorFlushIntervalMs` | `5000` | Periodic error queue flush |
| `delivery.errorQueueCapacity` | `100` | Maximum queued plus in-flight error events |
| `delivery.traceQueueCapacity` | `1000` | Maximum queued plus in-flight browser spans |
| `delivery.replayQueueCapacity` | `10` | Maximum queued plus in-flight Replay segments |
| `delivery.diagnosticQueueCapacity` | `20` | Maximum queued plus in-flight drop measurements |
| `delivery.maxAttempts` | `3` | Total attempts per custom batch |
| `delivery.retryInitialMs`, `delivery.retryMaxMs` | `100`, `2000` | Capped exponential retry delay |

### Replay sampling and privacy

Replay defaults to `{sampleRate: 0.1, onErrorFullCapture: true}`. Sampled sessions upload throughout the session every 10 seconds or 500 KB. Unsampled sessions retain a local 60-second ring only; an error uploads that look-back with `X-LO-Replay-Error: 1`. `onErrorOnly: true` disables normal sampling but keeps error look-back. `replay: false` disables all recording.

rrweb is loaded asynchronously only after `init()`. All input values are masked (`maskAllInputs`), and `[data-lo-block]` subtrees are blocked by default. Set `blockSelector` to a stricter application selector. Replay uses `CompressionStream('gzip')` when available; identity fallback is marked by `X-LO-Replay-Compression: identity`.

Replay also adds structured timeline markers for captured errors, instrumented fetch/XHR requests, and Web Vitals. Request markers contain only method, URL path, status, and duration; origins, credentials, query strings, fragments, request bodies, and response bodies are never recorded. Vital markers contain only `name`, `value`, and the `web-vitals` rating. Markers observed while rrweb is loading are queued until recording starts.

### Trace behavior

Automatic fetch/XHR wrapping adds a sampled W3C header `00-<32hex>-<16hex>-01` to same-origin and configured allowlist targets. Existing fetch `traceparent` headers are preserved. Cross-origin injection requires the target CORS policy to allow `traceparent`.

Use `startSpan()` to define the causal scope of a user action. The span is active for a synchronous callback or until an asynchronous callback settles. Errors captured in that scope use the action context. Instrumented fetch/XHR requests use the same trace ID with a new request span ID; a request never replaces the action context used by errors.

```ts
import { captureException, startSpan } from "@lazy-observe/browser";

await startSpan({ name: "submit checkout" }, async (span) => {
  await fetch("/api/checkout");
  captureException(new Error("checkout rendering failed"));
  console.log(span.context.trace_id);
});
```

Nested callbacks inherit their parent trace. Overlapping top-level callbacks use separate traces. If multiple independent asynchronous scopes are pending and the currently executing scope cannot be identified without a browser-wide async hook, the SDK omits implicit trace context instead of guessing the most recent scope. Preserve the callback's `span.context` and pass it explicitly for framework work that captures an error later:

```ts
let checkoutTrace;
await startSpan({ name: "submit checkout" }, async (span) => {
  checkoutTrace = span.context;
  await scheduleFrameworkUpdate();
});

captureException(new Error("delayed framework failure"), { trace: checkoutTrace });
```

After a callback resolves, rejects, or throws, its context is removed. An unrelated later error is never attached to the last completed request or action.

For explicit instrumentation:

```ts
const client = init({ dsn, fetchTracing: { autoPatch: false } });
const tracedFetch = client.wrapFetch(window.fetch.bind(window));
```

### Delivery reliability

Errors, browser spans, Replay segments, and SDK drop diagnostics use separate
bounded in-memory queues. Capacity includes the batch currently being exported;
enqueue never waits for the network. A full queue drops new telemetry, and a
failed batch (including a drained Replay segment) remains resident until its
finite retry budget is exhausted. Web Vitals skip the queues and are posted once
per metric; a failed upload is counted and reported like a queue drop, with
reason `server_rejected` for a 4xx and `retry_exhausted` for a network error or
5xx (single attempt). Each drop emits a local `console.warn`.

`client.diagnostics().droppedTelemetry` returns the page-local total. Drop
measurements use stable IDs and are sent to the dedicated SDK diagnostic ingest,
which writes `lazy_observe_sdk_dropped_telemetry`; they are not disguised as Web
Vitals. Diagnostic queue failure is counted locally without recursively creating
another diagnostic.

`pagehide` calls `flush()` with Fetch `keepalive` as best effort only. It does
not claim durable or guaranteed unload delivery. `close()` drains finite retry
budgets, restores installed hooks, and uses no disk spool.

### React

```tsx
import { ErrorBoundary, useRouteTracking } from "@lazy-observe/react";

function Route({ pathname }: { pathname: string }) {
  useRouteTracking(pathname);
  return <Page />;
}

<ErrorBoundary fallback={(error) => <p>{error.message}</p>}><App /></ErrorBoundary>
```

`useRouteTracking()` can also run without an argument to watch browser back/forward and hash changes. Pass your router's current pathname for client-side push navigation. Page values are normalized by removing query and hash.

## Public API

`@lazy-observe/browser` exports `init`, `startSpan`, `captureException`, `setPage`, `wrapFetch`, configuration/payload/diagnostic types, and the independently testable `parseDsn`, `parseStack`, `isInApp`, trace helpers, rate limiter and replay ring buffer. The client returned by `init` exposes `sessionId`, `startSpan`, `captureException`, `wrapFetch`, `setPage`, `diagnostics`, `flush`, and `close`.

`@lazy-observe/react` exports `ErrorBoundary` and `useRouteTracking`. `withProfiler` is intentionally outside this delivery.

## Development

```sh
mise exec -- pnpm install
mise exec -- pnpm build
mise exec -- pnpm test
LO_E2E_PLATFORM_URL=http://127.0.0.1:3100 \
LO_E2E_TOKEN=lo_... \
LO_E2E_ADMIN_PASSWORD=... \
  mise exec -- ./e2e.sh
```

`./e2e.sh` is a manual journey: neither `mise` nor CI runs it (`mise run sdk-browser`
and the CI SDK job only run unit tests). Run it yourself against an already running
lazy-observe platform. It builds and starts the Go Fiber and Node Express examples
plus both Vite examples, then uses Chromium to verify cross-origin error, Web Vitals
and Replay ingest, W3C trace propagation, React ErrorBoundary capture, and
platform-side query persistence.

- `LO_E2E_PLATFORM_URL`: base URL of the running platform.
- `LO_E2E_TOKEN`: the `default` project's API token (`lo_…`). It only builds the
  ingest DSN; project tokens are ingest-only (ADR 0010) and get `403` on read routes.
- `LO_E2E_ADMIN_PASSWORD`: the `admin` password. Every read in the journey uses the
  admin session cookie plus `X-LO-Project: default`.

On a fresh data directory the platform writes the bootstrap admin password to
`<data_dir>/initial-admin-password` (mode 0600) and logs only that path, plus the
`default` project token once on first start. The script also runs
`playwright install --with-deps`, which installs system packages through the OS
package manager.

See `examples/vanilla` and `examples/react` for Vite demos that can report to a
local gateway.
