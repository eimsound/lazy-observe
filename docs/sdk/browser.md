# Browser and React SDKs

The browser SDK captures JavaScript errors, Web Vitals, W3C trace context, and privacy-first Session Replay. The React adapter adds an ErrorBoundary and route tracking. Global listeners and fetch/XHR wrappers are installed only by `init()` and restored by `close()`.

## Install

The packages currently live in the repository pnpm workspace and are not documented as published registry releases. From `sdk/browser`:

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

Call `await client.close()` when the owning application is torn down. A second `init()` closes the previous module-level client.

## Configuration and capability switches

| Option | Default | Behavior |
| --- | --- | --- |
| `dsn` | required | `<origin>/p/<project>@<token>` |
| `errors` | true | Window error and unhandled rejection capture |
| `vitals` | true | LCP, CLS, INP, TTFB, FCP |
| `fetchTracing` | true | Patch fetch and XHR; object form controls patching/allowlist |
| `replay` | enabled | Boolean or replay configuration |
| `release`, `environment` | unset | Error event fields |
| `beforeSend` | unset | Modify or drop an error event |
| `errorLimitPerMinute` | 60 | Per-page storm limit |
| `errorBatchSize` | 10 | Event batch size |
| `errorFlushIntervalMs` | 5000 | Periodic event flush |

Disable selected capabilities explicitly:

```ts
init({
  dsn,
  vitals: false,
  replay: false,
  fetchTracing: { autoPatch: false }
});
```

## Error reporting

```ts
const eventId = client.captureException(error, {
  level: "warning",
  tags: { operation: "checkout" }
});
```

The module-level `captureException()` targets the latest initialized client. `beforeSend` may return a changed event or `null`/`undefined` to drop it. Events carry browser/OS context, the current replay session, URL, and active SDK-created trace context when available.

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

For explicit fetch wrapping without global patching:

```ts
const client = init({ dsn, fetchTracing: { autoPatch: false } });
const tracedFetch = client.wrapFetch(window.fetch.bind(window));
```

Existing `traceparent` fetch headers are preserved. The wrapper does not create/export browser spans; it creates a sampled W3C context for propagation and error correlation.

## Session Replay

Replay defaults are a 10% normal-session sample plus full error look-back. Sampled sessions flush every 10 seconds or 500 KiB. Unsampled sessions retain a 60-second in-memory ring and upload it after an error.

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

`onErrorOnly: true` sets normal sampling to zero while preserving error look-back. `replay: false` disables recording. rrweb masks all inputs and blocks `[data-lo-block]` by default; a custom `blockSelector` replaces that selector. Replay loads rrweb asynchronously and uses gzip when `CompressionStream` is available.

## React

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

Call `useRouteTracking()` without an argument to listen for browser back/forward and hash changes. Pass the router pathname for client-side push navigation. Query strings and fragments are removed from recorded page values.

Browser profiling and React `withProfiler` are not implemented.

## Source maps

The bundler plugin supports Vite and Webpack through one `unplugin` implementation.

```ts
import { defineConfig } from "vite";
import { vite as lazyObserve } from "@lazy-observe/bundler-plugin";

export default defineConfig({
  build: { sourcemap: true },
  plugins: [lazyObserve({
    dsn: process.env.LO_DSN!,
    release: process.env.RELEASE
  })]
});
```

Webpack uses `import { webpack as lazyObserve }` and places the returned value in `plugins`.

Plugin options:

| Option | Default | Behavior |
| --- | --- | --- |
| `dsn` | required | Gateway credentials |
| `release` | seven-character Git SHA | Release key used for upload and symbolication |
| `upload` | true | Upload emitted `.map` files after build |
| `stripSourceMaps` | false | Delete local `.map` files after successful upload |

Vite defines `__LAZY_OBSERVE_RELEASE__`; pass the same value to browser initialization:

```ts
declare const __LAZY_OBSERVE_RELEASE__: string;
init({ dsn, release: __LAZY_OBSERVE_RELEASE__ });
```

The server matches source maps by minified basename. Basenames must therefore be unique across the output tree; the plugin rejects duplicates.
