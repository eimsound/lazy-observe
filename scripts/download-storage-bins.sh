#!/usr/bin/env bash
set -euo pipefail

# 版本与 sha256 由 Renovate 一起升级（.github/renovate.json）：每个 sha 行行尾的 `# <tag>` 是它同步 digest 的标记，
# 人工升级时与下面的 *_version 一起改。*_version 变量不要加行尾注释（scripts/third-party-notices.sh 按整行读取）。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
bin_dir=${LO_STORAGE_BIN_DIR:-"$root/.cache/storage-bin"}
# 归档根目录，归档存于 $archive_root/$platform。未设置时沿用 $bin_dir/.archives；跨平台准备
# （bin_dir=.cache/storage-bin/<platform>）传入共享的 .cache/storage-bin/.archives，宿主平台归档只存一份，
# bench/release 的 storage-all 缓存不再把 linux-amd64 打包两份（#378）。
archive_root=${LO_STORAGE_ARCHIVE_DIR:-"$bin_dir/.archives"}
goos=${GOOS:-$(go env GOOS)}
goarch=${GOARCH:-$(go env GOARCH)}
platform="$goos-$goarch"

case "$platform" in
  linux-amd64)
    metrics_sha=54409ea381db0316f0bf6f242c718bc498085eb1eb37520b73526e6d86891601 # v1.156.0
    vmutils_sha=d71f7f00e07782531000620ec6c2a9d7ec83324f364dd1f36095ab378b5fb43b # v1.156.0
    logs_sha=739e78c82294520b6079bc8a6ae32a1f9fb95a543092929a53a2fe9ab7383016 # v1.54.0
    traces_sha=789e006d076f3c8b99de7bb1d0277024a7db776db0294d1c27fcc5422d98c4bc # v0.13.0
    ;;
  linux-arm64)
    metrics_sha=f777110b232bfdf435556c57d0896716e27000afafe969c4eb08852352dceb7d # v1.156.0
    vmutils_sha=1a8e4ed55df2a59d5cd03056213f50d293d4fab029e38dd960597362d92cf176 # v1.156.0
    logs_sha=f6c4eb5bc4e2ec5a70c75f670f038122eed907b7e635b66497e80d38956561fa # v1.54.0
    traces_sha=210bc863666865ba9a4ff0a19901318042da0c474d248fec69ca71c07b1e5f7a # v0.13.0
    ;;
  *)
    printf 'unsupported storage binary platform: %s (supported: linux x amd64/arm64)\n' "$platform" >&2
    exit 1
    ;;
esac

metrics_version=v1.156.0
logs_version=v1.54.0
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

archive_dir="$archive_root/$platform"
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-storage.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
mkdir -p "$bin_dir" "$archive_dir"

# 旧布局：归档根目录指向别处时，$bin_dir/.archives/$platform 是改动前留下的同平台归档
# （例如 restore-keys 恢复的旧 storage-all 缓存）。当前归档先挪进共享目录免重复下载，其余在校验通过后清掉；
# 若 .archives 或 .archives/$platform 本身是符号链接则跳过，避免跨符号链接挪动或删除外部目录。
legacy_archive_root="$bin_dir/.archives"
legacy_archive_candidate="$legacy_archive_root/$platform"
legacy_archive_dir=
if [[ -d "$legacy_archive_candidate" && ! -L "$legacy_archive_root" && ! -L "$legacy_archive_candidate" ]]; then
  legacy_archive_dir=$(cd -P -- "$legacy_archive_candidate" && pwd)
  [[ $legacy_archive_dir != "$(cd -P -- "$archive_dir" && pwd)" ]] || legacy_archive_dir=
fi

fetch() {
  local asset=$1 expected=$2 url=$3 archive actual legacy
  archive="$archive_dir/$asset"
  legacy="$legacy_archive_dir/$asset"
  if [[ -n $legacy_archive_dir && ! -e $archive && -f $legacy && ! -L $legacy ]]; then
    mv -- "$legacy" "$archive"
    printf 'moved legacy archive %s to %s\n' "$legacy" "$archive_dir"
  fi
  if [[ ! -f $archive ]]; then
    printf 'downloading %s\n' "$url"
    curl --proto '=https' --tlsv1.2 --proto-redir '=https' -fsSL \
      --connect-timeout 20 --max-time 120 \
      --retry 5 --retry-all-errors --retry-max-time 90 \
      -o "$tmp_dir/$asset" "$url"
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

# 4 个当前归档均通过 SHA-256 校验后，清理当前平台归档目录下不再引用的历史归档，
# 防止 restore-keys 恢复旧缓存后跨升级累积残留；仅扫 .archives/$platform 顶层文件，绝不触碰其它平台目录。
prune_stale_archives() {
  local entry name
  while IFS= read -r -d '' entry; do
    name=${entry##*/}
    case "$name" in
      "$metrics_asset" | "$vmutils_asset" | "$logs_asset" | "$traces_asset")
        continue
        ;;
    esac
    rm -f -- "$entry"
    printf 'pruned stale archive %s\n' "$name"
  done < <(find "$archive_dir" -mindepth 1 -maxdepth 1 \( -type f -o -type l \) -print0)
}
prune_stale_archives

# 同样只在 4 个当前归档校验通过后清空旧布局目录（仅顶层文件与符号链接），再删掉变空的目录，
# 下次保存的 storage-all 缓存里 .cache/storage-bin/*/.archives 不再带归档。
retire_legacy_archives() {
  [[ -n $legacy_archive_dir ]] || return 0
  local entry
  while IFS= read -r -d '' entry; do
    rm -f -- "$entry"
    printf 'removed legacy archive %s\n' "$entry"
  done < <(find "$legacy_archive_dir" -mindepth 1 -maxdepth 1 \( -type f -o -type l \) -print0)
  rmdir -- "$legacy_archive_dir" 2>/dev/null || true
  rmdir -- "${legacy_archive_dir%/*}" 2>/dev/null || true
}
retire_legacy_archives

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
