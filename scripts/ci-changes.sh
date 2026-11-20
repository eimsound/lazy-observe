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

flags=(go build e2e ha web meta_pg arm64 sdk_go sdk_node sdk_browser sdk_python sdk_journeys trivy_pin)

# ha：多副本 HA 功能门禁（mise run e2e-ha → scripts/e2e-multi-replica.sh，只做功能断言，
# 不含 perf-ha 的性能部分）。它跑的是 build-binary 产出的服务端二进制、外置的 storage-bins
# 和 Docker 里钉死版本的 VM 集群 / S3 / Postgres / Valkey，所以只在下面这些输入变化时打开：
#   - 服务端 Go 代码与依赖：cmd/**、internal/**、go.mod/go.sum，以及经 replace 编进二进制的
#     sdk/go/**、ent 生成链路（entc.*、tools/entgen/**、scripts/generate-ent.sh）；
#   - 门禁本身：scripts/e2e-multi-replica.sh 和它现场编译的 scripts/e2e-{path-router,otlp-payload,otlp-sink}/**；
#   - 门禁读取的预算：bench/performance-gates.json（副本 GOMAXPROCS、trace 夹具规模）；
#   - 存储二进制下载：scripts/download-storage-bins.sh；
#   - 全量路径（.github/**、mise.toml、scripts/ci-*、scripts/mise-release.env、Dockerfile*、未分类路径）。
# 刻意不打开的：web/**（build-binary 不构建前端，HA 断言不走 UI）、deploy/**、config.example.yaml
#（门禁不读 Helm chart 或示例配置，Helm 由 go job 的 helm-lint/helm-template-test 覆盖）、
# scripts/e2e.sh / e2e-restart.sh 等单副本 e2e 脚本、非 Go 的 SDK 与纯文档。
# 与 e2e 不完全同范围，所以单列一个标志。

# sdk_journeys：四个 SDK 的 journey（mise run sdk-journeys → scripts/sdk-journeys.sh）。它起一个真实平台
# （带嵌入前端的 build-binary + 外置 storage-bins），再对着它跑 sdk/{go,node,python,browser}/e2e.sh，
# 所以输入是 SDK 自身、服务端 Go 代码与依赖、会被嵌进二进制的前端、storage 二进制下载和门禁脚本：
#   - sdk/**（任一语言的 SDK，含各自的 e2e.sh 与示例；纯文档由 *.md 规则先挡掉）；
#   - cmd/**（cmd/perf-gate/** 除外：它是独立的性能门禁程序，不被平台二进制引用）、internal/**、go.mod/go.sum、ent 生成链路；
#   - web/**、scripts/prepare-web-assets.sh（browser journey 登录真实 UI）；
#   - scripts/download-storage-bins.sh、scripts/sdk-journeys.sh；
#   - 全量路径（.github/**、mise.toml、scripts/ci-*、scripts/mise-release.env、Dockerfile*、未分类路径）。
# 刻意不打开的：deploy/**、config.example.yaml、bench/**、单副本 e2e 与 HA 脚本、cmd/perf-gate/**。
#
# sdk_journeys_only：sdk_journeys 为 true 时要跑哪几个 journey。空 = 四个都跑；否则是空格分隔的语言列表
# （按 go node python browser 的固定顺序），ci.yml 原样传给 LO_SDK_JOURNEYS_ONLY。只有下面这些改动面是「窄」的，
# 混合改动时取并集，其它任何会打开 sdk_journeys 的「宽」路径（服务端、Go 依赖、journey/storage/web 资产脚本、sdk/ 根共享文件、
# CI、未分类）都让它回到空：
#   - sdk/go/**      → go browser    （browser journey 会现场编译并运行 sdk/go 的 fiber 示例）
#   - sdk/node/**    → node browser  （browser journey 会构建并运行 sdk/node 的 express 示例）
#   - sdk/python/**  → python
#   - sdk/browser/** → browser
#   - web/**         → browser       （只有 browser journey 登录嵌入的前端）
# sdk_journeys 为 false 时它也是空，ci.yml 里没有 job 会读它。

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
  scripts/lib/uv-python.sh
  scripts/check-container-smoke.sh
)

# trivy_pin：scripts/install-trivy.sh 改动时在 PR 里真下载一次 Trivy 并校验 sha256（ci.yml 的 trivy-pin job）。
# Trivy 的 sha256 只在发版扫描里用到，别的 job 不会替它把关；mise 与 Victoria 的 sha 在现有 job 里本来就会校验。
# 它是叠加标志：不改变该文件原有的分组。

declare -A out=()
for flag in "${flags[@]}"; do
  out[$flag]=false
done

# SDK journey 的范围：journey_full=true 表示四个都跑；否则只跑 journey_langs 里的语言。
journey_languages=(go node python browser)
journey_full=false
declare -A journey_langs=()

log() {
  printf '[ci-changes] %s\n' "$*" >&2
}

set_flags() {
  local flag
  for flag in "$@"; do
    out[$flag]=true
    # 没有显式限定语言的 sdk_journeys 就是四个全跑。
    [[ $flag == sdk_journeys ]] && journey_full=true
  done
  return 0
}

