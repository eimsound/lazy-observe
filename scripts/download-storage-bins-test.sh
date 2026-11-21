#!/usr/bin/env bash
set -euo pipefail

# scripts/download-storage-bins.sh 的离线自测，守三条：
# - 当前平台 4 个归档全部 SHA-256 校验通过后，保留当前 4 个归档并清理同平台 .archives/$platform 下残留的旧归档（含符号链接）；
# - 绝不触碰其它平台目录（例如 .archives/linux-arm64），保证双架构交叉构建互不误删；
# - 任意归档 SHA-256 校验失败时 fail-closed 退出非零，且不提前清理旧归档；二进制已安装快路径也会执行旧归档清理。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
script=$root/scripts/download-storage-bins.sh
bash_bin=$(command -v bash)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}

dry_run=$(LO_STORAGE_DOWNLOAD_DRY_RUN=1 GOOS=linux GOARCH=amd64 "$bash_bin" "$script")
mapfile -t assets < <(awk '/^[0-9a-f]{64}  /{ print $2 }' <<<"$dry_run")
if ((${#assets[@]} != 4)); then
  printf 'unexpected dry-run asset count: %d\n%s\n' "${#assets[@]}" "$dry_run" >&2
  exit 1
fi
metrics_asset=${assets[0]}
vmutils_asset=${assets[1]}
logs_asset=${assets[2]}
traces_asset=${assets[3]}

payloads=$work/payloads
mkdir -p "$payloads"

make_archive() {
  local asset=$1 binary=$2
  local stage=$work/stage-$binary
  mkdir -p "$stage"
  printf '#!/bin/sh\nexit 0\n' >"$stage/$binary"
  chmod 0755 "$stage/$binary"
  tar -czf "$payloads/$asset" -C "$stage" "$binary"
  sha256sum "$payloads/$asset" | awk '{print $1}'
}

metrics_sha=$(make_archive "$metrics_asset" victoria-metrics-prod)
vmutils_sha=$(make_archive "$vmutils_asset" vmagent-prod)
logs_sha=$(make_archive "$logs_asset" victoria-logs-prod)
traces_sha=$(make_archive "$traces_asset" victoria-traces-prod)

test_script=$work/download-storage-bins.sh
awk \
  -v m="$metrics_sha" -v v="$vmutils_sha" -v l="$logs_sha" -v t="$traces_sha" '
  /^[[:space:]]*linux-amd64\)/ { in_amd64 = 1 }
  /^[[:space:]]*;;/ { in_amd64 = 0 }
  in_amd64 && /^[[:space:]]*metrics_sha=/ { print "    metrics_sha=" m; next }
  in_amd64 && /^[[:space:]]*vmutils_sha=/ { print "    vmutils_sha=" v; next }
  in_amd64 && /^[[:space:]]*logs_sha=/ { print "    logs_sha=" l; next }
  in_amd64 && /^[[:space:]]*traces_sha=/ { print "    traces_sha=" t; next }
  { print }
' "$script" >"$test_script"
chmod 0755 "$test_script"

fakebin=$work/fakebin
mkdir -p "$fakebin"
cat >"$fakebin/curl" <<'CURL'
#!/bin/sh
set -eu
out=
url=
if [ -n "${CURL_CALL_LOG:-}" ]; then
  printf '%s\n' '---' >>"$CURL_CALL_LOG"
  for arg in "$@"; do
    printf '%s\n' "$arg" >>"$CURL_CALL_LOG"
  done
fi
while [ $# -gt 0 ]; do
  case $1 in
  -o | --output | --proto | --proto-redir | --connect-timeout | --max-time | --retry | --retry-delay | --retry-max-time | --max-redirs | -H | --header | -w | --write-out)
    if [ "$1" = -o ] || [ "$1" = --output ]; then
      out=$2
    fi
    shift 2
    ;;
  --)
    shift
    break
    ;;
  -*)
    shift
    ;;
  *)
    if [ -n "$url" ]; then
      printf 'fake curl: unexpected extra positional arg: %s\n' "$1" >&2
      exit 2
    fi
    url=$1
    shift
    ;;
  esac
