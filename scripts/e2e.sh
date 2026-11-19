#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
binary_input=${BINARY:-bin/lazy-observe}
storage_input=${STORAGE_BIN_DIR:-.cache/storage-bin}
evidence_dir=${LO_E2E_OUTPUT_DIR:-.cache/e2e-evidence}

case $binary_input in
/*) binary=$binary_input ;;
*) binary=$root/$binary_input ;;
esac
case $storage_input in
/*) storage=$storage_input ;;
*) storage=$root/$storage_input ;;
esac
case $evidence_dir in
/*) evidence=$evidence_dir/e2e-evidence.txt ;;
*) evidence=$root/$evidence_dir/e2e-evidence.txt ;;
esac

[[ -x $binary ]] || {
  printf 'E2E binary is missing or not executable: %s\n' "$binary" >&2
  exit 1
}
[[ -d $storage ]] || {
  printf 'E2E storage directory is missing: %s\n' "$storage" >&2
  exit 1
}
mkdir -p "${evidence%/*}"
: >"$evidence"

mark() {
  printf '%s\n' "$1" | tee -a "$evidence"
}

cd "$root"

# 这里只编排服务端自身的 e2e 检查与 evidence 机制。客户端 SDK（sdk/go、sdk/node、
# sdk/browser、sdk/python）的各语言 journey 在各自的 e2e.sh 里，由 `mise run sdk-journeys`
# （scripts/sdk-journeys.sh）起一个真实平台、准备凭据并依次跑；CI 的 SDK journeys job 跑同一条任务。
# 手动对着一个运行中的平台单独执行某个 e2e.sh 时，传 LO_E2E_PLATFORM_URL、LO_E2E_TOKEN（lo_，只用于
# 摄取 DSN）和 LO_E2E_ADMIN_PASSWORD（读请求用 admin 会话 Cookie 加 X-LO-Project，见 ADR 0010）。
# CI 的 sdk-* job 只跑各语言单元测试。

printf 'checking the supervisor recovery module before the product journey...\n'
CGO_ENABLED=0 go test ./internal/embed -run '^TestSupervisorRestartsUnexpectedManagedExit$' -count=1
mark 'PASS e2e module: supervisor-recovery'

# 网关 e2e 全集。这些测试大多在等固定的观察窗口，串行要 6 分钟以上；CI 用
# LO_E2E_GATEWAY_TESTS 把它们分到并行 job（见 ci.yml 的 e2e 矩阵），
# scripts/check-e2e-contract.sh 校验各分片的并集等于这份全集。
all_gateway_tests='StorageGateway|SelfMonitoringDisabled|LogsAlert|ScrapeManagement|NativeTLSLoopback'
gateway_tests=${LO_E2E_GATEWAY_TESTS:-$all_gateway_tests}
# go test -run 匹配不到测试也会通过，所以拼错的名字必须在这里拦下。
IFS='|' read -r -a requested <<<"$gateway_tests"
((${#requested[@]} > 0)) || { printf 'LO_E2E_GATEWAY_TESTS is empty\n' >&2; exit 1; }
for name in "${requested[@]}"; do
  [[ "|$all_gateway_tests|" == *"|$name|"* ]] || {
    printf 'unknown gateway E2E test in LO_E2E_GATEWAY_TESTS: %s\n' "$name" >&2
    exit 1
  }
done
if [[ $gateway_tests == "$all_gateway_tests" ]]; then
  gateway_marker='PASS e2e gateway: storage-self-monitoring-logs-alert-scrape-management'
else
  gateway_marker="PASS e2e gateway shard: $gateway_tests"
fi

printf 'running opt-in Gateway coverage (%s)...\n' "$gateway_tests"
LO_E2E_REAL_BINARY="$binary" \
  LO_E2E_BINARY="$root/scripts/e2e-binary-wrapper.sh" \
  LO_E2E_STORAGE_BIN_DIR="$storage" \
  CGO_ENABLED=0 \
  go test ./internal/gateway -run "^Test(${gateway_tests})E2E\$" -count=1
mark "$gateway_marker"

for required in \
  'PASS e2e module: supervisor-recovery' \
  "$gateway_marker"; do
  grep -Fqx "$required" "$evidence" || {
    printf 'E2E evidence is missing marker: %s\n' "$required" >&2
    exit 1
  }
done

if [[ $gateway_tests == "$all_gateway_tests" ]]; then
  mark 'PASS e2e: consolidated functional and recovery evidence complete'
else
  mark "PASS e2e: shard complete ($gateway_tests)"
fi
