# Browser and React SDKs

The browser SDK captures JavaScript errors, Web Vitals, W3C trace context, and privacy-first session replay. The React adapter adds an `ErrorBoundary` and route tracking. Calling `init()` installs global listeners and fetch/XHR wrappers, and calling `close()` restores them.

## Install

The packages live in the repository pnpm workspace and are not yet published to a package registry. From `sdk/browser`, run:

```sh
mise exec -- pnpm install --frozen-lockfile
mise exec -- pnpm build
```

Workspace examples resolve `@lazy-observe/browser`, `@lazy-observe/react`, and `@lazy-observe/bundler-plugin` directly.

## Initialize

```ts
import { init } from "@lazy-observe/browser";

const client = init({
  dsn: "http://127.0.0.1:3100/p/default@TOKEN",
  release: "2026.07.17",
  environment: "production"
});
```

Call `await client.close()` when the owning application tears down. A second `init()` call closes the previous module-level client.

## Configuration and capability switches

| Option | Default | Behavior |
| --- | --- | --- |
| `dsn` | required | `<origin>/p/<project>@<token>`. This `lo_...` project API token ships inside the public page, so any visitor can read it. It can only ingest, and read routes reject it with `403 ingest_only_token`. Querying requires a `lou_...` user API token and the `X-LO-Project` header. See the [deployment guide](../deployment.md#query-api-mcp-and-grafana). |
| `errors` | `true` | Window error and unhandled rejection capture. |
| `vitals` | `true` | LCP, CLS, INP, TTFB, and FCP with low-cardinality `device_type`, `browser_family`, and `os_family` dimensions. Object form `{ country: "US" }` attaches an explicit 2-letter ISO country code. |
| `country` | unset | Optional explicit 2-letter ISO 3166-1 alpha-2 country code attached to Web Vitals. Never inferred from locale or timezone. Update at runtime with `setCountry()`. |
| `fetchTracing` | `true` | Patch fetch and XHR. Object form controls patching and allowlist. |
| `replay` | enabled | Boolean or replay configuration. |
| `release`, `environment` | unset | Error event fields. |
| `beforeSend` | unset | Modify or drop an error event. |
| `errorLimitPerMinute` | `60` | Per-page storm limit. |
| `errorBatchSize` | `10` | Event batch size. |
| `errorFlushIntervalMs` | `5000` | Periodic event flush interval in milliseconds. |

Disable selected capabilities explicitly:

```ts
init({
  dsn,
  vitals: false,
  replay: false,
  fetchTracing: { autoPatch: false }
});
```

### Web Vitals dimensions

Web Vitals uploads attach low-cardinality dimensions (`device_type`, `browser_family`, and `os_family`) normalized into fixed enums. The SDK never sends raw User-Agent strings or IP addresses as metric labels. It also never guesses geography from browser locale or timezone. Omit `country` so a gateway behind `security.trusted_proxies` derives it from edge headers (`CF-IPCountry`, `CloudFront-Viewer-Country`, `X-Vercel-IP-Country`, `X-Country-Code`). Alternatively, pass an explicit 2-letter ISO 3166-1 alpha-2 code through `init({ country: "US" })`, `init({ vitals: { country: "US" } })`, `client.setCountry("US")`, or `setCountry("US")`. Pass `undefined` to clear it.

## Error reporting

```ts
const eventId = client.captureException(error, {
  level: "warning",
  tags: { operation: "checkout" }
});
```

The module-level `captureException()` targets the latest initialized client. `beforeSend` may return a changed event, or return `null` or `undefined` to drop it. Events carry browser and OS context, the current replay session, the URL, and active SDK-created trace context when available.

## Telemetry drop diagnostics

Errors, spans, Web Vitals, replay segments, and drop diagnostics use bounded, protocol-aware in-memory queues. These queues drop permanent 4xx rejections immediately as `server_rejected`, retry `408`, `429`, `5xx`, and transient failures with `Retry-After` or Full Jitter exponential backoff, enforce per-attempt timeouts, and split batches by byte budget (and `<= 32` distinct pages for Web Vitals). Unsent Web Vitals are deduplicated by `(name, page)` keeping the latest value; batch splitting stays under the 64 KiB `keepalive` cap on page unload (with `visibilitychange`/`pagehide` flushing synchronously even if an async retry is in flight) and aligns with the 200 KiB error-event limit.

Before queuing an error event, the built-in `sanitize` pipeline strips URL credentials, query strings, and fragments, and caps oversized fields and stack frames. The SDK never throws an ingest failure into the page. Every drop increments `client.diagnostics().droppedTelemetry`, emits one local `console.warn`, and reports to `/api/events/diagnostics`.

| `reason` | Meaning |
| --- | --- |
| `queue_full` | A bounded queue was full and the SDK dropped the new telemetry. |
| `retry_exhausted` | The finite delivery budget ran out after exhausting configured retry attempts. |
| `shutdown_budget` | Telemetry was enqueued after `close()`. |
| `server_rejected` | Ingest returned 4xx, excluding 408 and 429 which are transient. The payload will never be accepted as-is, so the SDK does not retry it. |

The reported `signal` is `error`, `trace`, `vitals`, `replay`, or `diagnostic`. A failed diagnostics upload is counted locally and never reported recursively.

Older servers that reject an unknown `signal` or `reason` return `422` for the whole diagnostics batch. Because of that, the server also rejects measurements it does recognize in the same batch, such as `error` with `queue_full`. That batch stays queued and retries within the diagnostic queue's finite delivery budget with backoff. When the budget runs out, the SDK drops the whole batch, increases `droppedTelemetry` by the batch size, and records the loss locally as `diagnostic` with `retry_exhausted` so it cannot recurse. The SDK still degrades silently with one `console.warn` and throws nothing.

## Fetch and XHR tracing

Same-origin requests are eligible by default. Add explicit cross-origin destinations and configure their CORS policy to allow `traceparent`:

```ts
init({
  dsn,
  fetchTracing: {
    allowlist: ["https://api.example.com/", /^https:\/\/edge\d+\.example\.com\//]
  }
});
```

Wrap `fetch` explicitly without global patching:

```ts
const client = init({ dsn, fetchTracing: { autoPatch: false } });
const tracedFetch = client.wrapFetch(window.fetch.bind(window));
```

The wrapper preserves existing `traceparent` fetch headers without creating a duplicate client span. When no `traceparent` header is present, the wrapper generates a sampled W3C context for propagation and error correlation and exports a browser client span to `/v1/traces`.

## Session replay

Replay defaults to a 10% normal-session sample plus full error look-back. Sampled sessions flush every 10 seconds or 500 KiB. Unsampled sessions keep a 60-second in-memory ring and upload it after an error.

```ts
init({
  dsn,
  replay: {
    sampleRate: 0.05,
    onErrorFullCapture: true,
    bufferMs: 60_000,
    blockSelector: "[data-private]"
  }
});
```

Setting `onErrorOnly: true` sets normal sampling to zero while keeping error look-back. Setting `replay: false` disables recording. `rrweb` is an optional peer dependency of `@lazy-observe/browser`. Install `rrweb` when you enable session replay. If `rrweb` is absent, replay logs one warning and degrades silently without disrupting error or trace collection. By default, `rrweb` masks all inputs and blocks `[data-lo-block]`. A custom `blockSelector` replaces that selector. Replay loads `rrweb` asynchronously and uses gzip when `CompressionStream` is available.

## React

`@lazy-observe/react` declares `@lazy-observe/browser` and `react` as peer dependencies so applications control one shared browser SDK instance.

```tsx
import { ErrorBoundary, useRouteTracking } from "@lazy-observe/react";

function RoutePage({ pathname }: { pathname: string }) {
  useRouteTracking(pathname);
  return <Page />;
}

<ErrorBoundary fallback={(error) => <p>{error.message}</p>}>
  <App />
</ErrorBoundary>
```

Call `useRouteTracking()` without an argument to listen for browser back, forward, and hash changes. Pass the router pathname for client-side push navigation. The SDK removes query strings and fragments from recorded page values.

Browser profiling and React `withProfiler` are not implemented.

## Source maps

Uploading source maps requires a source map upload token (`los_...`), not the project API token that browsers use. `@lazy-observe/bundler-plugin` validates the `los_` prefix locally before uploading and rejects `lo_...` and `lou_...` tokens without echoing the token. The upload endpoint also rejects project API tokens with `403 source_map_upload_token_required`. An upload token can only upload source maps to its own project. It cannot ingest or read anything.

1. An admin, the global role that also manages project API tokens, opens Settings, selects Projects, selects Manage tokens, opens Source map upload tokens, creates a token, and copies the one-time DSN (`<origin>/p/<project>@los_...`).
2. Store that DSN in a CI secret, such as `LO_DSN`, and expose it only to the build job. Never put it in a page, a bundle, or a committed file. Revoke the token from the same dialog if it leaks.
3. Keep using the ordinary project DSN (`lo_...`) in `init({ dsn })` for the browser.

`@lazy-observe/bundler-plugin` uploads emitted source maps. It is ESM-only, has no runtime dependencies, and requires Node.js 22.15 or newer. Vite projects use the plugin. All other bundlers use the `lazy-observe-sourcemaps` CLI. Do not use both in one project.

```ts
import { defineConfig } from "vite";
import { vite as lazyObserve } from "@lazy-observe/bundler-plugin";

export default defineConfig({
  build: { sourcemap: true },
  plugins: [lazyObserve({
    dsn: process.env.LO_DSN,
    release: process.env.RELEASE
  })]
});
```

Plugin options:

| Option | Default | Behavior |
| --- | --- | --- |
| `dsn` | unset | Gateway DSN carrying a source map upload token (`los_...`) from a CI secret. When missing or empty, the build prints a warning, uploads nothing, and succeeds. |
| `release` | seven-character Git SHA | Release key used for upload and symbolication. It is needed only for uploading, so without a DSN a missing `.git` directory does not fail the build. With a DSN, no release, and no Git checkout, `vite build` fails with `unable to infer release from git; set release explicitly`. |
| `upload` | `true` | Upload emitted `.map` files after build. |
| `stripSourceMaps` | `false` | Delete local `.map` files after a successful upload, or when upload is skipped without a DSN. Keeps local `.map` files if a configured upload fails. |

Vite defines `__LAZY_OBSERVE_RELEASE__`. Pass that value to browser initialization:

```ts
declare const __LAZY_OBSERVE_RELEASE__: string | undefined;
init({ dsn, release: __LAZY_OBSERVE_RELEASE__ });
```

If Vite cannot infer a release and nothing requires one (no DSN, `upload: false`, or `vite dev`), Vite defines `__LAZY_OBSERVE_RELEASE__` as `undefined`, so events carry no release.

### Other bundlers and Next.js

```sh
lazy-observe-sourcemaps upload --dir <directory> [--dsn <dsn>] [--release <release>] [--strip]
```

`--dsn` defaults to `$LO_DSN`. `--release` defaults to `$LO_RELEASE`, then the seven-character Git SHA. With a DSN, no release, and no Git checkout, the CLI exits with status 1. Without a DSN, it never infers a release. Because the browser SDK does not read the release from the build, pass the same release to `init({ release })`. Next.js 16 builds with Turbopack by default, so use the CLI there. Set `productionBrowserSourceMaps: true` in `next.config` and run `lazy-observe-sourcemaps upload --dir .next/static --strip` as the `postbuild` script. See the [bundler-plugin README](https://github.com/eimsound/lazy-observe/blob/main/sdk/browser/packages/bundler-plugin/README.md) for the full example.

In CI, provide the upload-token DSN from a secret such as `LO_DSN`. Without a DSN, for example in a local build, the plugin and the CLI warn, skip the upload, and succeed. When a DSN is set and the upload fails, the Vite build fails and the CLI exits non-zero.

The server matches source maps by minified basename. Basenames must be unique across the output tree, and the plugin and CLI reject duplicates.
