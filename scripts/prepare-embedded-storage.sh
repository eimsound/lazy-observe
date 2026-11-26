#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
goos=${GOOS:-$(go env GOOS)}
goarch=${GOARCH:-$(go env GOARCH)}
platform="$goos-$goarch"
source_dir=${LO_STORAGE_BIN_DIR:-"$root/.cache/storage-bin/$platform"}
# 两个平台与单平台 storage-bins 共用 .cache/storage-bin/.archives/<platform>，宿主平台归档只存一份（#378）。
archive_root=${LO_STORAGE_ARCHIVE_DIR:-"$root/.cache/storage-bin/.archives"}
target_dir="$root/internal/embed/bins/assets/$platform"

case "$platform" in
  linux-amd64|linux-arm64) ;;
  *) printf 'unsupported embedded storage platform: %s\n' "$platform" >&2; exit 1 ;;
esac

LO_STORAGE_BIN_DIR="$source_dir" LO_STORAGE_ARCHIVE_DIR="$archive_root" GOOS="$goos" GOARCH="$goarch" \
  bash "$root/scripts/download-storage-bins.sh"

mkdir -p "$target_dir"
find "$target_dir" -maxdepth 1 -type f ! -name .gitkeep -delete

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  printf 'missing required SHA-256 tool: sha256sum or shasum\n' >&2
  exit 1
fi

binaries=(victoria-metrics-prod victoria-logs-prod victoria-traces-prod vmagent-prod)
manifest="$target_dir/manifest.tsv"
: >"$manifest"
for binary in "${binaries[@]}"; do
  source_path="$source_dir/$binary"
  [[ -x $source_path ]] || { printf 'missing executable storage binary: %s\n' "$source_path" >&2; exit 1; }
  gzip -9 -n -c "$source_path" >"$target_dir/$binary.gz"
  size=$(wc -c <"$source_path" | tr -d ' ')
  printf '%s\t%s\t%s\n' "$binary" "$size" "$(sha256 "$source_path")" >>"$manifest"
done
printf 'prepared compressed embedded storage payloads for %s in %s\n' "$platform" "$target_dir"
