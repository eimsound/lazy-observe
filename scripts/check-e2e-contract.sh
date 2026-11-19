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
# 由 mise run sdk-journeys 起真实平台后依次跑，ci.yml 的 sdk-journeys job 跑同一条任务（见下面的契约）。

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

# SDK journeys 按改动面跟 PR 跑：mise run sdk-journeys 起一个真实平台，依次跑 Go、Node、Python、Browser 四个
# journey；ci.yml 的 sdk-journeys job 由 sdk_journeys 标志控制，跑同一条 mise 任务。工具链缺失必须报错而不是 skip。
journeys_task=$(sed -n '/^\[tasks\.sdk-journeys\]$/,/^\[tasks\.install-check\]$/p' mise.toml)
[[ $journeys_task == *'depends = ["build", "storage-bins", "sdk-python"]'* ]] ||
  fail 'mise sdk-journeys must depend on build, storage-bins and sdk-python'
[[ $journeys_task == *'bash scripts/sdk-journeys.sh'* ]] || fail 'mise sdk-journeys must run scripts/sdk-journeys.sh'
journeys_script=scripts/sdk-journeys.sh
[[ -f $journeys_script ]] || fail 'scripts/sdk-journeys.sh is missing'
rg -Fq 'all_journeys=(go node python browser)' "$journeys_script" ||
  fail 'sdk-journeys.sh must run the go, node, python and browser journeys in that order'
if rg -n 'exit 77|SKIP:' "$journeys_script"; then
  fail 'sdk-journeys.sh must fail, not skip, when a toolchain is missing'
fi
for language in go node python browser; do
  [[ -f sdk/$language/e2e.sh ]] || fail "sdk/$language/e2e.sh is missing"
done
journeys_job=$(awk '$0 == "  sdk-journeys:" { on = 1; next } on && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { exit } on { print }' "$ci")
[[ -n $journeys_job ]] || fail 'push/PR CI lacks the sdk-journeys job'
[[ $journeys_job == *"if: needs.changes.outputs.sdk_journeys == 'true'"* ]] ||
  fail 'ci.yml sdk-journeys must gate on the sdk_journeys change flag'
[[ $journeys_job == *'MISE_CI_PROFILE: sdk-journeys'* ]] || fail 'ci.yml sdk-journeys must install the sdk-journeys mise profile'
[[ $journeys_job == *'- run: mise run sdk-journeys'* ]] || fail 'ci.yml sdk-journeys must run mise run sdk-journeys'
[[ $journeys_job == *'LO_SDK_JOURNEYS_ONLY: ${{ needs.changes.outputs.sdk_journeys_only }}'* ]] ||
  fail 'ci.yml sdk-journeys must pass the changes job sdk_journeys_only output as LO_SDK_JOURNEYS_ONLY'
rg -Fq 'sdk_journeys_only: ${{ steps.detect.outputs.sdk_journeys_only }}' "$ci" ||
  fail 'ci.yml changes job must export the sdk_journeys_only output'
# 每个 journey 必须落在独立的进程组里（set -m），cleanup 按组发信号；退回按 PID 停会重新漏进程（见 sdk-journeys-interrupt-test.sh）。
rg -Fq 'kill -TERM -- "-$pgid"' "$journeys_script" && rg -Fq 'kill -KILL -- "-$pgid"' "$journeys_script" ||
  fail 'sdk-journeys.sh cleanup must TERM and then KILL the whole process group of the platform and each journey'
[[ -x scripts/sdk-journeys-interrupt-test.sh ]] || fail 'scripts/sdk-journeys-interrupt-test.sh is missing or not executable'
for path in sdk/go/e2e.sh sdk/node/e2e.sh sdk/python/e2e.sh sdk/browser/e2e/performance-journey.spec.ts \
  cmd/lazy-observe/main.go internal/gateway/router.go web/src/App.tsx go.mod scripts/sdk-journeys.sh; do
  flags_out=$(printf '%s\n' "$path" | bash "$changes_script" --stdin 2>/dev/null)
  [[ $'\n'$flags_out$'\n' == *$'\n'sdk_journeys=true$'\n'* ]] || fail "SDK journeys do not trigger on $path"
done

# 默认项目的 token 只在首次创建时通过 slog 打一次日志，几个脚本从日志里解析它。这里锁住日志的 msg 与 key：
# internal/meta/store.go 的这条日志一改，下面的消费方就读不到 token（失败原因会很难看），所以改格式必须同步改它们，
# 并在这里先失败。slog 文本格式输出 `msg="created default project" ... project_api_token=lo_…`。
token_log_msg='created default project'
token_log_key='project_api_token'
rg -Fq "logger.Info(\"$token_log_msg\"," internal/meta/store.go ||
  fail "internal/meta/store.go no longer logs the default project token with msg \"$token_log_msg\""
rg -q "logger\\.Info\\(\"$token_log_msg\",.*\"$token_log_key\", token\\)" internal/meta/store.go ||
  fail "the \"$token_log_msg\" log in internal/meta/store.go must carry the token under key $token_log_key"
for consumer in scripts/sdk-journeys.sh scripts/e2e-multi-replica.sh scripts/check-release-smoke.sh; do
  rg -Fq "$token_log_msg" "$consumer" && rg -Fq "$token_log_key=" "$consumer" ||
    fail "$consumer no longer parses the default project token from the \"$token_log_msg\" log ($token_log_key=)"
done

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
