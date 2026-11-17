#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

fail() {
  printf 'E2E contract: %s\n' "$1" >&2
  exit 1
}

# 本脚本只做服务端自身 e2e / restart / cross-platform artifact 的静态契约检查。
# SDK（sdk/<lang>）的单元测试由 ci.yml 的 sdk-* job 运行；各语言 journey（sdk/*/e2e.sh）
# 需要外部平台，暂不进 CI。

e2e_task=$(sed -n '/^\[tasks\.e2e\]$/,/^\[tasks\.e2e-restart\]$/p' mise.toml)
[[ $e2e_task == *'bash scripts/e2e.sh'* ]] || fail 'mise e2e must delegate to the root orchestrator'

[[ -f scripts/e2e.sh ]] || fail 'scripts/e2e.sh is missing'
all_gateway_tests=$(sed -n "s/^all_gateway_tests='\(.*\)'$/\1/p" scripts/e2e.sh)
[[ $all_gateway_tests == 'StorageGateway|SelfMonitoringDisabled|LogsAlert|ScrapeManagement|NativeTLSLoopback' ]] || \
  fail 'root orchestrator does not define the full opt-in Gateway E2E set'
rg -Fq 'go test ./internal/gateway -run "^Test(${gateway_tests})E2E\$"' scripts/e2e.sh || \
  fail 'root orchestrator does not run the selected Gateway E2E tests'
rg -Fq 'scripts/e2e-binary-wrapper.sh' scripts/e2e.sh || \
  fail 'Gateway E2E coverage does not use the real-binary wrapper'

for marker in \
  'PASS e2e module: supervisor-recovery' \
  'PASS e2e gateway: storage-self-monitoring-logs-alert-scrape-management' \
  'PASS e2e recovery: managed-child' \
  'PASS e2e restart: graceful' \
  'PASS e2e restart: forced'; do
  rg -Fq "$marker" scripts/e2e.sh scripts/e2e-restart.sh || fail "missing named evidence marker: $marker"
done

ci=.github/workflows/ci.yml
release=.github/workflows/release.yml
bench=.github/workflows/bench.yml

