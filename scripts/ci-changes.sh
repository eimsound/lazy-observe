#!/usr/bin/env bash
set -euo pipefail

# 增量 CI 的改动面检测：算出本次变更涉及的文件，按改动面分组输出布尔标志，
# ci.yml 各业务 job 用 `if: needs.changes.outputs.<flag> == 'true'` 决定跑还是跳过。
#
# 为什么不用 workflow 级 on.<event>.paths：被过滤掉的 workflow 不上报状态，
# 设成 required 的检查会一直等待。这里所有 job 都在 ci.yml 里，被跳过的 job 算通过，
# 由最后的 ci-ok 汇总 job 作为唯一的 required check。
#
# base 计算：
#   - pull_request：merge-base(CI_BASE_SHA, HEAD)，CI_BASE_SHA 来自
#     github.event.pull_request.base.sha；
#   - push：CI_BEFORE_SHA（github.event.before）；全零（新分支）或取不到时全量；
#   - 其它事件（workflow_dispatch 等）：全量。
# 任何一步取不到 base 都按全量处理，宁可多跑不漏跑。
#
# 本地测试：`printf '%s\n' a.go b.md | bash scripts/ci-changes.sh --stdin`，
# 标志写到 stdout（以及 $GITHUB_OUTPUT，若设置）。见 scripts/ci-changes-test.sh。

flags=(go build e2e ha web meta_pg arm64 sdk_go sdk_node sdk_browser sdk_python sdk_journeys pins)

# ha：多副本 HA 功能门禁（mise run e2e-ha → scripts/e2e-multi-replica.sh，只做功能断言，
# 不含 perf-ha 的性能部分）。它跑的是 build-binary 产出的服务端二进制、外置的 storage-bins
# 和 Docker 里钉死版本的 VM 集群 / S3 / Postgres / Valkey，所以只在下面这些输入变化时打开：
#   - 服务端 Go 代码与依赖：cmd/**、internal/**、go.mod/go.sum，以及经 replace 编进二进制的
#     sdk/go/**、ent 生成链路（entc.*、tools/entgen/**、scripts/generate-ent.sh）；
#   - 门禁本身：scripts/e2e-multi-replica.sh 和它现场编译的 scripts/e2e-{path-router,otlp-payload,otlp-sink}/**；
#   - 门禁读取的预算：bench/performance-gates.json（副本 GOMAXPROCS、trace 夹具规模）；
#   - 存储二进制下载：scripts/download-storage-bins.sh；
#   - 全量路径（.github/**、mise.toml、scripts/ci-*、Dockerfile*、未分类路径）。
# 刻意不打开的：web/**（build-binary 不构建前端，HA 断言不走 UI）、deploy/**、config.example.yaml
#（门禁不读 Helm chart 或示例配置，Helm 由 go job 的 helm-lint/helm-template-test 覆盖）、
# scripts/e2e.sh / e2e-restart.sh 等单副本 e2e 脚本、非 Go 的 SDK 与纯文档。
# 与 e2e 不完全同范围，所以单列一个标志。

# sdk_journeys：四个 SDK 的 journey（mise run sdk-journeys → scripts/sdk-journeys.sh）。它起一个真实平台
# （带嵌入前端的 build-binary + 外置 storage-bins），再对着它跑 sdk/{go,node,python,browser}/e2e.sh，
# 所以输入是 SDK 自身、服务端 Go 代码与依赖、会被嵌进二进制的前端、storage 二进制下载和门禁脚本：
#   - sdk/**（任一语言的 SDK，含各自的 e2e.sh 与示例；纯文档由 *.md 规则先挡掉）；
#   - cmd/**、internal/**、go.mod/go.sum、ent 生成链路；
#   - web/**、scripts/prepare-web-assets.sh（browser journey 登录真实 UI）；
#   - scripts/download-storage-bins.sh、scripts/sdk-journeys.sh；
#   - 全量路径（.github/**、mise.toml、scripts/ci-*、Dockerfile*、未分类路径）。
# 刻意不打开的：deploy/**、config.example.yaml、bench/**、单副本 e2e 与 HA 脚本。

