#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
results=${LO_PERF_OUTPUT_DIR:-"$root/bench/results"}
max_age_seconds=${LO_PERF_MAX_AGE_SECONDS:-21600}
expected_commit=${LO_PERF_EXPECTED_COMMIT:-}
if [[ -z $expected_commit ]]; then
  expected_commit=$(git -C "$root" rev-parse HEAD 2>/dev/null || true)
fi
case $max_age_seconds in
  ''|*[!0-9]*) printf 'LO_PERF_MAX_AGE_SECONDS must be a positive integer\n' >&2; exit 1 ;;
esac
(( max_age_seconds > 0 )) || { printf 'LO_PERF_MAX_AGE_SECONDS must be positive\n' >&2; exit 1; }

native="$(go env GOOS)-$(go env GOARCH)"
now_epoch=$(date -u +%s)
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
  generated_at=$(jq -er '.generated_at | select(type == "string" and length > 0)' "$path" 2>/dev/null) || {
    printf 'performance result is missing generated_at timestamp: %s\n' "$path" >&2
    exit 1
  }
  generated_epoch=$(date -u -d "$generated_at" +%s 2>/dev/null) || {
    printf 'performance result has invalid generated_at timestamp (%s): %s\n' "$generated_at" "$path" >&2
    exit 1
  }
  age=$((now_epoch - generated_epoch))
  if (( age > max_age_seconds || age < -300 )); then
    printf 'performance result is stale (age=%ds, max=%ds, generated_at=%s): %s\n' \
      "$age" "$max_age_seconds" "$generated_at" "$path" >&2
    exit 1
  fi
  if [[ -n $expected_commit ]]; then
    actual_commit=$(jq -er '.git_commit | select(type == "string" and length > 0)' "$path" 2>/dev/null) || {
      printf 'performance result is missing git_commit: %s\n' "$path" >&2
      exit 1
    }
    if [[ $actual_commit != "$expected_commit" ]]; then
      printf 'performance result commit mismatch (got %s, want %s): %s\n' \
        "$actual_commit" "$expected_commit" "$path" >&2
      exit 1
    fi
  fi
done

printf 'PASS: all %d performance result files exist, are fresh, match %s, and passed\n' \
  "${#expected[@]}" "${expected_commit:-<no-git>}"
