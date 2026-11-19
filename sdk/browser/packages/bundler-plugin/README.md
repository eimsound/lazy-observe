# @lazy-observe/bundler-plugin

Uploads emitted source maps to lazy-observe. It ships a Vite plugin and a `lazy-observe-sourcemaps` CLI for every other bundler. The package has no runtime dependencies, is ESM only, and needs Node.js 22.15 or newer (`require()` of the package also works there).

Use one or the other, never both: a Vite project uses the plugin and does not call the CLI.

## Vite

```ts
// vite.config.ts
import { defineConfig } from "vite";
import { vite as lazyObserve } from "@lazy-observe/bundler-plugin";

export default defineConfig({
  build: { sourcemap: true },
  plugins: [lazyObserve({ dsn: process.env.LO_DSN })]
});
```

Options are `{ dsn, release, upload, stripSourceMaps }`. `release` defaults to the seven-character Git SHA, `upload` defaults to `true`, and `stripSourceMaps` defaults to `false`. The plugin uploads after `vite build` has written the bundle, and defines `__LAZY_OBSERVE_RELEASE__`; pass it to SDK initialization so events and source maps share one release:

```ts
declare const __LAZY_OBSERVE_RELEASE__: string;
init({ dsn, release: __LAZY_OBSERVE_RELEASE__ });
```

## Other bundlers (webpack, Rollup, esbuild, Turbopack): CLI

```sh
lazy-observe-sourcemaps upload --dir <directory> [--dsn <dsn>] [--release <release>] [--strip]
```

`--dsn` defaults to `$LO_DSN` and `--release` defaults to `$LO_RELEASE`, then to the seven-character Git SHA. `--strip` deletes the local `.map` files after a successful upload. Pass the same release to the browser SDK, because the SDK does not read it from the build. Pick a value in your build (for example `LO_RELEASE=$(git rev-parse --short=7 HEAD)`) and give it to both the CLI and the code that calls `init({ release })`.

### Next.js

Next.js 16 builds with Turbopack by default, so a webpack plugin would not run. Emit browser source maps and upload them after the build:

```ts
// next.config.ts
import type { NextConfig } from "next";

const config: NextConfig = {
  productionBrowserSourceMaps: true,
  env: { NEXT_PUBLIC_LO_RELEASE: process.env.LO_RELEASE }
};
export default config;
```

```jsonc
// package.json
{
  "scripts": {
    "build": "next build",
    "postbuild": "lazy-observe-sourcemaps upload --dir .next/static --strip"
  }
}
```

```ts
// browser entry
init({ dsn, release: process.env.NEXT_PUBLIC_LO_RELEASE });
```

npm and pnpm run `postbuild` automatically after `build`. Yarn 2 and newer do not run `pre`/`post` scripts, so with Yarn chain the command in `build` instead (`next build && lazy-observe-sourcemaps upload --dir .next/static --strip`). Run the build with the release set, for example `LO_RELEASE=$(git rev-parse --short=7 HEAD) npm run build`. `--strip` removes the uploaded `.map` files from `.next/static` so they are not served publicly.

## Credentials and skipping

The DSN contains the upload token. In CI, store it as a secret and expose it to the build step as `LO_DSN` (for example `env: { LO_DSN: ${{ secrets.LO_DSN }} }` in GitHub Actions); do not commit it or put it on a command line that CI logs. When you build locally without `LO_DSN`, the plugin and the CLI print a warning, upload nothing, and succeed, so local builds never need credentials.

If a DSN is set but the upload fails (bad DSN, rejected token, server unreachable, duplicate basenames), the Vite build fails and the CLI exits with status 1. Usage errors exit with 2.

## Programmatic use

```ts
import { uploadSourceMaps } from "@lazy-observe/bundler-plugin";

await uploadSourceMaps({ dsn, release, dir: "dist", stripSourceMaps: false });
```

It resolves with `{ uploaded, skipped?, release? }`, and rejects when the upload fails. A missing or empty `dsn` warns and resolves with `skipped: "no-dsn"`.

## Wire format

Each multipart field name is the emitted minified basename (`app.js`), and its body is the corresponding `app.js.map`, sent to `POST /api/releases/:release/sourcemaps` with `Authorization: Bearer <token>`. Basenames must be unique across the output tree because server lookup intentionally matches browser frame basenames.
