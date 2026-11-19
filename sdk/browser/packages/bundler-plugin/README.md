# @lazy-observe/bundler-plugin

Uploads emitted source maps to lazy-observe. It ships a Vite plugin and a `lazy-observe-sourcemaps` CLI for every other bundler. The package has no runtime dependencies, is ESM only, and needs Node.js 22.15 or newer (`require()` of the package also works there).

Use one or the other, never both: a Vite project uses the plugin and does not call the CLI.

## DSN: use a source map upload token

The `dsn` must carry a **source map upload token** (`los_…`), not the project API token (`lo_…`) that browsers use: the upload endpoint answers a project API token with `403 source_map_upload_token_required`. An upload token can only upload source maps to its own project; it cannot ingest or read anything.

1. An admin (the global role that also manages project API tokens) creates the token in the web UI under **Settings → Projects → Manage tokens → Source map upload tokens**. The value is shown once, together with a ready-made DSN, `<origin>/p/<project>@los_…`.
2. Put that DSN in a CI secret (for example `LO_DSN`) and expose it only to the build job. Never commit it and never ship it to the browser; anyone holding it can replace the project's source maps. Revoke it from the same dialog if it leaks.
3. Keep the ordinary project DSN (`lo_…`) for `init({ dsn })` in the browser.

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

Options are `{ dsn, release, upload, stripSourceMaps }`. `release` defaults to the seven-character Git SHA of `HEAD`, `upload` defaults to `true`, and `stripSourceMaps` defaults to `false`. The plugin uploads after `vite build` has written the bundle, and defines `__LAZY_OBSERVE_RELEASE__`; pass it to SDK initialization so events and source maps share one release:

```ts
declare const __LAZY_OBSERVE_RELEASE__: string | undefined;
init({ dsn, release: __LAZY_OBSERVE_RELEASE__ });
```

The Git SHA is only needed to upload. Without a DSN, or with `upload: false`, a missing `.git` (for example a Docker build that excludes it) does not fail the build: `__LAZY_OBSERVE_RELEASE__` is then defined as `undefined`, which the browser SDK accepts (events carry no release). With a DSN and no `release` and no readable Git checkout, `vite build` fails before bundling with `unable to infer release from git; set release explicitly`: pass `release` (for example `release: process.env.RELEASE`). `vite dev` never fails on this.

## Other bundlers (webpack, Rollup, esbuild, Turbopack): CLI

```sh
lazy-observe-sourcemaps upload --dir <directory> [--dsn <dsn>] [--release <release>] [--strip]
```

`--dsn` defaults to `$LO_DSN` and `--release` defaults to `$LO_RELEASE`, then to the seven-character Git SHA. Without a DSN the release is never inferred, so the CLI still warns and exits 0 in a directory without `.git`; with a DSN and no release and no Git checkout it exits 1 with `unable to infer release from git; set release explicitly`. `--strip` deletes the local `.map` files after a successful upload. Pass the same release to the browser SDK, because the SDK does not read it from the build. Pick a value in your build (for example `LO_RELEASE=$(git rev-parse --short=7 HEAD)`) and give it to both the CLI and the code that calls `init({ release })`.

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

The DSN contains the source map upload token (`los_…`), a secret. In CI, store it as a secret and expose it to the build step as `LO_DSN` (for example `env: { LO_DSN: ${{ secrets.LO_DSN }} }` in GitHub Actions); do not commit it or put it on a command line that CI logs. When you build locally without `LO_DSN`, the plugin and the CLI print a warning, upload nothing, and succeed, so local builds never need credentials (or a Git checkout).

If a DSN is set but the upload fails (bad DSN, rejected token, server unreachable, duplicate basenames), the Vite build fails and the CLI exits with status 1. Usage errors exit with 2.

## Programmatic use

```ts
import { uploadSourceMaps } from "@lazy-observe/bundler-plugin";

await uploadSourceMaps({ dsn, release, dir: "dist", stripSourceMaps: false });
```

It resolves with `{ uploaded, skipped?, release? }`, and rejects when the upload fails. A missing or empty `dsn` warns and resolves with `skipped: "no-dsn"`.

## Wire format

Each multipart field name is the emitted minified basename (`app.js`), and its body is the corresponding `app.js.map`, sent to `POST /api/releases/:release/sourcemaps` with `Authorization: Bearer <token>`, where `<token>` is the source map upload token. Basenames must be unique across the output tree because server lookup intentionally matches browser frame basenames.
