#!/bin/sh
set -eu
cd "$(dirname "$0")"
pnpm dlx terser lazy-observe-original.js --module --compress --mangle \
  --source-map "filename='app.min.js',url='app.min.js.map',includeSources" \
  -o app.min.js
