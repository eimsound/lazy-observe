#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
results=${LO_PERF_OUTPUT_DIR:-"$root/bench/results"}
native="$(go env GOOS)-$(go env GOARCH)"
expected=(
  ingest.json
  release-linux-amd64.json
  release-linux-arm64.json
  "release-smoke-$native.json"
  query.json
  scale.json
)

for file in "${expected[@]}"; do
  path="$results/$file"
  if [[ ! -f $path ]]; then
    printf 'missing performance result: %s\n' "$path" >&2
    exit 1
  fi
  if ! jq -e '.gate_passed == true' "$path" >/dev/null; then
    printf 'performance result did not pass: %s\n' "$path" >&2
    exit 1
  fi
done

printf 'PASS: all %d performance result files exist and passed\n' "${#expected[@]}"
