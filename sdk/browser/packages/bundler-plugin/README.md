# @lazy-observe/bundler-plugin

Uploads emitted source maps to lazy-observe from one `unplugin` implementation for Vite and Webpack.

```ts
// vite.config.ts
import { defineConfig } from "vite";
import { vite as lazyObserve } from "@lazy-observe/bundler-plugin";

export default defineConfig({
  build: { sourcemap: true },
  plugins: [lazyObserve({ dsn: process.env.LO_DSN! })]
});
```

Webpack uses `import { webpack as lazyObserve } from "@lazy-observe/bundler-plugin"` in `plugins`.

Options are `{ dsn, release, upload, stripSourceMaps }`. `release` defaults to the seven-character Git SHA, `upload` defaults to `true`, and `stripSourceMaps` defaults to `false`. The Vite adapter defines `__LAZY_OBSERVE_RELEASE__`; pass it manually to SDK initialization:

```ts
declare const __LAZY_OBSERVE_RELEASE__: string;
init({ dsn, release: __LAZY_OBSERVE_RELEASE__ });
```

Each multipart field name is the emitted minified basename (`app.js`), and its body is the corresponding `app.js.map`. Basenames must be unique across the output tree because server lookup intentionally matches browser frame basenames.