rg -Fq 'mise run e2e' "$ci" || fail 'push/PR CI must run the consolidated e2e task'
# ci.yml 把网关 e2e 拆成并行分片：各分片的并集必须正好是全集，且不重复。
mapfile -t shard_tests < <(sed -n 's/^ *gateway_tests: "\(.*\)"$/\1/p' "$ci" | tr '|' '\n' | sort)
mapfile -t expected_tests < <(printf '%s\n' "$all_gateway_tests" | tr '|' '\n' | sort)
[[ ${#shard_tests[@]} -gt 0 ]] || fail 'ci.yml e2e matrix defines no gateway_tests shards'
[[ "${shard_tests[*]}" == "${expected_tests[*]}" ]] || \
  fail "ci.yml e2e shards (${shard_tests[*]}) must cover each Gateway E2E test exactly once (${expected_tests[*]})"
rg -Fq 'include: ${{ fromJSON(needs.changes.outputs.build_matrix) }}' "$ci" || \
  fail 'push/PR CI build and smoke jobs must use the change-driven amd64/arm64 matrix'
rg -Fq 'scripts/check-container-smoke.sh' "$ci" || fail 'push/PR CI lacks zero-config container smoke'

go_cross=$(sed -n '/^  go-cross:$/,/^  e2e:$/p' "$ci")
[[ $go_cross == *'mise run dist'* ]] || fail 'go-cross must build real embedded assets with mise run dist'
[[ $go_cross == *'GOARCH: ${{ matrix.arch }}'* ]] || fail 'go-cross must build for the matrix architecture'
[[ $go_cross == *'scripts/check-container-smoke.sh "lazy-observe:ci-${{ matrix.arch }}" "linux/${{ matrix.arch }}"'* ]] || \
  fail 'artifact-smoke must smoke the matrix architecture'

# CI 分层契约：arm64 不跟每个 PR 跑，但覆盖不能丢。
# - ci.yml：PR 动到打包链路时 scripts/ci-changes.sh 把 arm64（ubuntu-24.04-arm）加进 build 矩阵；
# - release.yml：发版前对两个架构做容器冒烟。
changes_script=scripts/ci-changes.sh
[[ -f $changes_script ]] || fail 'scripts/ci-changes.sh is missing'
rg -Fq '{"arch":"arm64","runner":"ubuntu-24.04-arm"}' "$changes_script" || \
  fail 'packaging changes must add the native arm64 runner to the build matrix'
packaging=$(sed -n '/^packaging_paths=($/,/^)$/p' "$changes_script")
for path in \
  Dockerfile \
  .dockerignore \
  .goreleaser.yaml \
  go.mod \
  go.sum \
  mise.toml \
  "'internal/embed/**'" \
  scripts/download-storage-bins.sh \
  scripts/prepare-embedded-storage.sh \
  scripts/prepare-web-assets.sh \
  scripts/check-embedded-artifact.sh \
  scripts/check-container-smoke.sh; do
  grep -Fxq -- "  $path" <<<"$packaging" || fail "arm64 build matrix does not trigger on packaging path: $path"
done
rg -Fq 'arch: [amd64, arm64]' "$release" || \
  fail 'release workflow lacks the amd64/arm64 container smoke matrix'

# 多副本 HA 功能门禁按改动面跟 PR 跑：ci.yml 的 e2e-ha job 由 ha 标志控制、先确认 Docker 可用，
# 只跑功能断言；性能部分（LO_E2E_PERF_GATES / perf-ha）不进 PR。
ha_task=$(sed -n '/^\[tasks\.e2e-ha\]$/,/^\[tasks\.perf-ha\]$/p' mise.toml)
[[ $ha_task == *'bash scripts/e2e-multi-replica.sh'* ]] || fail 'mise e2e-ha must run scripts/e2e-multi-replica.sh'
[[ $ha_task != *LO_E2E_PERF_GATES* ]] || fail 'mise e2e-ha must stay a functional gate without LO_E2E_PERF_GATES'
ha_job=$(awk '$0 == "  e2e-ha:" { on = 1; next } on && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { exit } on { print }' "$ci")
[[ -n $ha_job ]] || fail 'push/PR CI lacks the e2e-ha job'
[[ $ha_job == *"if: needs.changes.outputs.ha == 'true'"* ]] || fail 'ci.yml e2e-ha must gate on the ha change flag'
[[ $ha_job == *'bash scripts/ci-setup-docker.sh'* ]] || fail 'ci.yml e2e-ha must verify Docker access first'
[[ $ha_job == *'- run: mise run e2e-ha'* ]] || fail 'ci.yml e2e-ha must run mise run e2e-ha'
[[ $ha_job != *LO_E2E_PERF_GATES* ]] || fail 'ci.yml e2e-ha must not enable the HA performance gates'
for path in scripts/e2e-multi-replica.sh go.mod internal/meta/store.go cmd/lazy-observe/main.go; do
  # 先收齐输出再匹配：pipefail 下 grep -q 提前退出会让 ci-changes.sh 收到 SIGPIPE（141）。
  flags_out=$(printf '%s\n' "$path" | bash "$changes_script" --stdin 2>/dev/null)
  [[ $'\n'$flags_out$'\n' == *$'\n'ha=true$'\n'* ]] || fail "HA E2E does not trigger on $path"
done
if rg -n 'mise run (perf-ha|perf-release|bench|performance-gates)' "$ci"; then
  fail 'push/PR CI must not run the performance gates'
fi

# 性能门禁不跟 PR 跑：必须挡在发版链路上（archive 之前），bench.yml 只留手动入口。
for task in 'mise run bench' 'mise run perf-release' 'mise run perf-ha' 'mise run perf-results-check'; do
  rg -Fq "$task" "$release" || fail "release workflow lacks performance gate: $task"
  rg -Fq "$task" "$bench" || fail "manual performance workflow lacks: $task"
done
archive_needs=$(sed -n '/^  archive:$/,/^    runs-on:/p' "$release")
[[ $archive_needs == *container-check* && $archive_needs == *performance-gates* ]] || \
  fail 'release archive must wait for container-check and performance-gates'

for platform in linux/amd64 linux/arm64; do
  rg -Fq "PASS artifact: $platform zero-config" scripts/check-container-smoke.sh || \
    fail "container smoke lacks evidence marker for $platform"
done

printf 'PASS: E2E coverage and cross-platform artifact contract is statically complete\n'