# set_journeys LANG...：打开 sdk_journeys，但只跑这些语言的 journey（多次调用取并集）。
set_journeys() {
  local language
  out[sdk_journeys]=true
  for language in "$@"; do
    journey_langs[$language]=1
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

# classify FILE：把单个文件映射到改动面。没有命中任何已知分组的文件按全量处理。
classify() {
  local file=$1

  if matches_packaging "$file"; then
    set_flags build arm64
  fi
  if [[ $file == scripts/install-trivy.sh ]]; then
    set_flags trivy_pin
  fi
  # 7 天发布年龄策略（scripts/check-github-workflows.sh）和 pipefail 静态门禁（scripts/check-pipefail-pipelines.sh）
  # 都运行在 go job 的 workflow-check 中：动到任意 pnpm-workspace/lock、pyproject.toml 或 shell 脚本时必须打开 go，
  # 避免被下面的 sdk/*、web/* 窄分组跳过。
  case "$file" in
  *pnpm-workspace.yaml | *pnpm-lock.yaml | *pyproject.toml | *.sh)
    set_flags go
    ;;
  esac

  case "$file" in
  # 纯文档与 agent 流程：不跑业务 job。
  *.md | docs/* | LICENSE | NOTICE | .agents/* | .claude/* | skills-lock.json | .air.toml | .github/ISSUE_TEMPLATE/*)
    return
    ;;
  # Renovate 配置只被 Mend 的 Renovate App 读取，不改变任何 job 的行为；.trivyignore.yaml 只在
  # release.yml 的镜像扫描与嵌入二进制扫描里生效。两者的格式都由 go job 里的 workflow-check 校验，所以只跑 go。
  .github/renovate.json | .trivyignore.yaml)
    set_flags go
    ;;
  # CI 自身、工具链与镜像：全量。scripts/mise-release.env 是每个 job 的 ci-setup-mise.sh 与 Dockerfile
  # 共用的 mise 引导版本，升级 mise 要让所有引导路径都跑一遍。
  .github/* | mise.toml | scripts/ci-* | scripts/mise-release.env | Dockerfile*)
    set_all
    ;;
  # 服务端经 replace 把 sdk/go 编进二进制（自监控链路），所以也要跑构建与 e2e。
  sdk/go/*)
    set_flags sdk_go go build e2e ha
    set_journeys go browser
    ;;
  sdk/node/*)
    set_flags sdk_node
    set_journeys node browser
    ;;
  sdk/browser/*)
    set_flags sdk_browser
    set_journeys browser
    ;;
  sdk/python/*)
    set_flags sdk_python
    set_journeys python
    ;;
  sdk/scripts/*)
    set_flags go sdk_go sdk_node sdk_browser sdk_python sdk_journeys
    ;;
  sdk/*)
    set_flags sdk_go sdk_node sdk_browser sdk_python sdk_journeys
    ;;
  web/*)
    set_flags web build
    set_journeys browser
    ;;
  internal/db/* | internal/meta/* | internal/embeddedpg/* | entc.mod | entc.sum | tools/entgen/* | scripts/generate-ent.sh)
    set_flags go build e2e ha meta_pg sdk_journeys
    ;;
  # 依赖升级可能动到 pgx / embedded-postgres，PG 元数据测试一起跑。
  go.mod | go.sum)
    set_flags go build e2e ha meta_pg sdk_journeys
    ;;
  # perf-gate 是独立的性能门禁程序（mise 的 performance-gates 与 HA 脚本用它），不在 cmd/lazy-observe 的依赖里，
  # 平台二进制不含它，所以不触发 SDK journey；其余标志与 cmd/* 一致。
  cmd/perf-gate/*)
    set_flags go build e2e ha
    ;;
  cmd/* | internal/*)
    set_flags go build e2e ha sdk_journeys
    ;;
  # HA 门禁脚本与它现场编译的辅助程序；也保留单副本 e2e 原有的触发。
  scripts/e2e-multi-replica.sh | scripts/lib/uv-python.sh | scripts/e2e-path-router/* | scripts/e2e-otlp-payload/* | scripts/e2e-otlp-sink/*)
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
  # JS 许可证检查被 web 与两个 JS SDK job 共用；本身是 shell 脚本也走 go 的 workflow-check。
  scripts/check-js-licenses.sh)
    set_flags go web sdk_node sdk_browser
    ;;
  # Python 许可证检查由 sdk-python 与 sdk-publish 共用。
  scripts/check-python-licenses.sh)
    set_flags go sdk_python
    ;;
  # 根 .golangci.yml 同时约束主仓库与 sdk/go。
  .golangci.yml)
    set_flags go sdk_go
    ;;
  .dockerignore | .goreleaser.yaml)
    set_flags go build arm64
    ;;
  scripts/* | deploy/* | bench/* | tools/* | config.example.yaml | .gitignore)
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

# sdk_journeys_only：见文件头部的说明。sdk_journeys 为 false 或要全跑时为空。
journeys_only=''
if [[ ${out[sdk_journeys]} == true && $journey_full == false ]]; then
  for language in "${journey_languages[@]}"; do
    [[ -n ${journey_langs[$language]:-} ]] && journeys_only+="${journeys_only:+ }$language"
  done
fi
printf 'sdk_journeys_only=%s\n' "$journeys_only"
if [[ -n $output ]]; then
  printf 'sdk_journeys_only=%s\n' "$journeys_only" >>"$output"
fi

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
