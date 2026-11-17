#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
platform="$(go env GOOS)-$(go env GOARCH)"
storage_dir="$root/.cache/storage-bin/$platform"

for binary in victoria-metrics-prod victoria-logs-prod victoria-traces-prod vmagent-prod; do
  if [[ ! -x "$storage_dir/$binary" ]]; then
    printf 'missing development storage binary: %s\nrun `mise run dev-storage` first\n' \
      "$storage_dir/$binary" >&2
    exit 1
  fi
done

export LO_SELF_MONITORING_ENABLED="${LO_DEV_SELF_MONITORING:-true}"

exec "$root/.cache/air/lazy-observe" server \
  --listen "${LO_DEV_LISTEN:-0.0.0.0:3100}" \
  --data-dir "${LO_DEV_DATA_DIR:-$root/data}" \
  --serve-web-ui=false \
  --storage-bin-dir "$storage_dir" \
  --storage-shutdown-timeout "${LO_DEV_STORAGE_SHUTDOWN_TIMEOUT:-5s}" \
  --drain-timeout "${LO_DEV_DRAIN_TIMEOUT:-5s}"
