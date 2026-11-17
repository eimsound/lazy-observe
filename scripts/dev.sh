#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

pids=()

cleanup() {
  local status=${1:-0} pid alive deadline
  trap - EXIT HUP INT TERM

  if (( ${#pids[@]} > 0 )); then
    printf '\nStopping development services...\n'
    for pid in "${pids[@]}"; do
      kill -INT "$pid" 2>/dev/null || true
    done

    deadline=$((SECONDS + 25))
    while (( SECONDS < deadline )); do
      alive=0
      for pid in "${pids[@]}"; do
        kill -0 "$pid" 2>/dev/null && alive=1
      done
      (( alive == 0 )) && break
      sleep 0.2
    done

    for pid in "${pids[@]}"; do
      kill -TERM "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    done
  fi

  exit "$status"
}

trap 'cleanup $?' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

mise run dev-backend &
pids+=("$!")
mise run dev-web &
pids+=("$!")

# LO_DEV_HOST: 打印给浏览器用的主机名（远程开发机上设成它的地址），默认 localhost。
dev_host=${LO_DEV_HOST:-localhost}
printf 'Development services are starting:\n'
printf '  Web UI (Vite HMR): http://%s:5173\n' "$dev_host"
printf '  Gateway API only:  http://%s:3100\n' "$dev_host"
printf 'Press Ctrl-C to stop both services and the managed VM suite.\n'

while :; do
  for pid in "${pids[@]}"; do
    if ! kill -0 "$pid" 2>/dev/null; then
      if wait "$pid"; then
        exit 0
      else
        exit $?
      fi
    fi
  done
  sleep 1
done
