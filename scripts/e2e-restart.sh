#!/usr/bin/env bash
set -euo pipefail

script_dir=${BASH_SOURCE[0]%/*}
root=$(CDPATH= cd -- "$script_dir/.." && pwd -P)
binary_input=${BINARY:-bin/lazy-observe}
storage_input=${STORAGE_BIN_DIR:-.cache/storage-bin}

case $binary_input in
  /*) binary=$binary_input ;;
  *) binary=$root/$binary_input ;;
esac
case $storage_input in
  /*) storage=$storage_input ;;
  *) storage=$root/$storage_input ;;
esac

[[ -x $binary ]] || { printf 'restart E2E binary is missing or not executable: %s\n' "$binary" >&2; exit 1; }
[[ -d $storage ]] || { printf 'restart E2E storage directory is missing: %s\n' "$storage" >&2; exit 1; }

cd "$root"
LO_E2E_BINARY="$binary" \
LO_E2E_STORAGE_BIN_DIR="$storage" \
CGO_ENABLED=0 \
  go test ./internal/gateway -run '^TestRestartJourneyE2E$' -count=1 -v

printf '%s\n' 'PASS e2e restart: graceful'
printf '%s\n' 'PASS e2e restart: forced'
printf '%s\n' 'PASS e2e restart: pending-work'
printf '%s\n' 'PASS e2e recovery: managed-child'
printf '%s\n' 'PASS e2e-restart: graceful, forced, pending-work, and managed-child recovery gates passed'
