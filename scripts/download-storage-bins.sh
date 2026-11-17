#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
bin_dir=${LO_STORAGE_BIN_DIR:-"$root/.cache/storage-bin"}
goos=${GOOS:-$(go env GOOS)}
goarch=${GOARCH:-$(go env GOARCH)}
platform="$goos-$goarch"

case "$platform" in
  linux-amd64)
    metrics_sha=7d16e5099be040e10019cfe0c03c50043bad4e896b371129cb41be80efd687cc
    vmutils_sha=1e5da38c3d8d7af9c4fc0daa8a2103f2578c40ed81752d21f20a3bd6d0506b3f
    logs_sha=d14f585144b8d6813f15e11f0041f487e15e10e5f5e5a31be0311367e93d3494
    traces_sha=a082f58abfdb1d5ca57d936313eb2980ee0d775f654acfbc8daf3888879b47fa
    ;;
  linux-arm64)
    metrics_sha=3ef55ae5fb54882246ac936d3229c21a82078aa5964d68c12e0d27bab17ab6cd
    vmutils_sha=0348ec940845fe35197310dd49df0620fca840d43e67d75616ba55eebd7e6edb
    logs_sha=91338c3e5e3d743a862c0a8665bf80862f639dbd4de6f6ff19ada7df5e9acf45
    traces_sha=7a08df830ffc040593c6018a8434117fd7ca9292171c6766ff66defa205b86fa
    ;;
  *)
    printf 'unsupported storage binary platform: %s (supported: linux x amd64/arm64)\n' "$platform" >&2
    exit 1
    ;;
esac

metrics_version=v1.147.0
logs_version=v1.52.0
traces_version=v0.9.4
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