# 打包链路：动到这些路径时额外跑 arm64 构建与冒烟（以及 amd64 构建与冒烟）。
# scripts/check-e2e-contract.sh 校验这份列表。
packaging_paths=(
  Dockerfile
  .dockerignore
  .goreleaser.yaml
  go.mod
  go.sum
  mise.toml
  'internal/embed/**'
  scripts/download-storage-bins.sh
  scripts/prepare-embedded-storage.sh
  scripts/prepare-web-assets.sh
  scripts/check-embedded-artifact.sh
  scripts/check-container-smoke.sh
)

# pins：手工固定点的「最小发布年龄」门禁（scripts/check-pinned-releases.sh --gate，要联网查 GitHub API，
# 所以只在固定点本身变化时跑）。路径与 check-pinned-releases.sh 解析的文件一一对应：
# Victoria 存储二进制、mise 引导版本、Trivy 版本（拆出 install-trivy.sh 前后各一个位置）、e2e 的 VM 集群镜像，
# 以及脚本自身、测试用的年龄常量。它是叠加标志：不改变这些文件原有的分组。
pinned_release_paths=(
  scripts/download-storage-bins.sh
  scripts/mise-release.env
  scripts/check-image-vulns.sh
  scripts/install-trivy.sh
  scripts/e2e-multi-replica.sh
  scripts/check-pinned-releases.sh
  scripts/release-age.env
)

declare -A out=()
for flag in "${flags[@]}"; do
  out[$flag]=false
done

log() {
  printf '[ci-changes] %s\n' "$*" >&2
}

set_flags() {
  local flag
  for flag in "$@"; do
    out[$flag]=true
  done
}

set_all() {
  set_flags "${flags[@]}"
}

matches_packaging() {
  local file=$1 pattern
  for pattern in "${packaging_paths[@]}"; do
    if [[ $pattern == *'/**' ]]; then
      [[ $file == "${pattern%/**}/"* ]] && return 0
    elif [[ $file == "$pattern" ]]; then
      return 0
    fi
  done
  return 1
}

matches_pinned_releases() {
  local file=$1 pattern
  for pattern in "${pinned_release_paths[@]}"; do
    [[ $file == "$pattern" ]] && return 0
  done
  return 1
}