done
if [ -z "$url" ] && [ $# -gt 0 ]; then
  url=$1
fi
asset=${url##*/}
cp "${FAKE_STORAGE_PAYLOADS:?}/$asset" "$out"
CURL
chmod 0755 "$fakebin/curl"

run_downloader() {
  local bin_dir=$1 stdout_file=$2 stderr_file=$3 curl_log=${4:-}
  local rc=0
  env -i \
    PATH="$fakebin:/usr/bin:/bin" \
    GOOS=linux \
    GOARCH=amd64 \
    LO_STORAGE_BIN_DIR="$bin_dir" \
    FAKE_STORAGE_PAYLOADS="$payloads" \
    CURL_CALL_LOG="$curl_log" \
    "$bash_bin" "$test_script" >"$stdout_file" 2>"$stderr_file" || rc=$?
  return "$rc"
}

# 1. 部分缓存命中 + 同平台旧归档与符号链接清理 + 保护其它平台目录 + curl TLS/超时/指数退避参数。
bin_ok=$work/bin-ok
arch_amd64=$bin_ok/.archives/linux-amd64
arch_arm64=$bin_ok/.archives/linux-arm64
mkdir -p "$arch_amd64" "$arch_arm64"
cp "$payloads/$metrics_asset" "$arch_amd64/$metrics_asset"
cp "$payloads/$logs_asset" "$arch_amd64/$logs_asset"
printf 'stale-metrics\n' >"$arch_amd64/victoria-metrics-linux-amd64-v1.147.0.tar.gz"
printf 'stale-logs\n' >"$arch_amd64/victoria-logs-linux-amd64-v1.53.0.tar.gz"
ln -s "victoria-logs-linux-amd64-v1.53.0.tar.gz" "$arch_amd64/stale-symlink.tar.gz"
printf 'arm64-metrics-bytes\n' >"$arch_arm64/victoria-metrics-linux-arm64-v1.156.0.tar.gz"
printf 'arm64-logs-bytes\n' >"$arch_arm64/victoria-logs-linux-arm64-v1.54.0.tar.gz"

ok_curl_log=$work/ok.curl.log
: >"$ok_curl_log"
rc=0
run_downloader "$bin_ok" "$work/ok.out" "$work/ok.err" "$ok_curl_log" || rc=$?
if ((rc != 0)); then
  fail "case 1: expected exit 0, got $rc; stderr: $(cat "$work/ok.err")"
fi

if (($(grep -c '^---$' "$ok_curl_log") != 2)); then
  fail "case 1: expected 2 curl calls for 2 uncached archives, got $(grep -c '^---$' "$ok_curl_log")"
fi
for flag in --proto =https --tlsv1.2 --proto-redir -fsSL --connect-timeout 20 --max-time 120 --retry 5 --retry-all-errors --retry-max-time 90; do
  grep -Fxq -- "$flag" "$ok_curl_log" || fail "case 1: curl missing expected argument $flag"
done
! grep -Fxq -- '--retry-delay' "$ok_curl_log" || fail "case 1: curl must not set fixed --retry-delay"

for asset in "${assets[@]}"; do
  [[ -f "$arch_amd64/$asset" ]] || fail "case 1: missing current archive $asset"
done
for stale in \
  victoria-metrics-linux-amd64-v1.147.0.tar.gz \
  victoria-logs-linux-amd64-v1.53.0.tar.gz \
  stale-symlink.tar.gz; do
  [[ ! -e "$arch_amd64/$stale" && ! -L "$arch_amd64/$stale" ]] ||
    fail "case 1: stale entry was not pruned: $stale"
done
mapfile -t remaining_amd64 < <(find "$arch_amd64" -mindepth 1 -maxdepth 1 | sort)
if ((${#remaining_amd64[@]} != 4)); then
  fail "case 1: expected exactly 4 archives in linux-amd64, got ${#remaining_amd64[@]}: ${remaining_amd64[*]}"
fi
[[ "$(cat "$arch_arm64/victoria-metrics-linux-arm64-v1.156.0.tar.gz")" == "arm64-metrics-bytes" ]] ||
  fail "case 1: linux-arm64 metrics archive was modified or deleted"
[[ "$(cat "$arch_arm64/victoria-logs-linux-arm64-v1.54.0.tar.gz")" == "arm64-logs-bytes" ]] ||
  fail "case 1: linux-arm64 logs archive was modified or deleted"
for binary in victoria-metrics-prod victoria-logs-prod victoria-traces-prod vmagent-prod; do
  [[ -x "$bin_ok/$binary" ]] || fail "case 1: missing executable binary $binary"
done

# 2. 二进制与 .manifest 已就绪的快路径，仍清理后来带入的旧归档且不影响其它平台。
printf 'stale-fastpath\n' >"$arch_amd64/victoria-traces-linux-amd64-v0.0.1-stale.tar.gz"
rc=0
run_downloader "$bin_ok" "$work/fast.out" "$work/fast.err" || rc=$?
if ((rc != 0)); then
  fail "case 2: fast-path run failed with exit $rc; stderr: $(cat "$work/fast.err")"
fi
[[ ! -e "$arch_amd64/victoria-traces-linux-amd64-v0.0.1-stale.tar.gz" ]] ||
  fail "case 2: fast-path did not prune stale archive"
mapfile -t remaining_fast < <(find "$arch_amd64" -mindepth 1 -maxdepth 1 | sort)
if ((${#remaining_fast[@]} != 4)); then
  fail "case 2: expected 4 archives after fast-path prune, got ${#remaining_fast[@]}"
fi
[[ -f "$arch_arm64/victoria-metrics-linux-arm64-v1.156.0.tar.gz" ]] ||
  fail "case 2: linux-arm64 archive disappeared during fast-path"

# 3. SHA-256 校验失败必须 fail-closed：非零退出且不提前删掉旧归档。
bin_bad=$work/bin-bad
bad_amd64=$bin_bad/.archives/linux-amd64
bad_arm64=$bin_bad/.archives/linux-arm64
mkdir -p "$bad_amd64" "$bad_arm64"
cp "$payloads/$metrics_asset" "$bad_amd64/$metrics_asset"
cp "$payloads/$vmutils_asset" "$bad_amd64/$vmutils_asset"
cp "$payloads/$logs_asset" "$bad_amd64/$logs_asset"
printf 'corrupted-traces-tarball\n' >"$bad_amd64/$traces_asset"
printf 'stale-must-stay\n' >"$bad_amd64/stale-must-stay.tar.gz"
printf 'arm64-keep\n' >"$bad_arm64/arm64-keep.tar.gz"

rc=0
run_downloader "$bin_bad" "$work/bad.out" "$work/bad.err" || rc=$?
if ((rc == 0)); then
  fail "case 3: expected non-zero exit on checksum mismatch, got 0"
fi
[[ -f "$bad_amd64/stale-must-stay.tar.gz" ]] ||
  fail "case 3: stale archive was pruned even though checksum verification failed"
[[ -f "$bad_arm64/arm64-keep.tar.gz" ]] ||
  fail "case 3: linux-arm64 archive was touched on failure"

if ((failures > 0)); then
  printf 'download-storage-bins test: %d failure(s)\n' "$failures" >&2
  exit 1
fi
printf 'PASS: download-storage-bins keeps current 4 archives, prunes stale platform archives after SHA-256 verification, and preserves other platforms\n'
