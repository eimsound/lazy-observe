#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
bin_dir=${LO_STORAGE_BIN_DIR:-"$root/.cache/storage-bin"}
goos=${GOOS:-$(go env GOOS)}
goarch=${GOARCH:-$(go env GOARCH)}
platform="$goos-$goarch"

case "$platform" in
  linux-amd64)
    metrics_sha=54409ea381db0316f0bf6f242c718bc498085eb1eb37520b73526e6d86891601
    vmutils_sha=d71f7f00e07782531000620ec6c2a9d7ec83324f364dd1f36095ab378b5fb43b
    logs_sha=55feba89713cafa952673f91b8b38e7701bb0290989263da671b47a0de16edd3
    traces_sha=789e006d076f3c8b99de7bb1d0277024a7db776db0294d1c27fcc5422d98c4bc
    ;;
  linux-arm64)
    metrics_sha=f777110b232bfdf435556c57d0896716e27000afafe969c4eb08852352dceb7d
    vmutils_sha=1a8e4ed55df2a59d5cd03056213f50d293d4fab029e38dd960597362d92cf176
    logs_sha=301b7ef12ff9fac7f70b155191affe07ac6212755514174461d775548699ef96
    traces_sha=210bc863666865ba9a4ff0a19901318042da0c474d248fec69ca71c07b1e5f7a
    ;;
  *)
    printf 'unsupported storage binary platform: %s (supported: linux x amd64/arm64)\n' "$platform" >&2
    exit 1
    ;;
esac

metrics_version=v1.156.0
logs_version=v1.53.0
traces_version=v0.13.0
metrics_asset="victoria-metrics-$platform-$metrics_version.tar.gz"
vmutils_asset="vmutils-$platform-$metrics_version.tar.gz"
logs_asset="victoria-logs-$platform-$logs_version.tar.gz"
traces_asset="victoria-traces-$platform-$traces_version.tar.gz"
metrics_url="https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/$metrics_version/$metrics_asset"
vmutils_url="https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/$metrics_version/$vmutils_asset"
logs_url="https://github.com/VictoriaMetrics/VictoriaLogs/releases/download/$logs_version/$logs_asset"
traces_url="https://github.com/VictoriaMetrics/VictoriaTraces/releases/download/$traces_version/$traces_asset"

print_entry() {
  printf '%s  %s\n' "$2" "$1"
  printf '  %s\n' "$3"
}

if [[ ${LO_STORAGE_DOWNLOAD_DRY_RUN:-0} == 1 ]]; then
  printf 'storage binary manifest for %s:\n' "$platform"
  print_entry "$metrics_asset" "$metrics_sha" "$metrics_url"
  print_entry "$vmutils_asset" "$vmutils_sha" "$vmutils_url"
  print_entry "$logs_asset" "$logs_sha" "$logs_url"
  print_entry "$traces_asset" "$traces_sha" "$traces_url"
  printf 'installs: victoria-metrics-prod victoria-logs-prod victoria-traces-prod vmagent-prod\n'
  exit 0
fi

for command in curl tar; do
  command -v "$command" >/dev/null 2>&1 || { printf 'missing required command: %s\n' "$command" >&2; exit 1; }
done
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  printf 'missing required SHA-256 tool: sha256sum or shasum\n' >&2
  exit 1
fi

archive_dir="$bin_dir/.archives/$platform"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-storage.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
mkdir -p "$archive_dir"

fetch() {
  local asset=$1 expected=$2 url=$3 archive actual
  archive="$archive_dir/$asset"
  if [[ ! -f $archive ]]; then
    printf 'downloading %s\n' "$url"
    curl -fL --retry 3 --retry-all-errors -o "$tmp_dir/$asset" "$url"
    mv "$tmp_dir/$asset" "$archive"
  else
    printf 'using cached %s\n' "$archive"
  fi
  actual=$(sha256 "$archive")
  if [[ $actual != "$expected" ]]; then
    printf 'checksum mismatch for %s: got %s, want %s\n' "$asset" "$actual" "$expected" >&2
    rm -f "$archive"
    exit 1
  fi
  printf 'verified SHA-256 %s\n' "$asset"
}

fetch "$metrics_asset" "$metrics_sha" "$metrics_url"
fetch "$vmutils_asset" "$vmutils_sha" "$vmutils_url"
fetch "$logs_asset" "$logs_sha" "$logs_url"
fetch "$traces_asset" "$traces_sha" "$traces_url"

manifest="$bin_dir/.manifest"
expected_manifest="$tmp_dir/manifest"
{
  printf 'platform=%s\n' "$platform"
  printf '%s %s\n' "$metrics_sha" "$metrics_asset"
  printf '%s %s\n' "$vmutils_sha" "$vmutils_asset"
  printf '%s %s\n' "$logs_sha" "$logs_asset"
  printf '%s %s\n' "$traces_sha" "$traces_asset"
} >"$expected_manifest"

binaries=(victoria-metrics-prod victoria-logs-prod victoria-traces-prod vmagent-prod)
complete=1
for binary in "${binaries[@]}"; do
  [[ -x "$bin_dir/$binary" ]] || complete=0
done
if [[ $complete == 1 && -f $manifest ]] && cmp -s "$expected_manifest" "$manifest"; then
  printf 'storage binaries are already installed in %s\n' "$bin_dir"
  exit 0
fi

extract_dir="$tmp_dir/extracted"
mkdir -p "$extract_dir"
tar -xzf "$archive_dir/$metrics_asset" -C "$extract_dir"
tar -xzf "$archive_dir/$vmutils_asset" -C "$extract_dir"
tar -xzf "$archive_dir/$logs_asset" -C "$extract_dir"
tar -xzf "$archive_dir/$traces_asset" -C "$extract_dir"
for binary in "${binaries[@]}"; do
  [[ -f "$extract_dir/$binary" ]] || { printf 'archive did not contain %s\n' "$binary" >&2; exit 1; }
  install -m 0755 "$extract_dir/$binary" "$bin_dir/$binary"
done
mv "$expected_manifest" "$manifest"
printf 'installed storage binaries for %s in %s\n' "$platform" "$bin_dir"