# classify FILE：把单个文件映射到改动面。没有命中任何已知分组的文件按全量处理。
classify() {
  local file=$1

  if matches_packaging "$file"; then
    set_flags build arm64
  fi
  if matches_pinned_releases "$file"; then
    set_flags pins
  fi

  case "$file" in
  # 纯文档与 agent 流程：不跑业务 job。
  *.md | docs/* | LICENSE | NOTICE | .agents/* | .claude/* | skills-lock.json | .air.toml | .github/ISSUE_TEMPLATE/*)
    return
    ;;
  # Dependabot 配置只被 GitHub 的 Dependabot 服务读取，不改变任何 job 的行为；.trivyignore.yaml 只在
  # release.yml 的镜像扫描里生效。两者的格式与覆盖面都由 go job 里的 workflow-check 校验，所以只跑 go。
  .github/dependabot.yml | .trivyignore.yaml)
    set_flags go
    ;;
  # CI 自身、工具链与镜像：全量。
  .github/* | mise.toml | scripts/ci-* | Dockerfile*)
    set_all
    ;;
  # 服务端经 replace 把 sdk/go 编进二进制（自监控链路），所以也要跑构建与 e2e。
  sdk/go/*)
    set_flags sdk_go go build e2e ha sdk_journeys
    ;;
  sdk/node/*)
    set_flags sdk_node sdk_journeys
    ;;
  sdk/browser/*)
    set_flags sdk_browser sdk_journeys
    ;;
  sdk/python/*)
    set_flags sdk_python sdk_journeys
    ;;
  sdk/*)
    set_flags sdk_go sdk_node sdk_browser sdk_python sdk_journeys
    ;;
  web/*)
    set_flags web build sdk_journeys
    ;;
  internal/db/* | internal/meta/* | entc.mod | entc.sum | tools/entgen/* | scripts/generate-ent.sh)
    set_flags go build e2e ha meta_pg sdk_journeys
    ;;
  # 依赖升级可能动到 pgx / embedded-postgres，PG 元数据测试一起跑。
  go.mod | go.sum)
    set_flags go build e2e ha meta_pg sdk_journeys
    ;;
  cmd/* | internal/*)
    set_flags go build e2e ha sdk_journeys
    ;;
  # HA 门禁脚本与它现场编译的辅助程序；也保留单副本 e2e 原有的触发。
  scripts/e2e-multi-replica.sh | scripts/e2e-path-router/* | scripts/e2e-otlp-payload/* | scripts/e2e-otlp-sink/*)
    set_flags go e2e ha
    ;;
  scripts/download-storage-bins.sh)
    set_flags go e2e ha sdk_journeys
    ;;
  scripts/e2e*)
    set_flags go e2e
    ;;
  # 嵌入前端的产物也是 SDK journey 里 browser 登录的真实 UI。
  scripts/prepare-web-assets.sh)
    set_flags go e2e sdk_journeys
    ;;
  # SDK journey 门禁自身：起真实平台跑四个 SDK 的 journey。
  scripts/sdk-journeys.sh)
    set_flags go sdk_journeys
    ;;
  # HA 门禁读取副本 GOMAXPROCS 与 trace 夹具规模。
  bench/performance-gates.json)
    set_flags go ha
    ;;
  # JS 许可证检查被 web 与两个 JS SDK job 共用。
  scripts/check-js-licenses.sh)
    set_flags web sdk_node sdk_browser
    ;;
  .dockerignore | .goreleaser.yaml)
    set_flags go build arm64
    ;;
  scripts/* | deploy/* | bench/* | tools/* | .golangci.yml | config.example.yaml | .gitignore)
    set_flags go
    ;;
  *)
    log "unclassified path $file: running everything"
    set_all
    ;;
  esac
}

changed_files=()
mode=${1:-}
case "$mode" in
--stdin)
  mapfile -t changed_files
  ;;
--all)
  set_all
  ;;
'')
  event=${CI_EVENT_NAME:-}
  base=''
  case "$event" in
  pull_request)
    if [[ -n ${CI_BASE_SHA:-} ]] && git cat-file -e "${CI_BASE_SHA}^{commit}" 2>/dev/null; then
      base=$(git merge-base "$CI_BASE_SHA" HEAD 2>/dev/null || true)
    fi
    ;;
  push)
    if [[ -n ${CI_BEFORE_SHA:-} && ! ${CI_BEFORE_SHA} =~ ^0+$ ]] &&
      git cat-file -e "${CI_BEFORE_SHA}^{commit}" 2>/dev/null; then
      base=$CI_BEFORE_SHA
    fi
    ;;
  esac
  if [[ -z $base ]]; then
    log "event=${event:-<none>}: no usable base, running everything"
    set_all
  else
    log "event=$event base=$base head=$(git rev-parse HEAD)"
    mapfile -t changed_files < <(git diff --name-only --no-renames "$base" HEAD)
  fi
  ;;
*)
  printf 'usage: ci-changes.sh [--stdin|--all]\n' >&2
  exit 2
  ;;
esac

for file in "${changed_files[@]}"; do
  [[ -n $file ]] && classify "$file"
done
log "changed files: ${#changed_files[@]}"

output=${GITHUB_OUTPUT:-}
for flag in "${flags[@]}"; do
  printf '%s=%s\n' "$flag" "${out[$flag]}"
  if [[ -n $output ]]; then
    printf '%s=%s\n' "$flag" "${out[$flag]}" >>"$output"
  fi
done

# arm64 进入 build 矩阵时，同时给出 build 与冒烟用的 runner 矩阵。
if [[ ${out[arm64]} == true ]]; then
  matrix='[{"arch":"amd64","runner":"ubuntu-24.04"},{"arch":"arm64","runner":"ubuntu-24.04-arm"}]'
else
  matrix='[{"arch":"amd64","runner":"ubuntu-24.04"}]'
fi
printf 'build_matrix=%s\n' "$matrix"
if [[ -n $output ]]; then
  printf 'build_matrix=%s\n' "$matrix" >>"$output"
fi
