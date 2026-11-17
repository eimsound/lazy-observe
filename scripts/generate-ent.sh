#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
target="$root/internal/db/ent"
work=$(mktemp -d "$root/internal/db/.ent.tmp.XXXXXX")
previous="$root/internal/db/.ent.previous.$$"
cleanup() {
  status=$?
  rm -rf "$work"
  if [[ -d "$previous" && ! -d "$target" ]]; then
    mv "$previous" "$target"
  else
    rm -rf "$previous"
  fi
  exit "$status"
}
trap cleanup EXIT

# Resolve Ent code-generation dependencies through an alternate module file.
# The pinned generator may use Atlas internally, but neither the production
# go.mod nor generated runtime code can import its migration packages.
host_goos=$(go env GOHOSTOS)
host_goarch=$(go env GOHOSTARCH)
(
  cd "$root/internal/db"
  env -u GOAMD64 -u GOARM64 CGO_ENABLED=0 GOOS="$host_goos" GOARCH="$host_goarch" \
    go run -mod=readonly -modfile="$root/entc.mod" \
    "$root/tools/entgen/main.go" ./schema "$work" \
    github.com/eimsound/lazy-observe/internal/db/ent
)

if [[ -d "$work/migrate" || -d "$work/enttest" ]]; then
  printf 'Ent generation recreated an automatic migration package\n' >&2
  exit 1
fi

if [[ -d "$target" ]]; then
  mv "$target" "$previous"
fi
mv "$work" "$target"
work=""
rm -rf "$previous"
trap - EXIT
