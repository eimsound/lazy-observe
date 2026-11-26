#!/usr/bin/env bash
set -euo pipefail

# scripts/ci-changes.sh 的「悄悄漏跑」防线：被跳过的 job 在 ci-ok 里算通过，所以守住：
#   1. CI 自身、工具链、未分类路径、取不到 base 一律全量（fail-closed）；
#   2. 每条安全 / 供应链 / 数据库 / Helm / SDK 门禁的真实输入文件清单都必须命中对应 job，
#      杜绝任何门禁因路径分组调整被静默跳过。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
script="$root/scripts/ci-changes.sh"
flags=(go build e2e ha web meta_pg arm64 sdk_go sdk_node sdk_browser sdk_python sdk_journeys trivy_pin docs design)
failures=0

changes_for() {
  tr ',' '\n' <<<"$1" | GITHUB_OUTPUT='' bash "$script" --stdin 2>/dev/null
}

expect_full() { # files
  local got flag
  got=$(changes_for "$1")
  for flag in "${flags[@]}"; do
    grep -qx "${flag}=true" <<<"$got" || {
      printf 'FAIL [%s] %s must be true (full run)\n' "$1" "$flag" >&2
      failures=$((failures + 1))
    }
  done
}

expect_flag() { # files flag [flag ...]
  local files=$1 got flag
  shift
  got=$(changes_for "$files")
  for flag in "$@"; do
    grep -qx "${flag}=true" <<<"$got" || {
      printf 'FAIL [%s] %s must be true\n' "$files" "$flag" >&2
      failures=$((failures + 1))
    }
  done
}

expect_only_flags() { # files flag [flag ...]
  local files=$1 got flag want
  shift
  got=$(changes_for "$files")
  for flag in "${flags[@]}"; do
    want=false
    for expected in "$@"; do
      if [[ $flag == "$expected" ]]; then
        want=true
        break
      fi
    done
    grep -qx "${flag}=${want}" <<<"$got" || {
      printf 'FAIL [%s] %s must be %s\n' "$files" "$flag" "$want" >&2
      failures=$((failures + 1))
    }
  done
}

# --- 1. 全量兜底（CI 自身、工具链、未分类路径） ---
expect_full '.github/workflows/ci.yml'
expect_full '.github/workflows/docs.yml'
expect_full '.github/workflows/release.yml'
expect_full '.github/workflows/sdk-release.yml'
expect_full 'mise.toml'
expect_full 'scripts/ci-changes.sh'
expect_full 'scripts/ci-setup-mise.sh'
expect_full 'scripts/mise-release.env'
expect_full 'Dockerfile'
expect_full 'third_party/go/THIRD_PARTY_NOTICES.txt' # 未显式降级的第三方通知走全量兜底
expect_full 'Makefile' # 未分类路径

# --- 2. 安全与供应链门禁真实输入（workflow-check、7 天发布年龄、Trivy、许可证、打包链路 arm64） ---
expect_flag '.github/renovate.json' go
expect_flag '.trivyignore.yaml' go
expect_flag 'scripts/release-age.env' go
expect_flag 'scripts/check-github-workflows.sh' go
expect_flag 'scripts/check-pipefail-pipelines.sh' go
expect_flag 'scripts/install-trivy.sh' go trivy_pin
expect_flag 'scripts/check-image-vulns.sh' go
expect_flag 'scripts/check-storage-bin-vulns.sh' go
expect_flag 'scripts/install.sh' go
expect_flag 'scripts/check-install.sh' go
expect_flag 'scripts/check-release-prerequisites.sh' go
expect_flag 'scripts/check-go-licenses.sh' go
expect_flag 'scripts/third-party-notices.sh' go
expect_flag 'scripts/check-js-licenses.sh' go web sdk_node sdk_browser
expect_flag 'scripts/web-third-party-licenses.sh' go web
expect_flag 'web/THIRD_PARTY_LICENSES.md' web
expect_flag 'scripts/check-python-licenses.sh' go sdk_python
expect_flag '.golangci.yml' go sdk_go
expect_flag 'web/pnpm-workspace.yaml' go web build sdk_journeys
expect_flag 'web/pnpm-lock.yaml' go web build sdk_journeys
expect_flag 'sdk/node/pnpm-workspace.yaml' go sdk_node sdk_journeys
expect_flag 'sdk/node/pnpm-lock.yaml' go sdk_node sdk_journeys
expect_flag 'sdk/browser/pnpm-workspace.yaml' go sdk_browser web build sdk_journeys
expect_flag 'sdk/browser/pnpm-lock.yaml' go sdk_browser web build sdk_journeys
expect_flag 'sdk/python/pyproject.toml' go sdk_python sdk_journeys
# 打包链路与原生 arm64 冒烟真实输入
expect_flag '.dockerignore' go build arm64
expect_flag '.goreleaser.yaml' go build arm64
expect_flag 'internal/embed/embed.go' go build e2e ha arm64 sdk_journeys
expect_flag 'scripts/download-storage-bins.sh' go build e2e ha arm64 sdk_journeys
expect_flag 'scripts/prepare-embedded-storage.sh' go build arm64
expect_flag 'scripts/prepare-web-assets.sh' go build e2e arm64 sdk_journeys
expect_flag 'scripts/check-embedded-artifact.sh' go build arm64
expect_flag 'scripts/check-container-smoke.sh' go build arm64
expect_flag 'scripts/lib/uv-python.sh' go build e2e ha arm64

# --- 3. 数据库、Ent 与元数据门禁真实输入（migration-check、ent-generate-check、meta-pg） ---
expect_flag 'internal/db/schema/project.go' go build e2e ha meta_pg sdk_journeys
expect_flag 'internal/db/ent/client.go' go build e2e ha meta_pg sdk_journeys
expect_flag 'internal/meta/store.go' go build e2e ha meta_pg sdk_journeys
expect_flag 'internal/meta/migrations/postgres/0001_baseline.sql' go build e2e ha meta_pg sdk_journeys
expect_flag 'internal/embeddedpg/pg.go' go build e2e ha meta_pg sdk_journeys
expect_flag 'entc.mod' go build e2e ha meta_pg sdk_journeys
expect_flag 'entc.sum' go build e2e ha meta_pg sdk_journeys
expect_flag 'tools/entgen/main.go' go build e2e ha meta_pg sdk_journeys
expect_flag 'scripts/generate-ent.sh' go build e2e ha meta_pg sdk_journeys
expect_flag 'scripts/check-migrations.sh' go
expect_flag 'go.mod' go build e2e ha meta_pg arm64 sdk_journeys
expect_flag 'go.sum' go build e2e ha meta_pg arm64 sdk_journeys

# --- 4. Helm 与配置契约门禁真实输入（helm-lint、helm-template-test、config.Load） ---
expect_flag 'deploy/helm/lazy-observe/Chart.yaml' go
expect_flag 'deploy/helm/lazy-observe/values.yaml' go
expect_flag 'deploy/helm/lazy-observe/values.schema.json' go
expect_flag 'deploy/helm/lazy-observe/values-standalone-ha-cluster.yaml' go
expect_flag 'deploy/helm/lazy-observe/templates/configmap.yaml' go
expect_flag 'deploy/helm/lazy-observe/templates/deployment.yaml' go
expect_flag 'scripts/helm-template-test.sh' go
expect_flag 'scripts/render-helm-release-values.sh' go
expect_flag 'scripts/check-helm-config/main.go' go
expect_flag 'internal/config/config.go' go build e2e ha sdk_journeys

# --- 5. 服务端、HA 与 SDK 门禁真实输入 ---
expect_flag 'internal/gateway/router.go' go build e2e ha sdk_journeys
expect_flag 'cmd/lazy-observe/main.go' go build e2e ha sdk_journeys
expect_flag 'cmd/perf-gate/main.go' go build e2e ha
expect_flag 'scripts/e2e-multi-replica.sh' go e2e ha
expect_flag 'bench/performance-gates.json' go ha
expect_flag 'sdk/go/go.mod' go sdk_go build e2e ha sdk_journeys
expect_flag 'sdk/go/client.go' go sdk_go build e2e ha sdk_journeys
expect_flag 'sdk/node/packages/node/src/index.ts' sdk_node sdk_journeys
expect_flag 'sdk/browser/packages/browser/src/index.ts' sdk_browser web build sdk_journeys
expect_flag 'sdk/python/lazy_observe/__init__.py' sdk_python sdk_journeys
expect_flag 'sdk/scripts/sdk-publish.sh' go sdk_go sdk_node sdk_browser sdk_python sdk_journeys
expect_flag 'scripts/sdk-journeys.sh' go sdk_journeys
expect_flag 'scripts/e2e-web-self-monitoring.mjs' go sdk_journeys

# --- 5.5 Zensical 文档站变更面（纯文档站不触发重型业务 job；docs/pyproject.toml 同时触发 go 的 7 天发布年龄校验） ---
expect_only_flags 'docs/index.md' docs
expect_only_flags 'docs/zh/quickstart.md' docs
expect_only_flags 'docs/zensical.toml' docs
expect_only_flags 'docs/zensical.zh.toml' docs
expect_only_flags 'docs/pyproject.toml' go docs
expect_only_flags 'README.md'

# --- 5.6 DESIGN.md（只跑 design-lint；web/DESIGN.md 等其它 *.md 不触发）与 agent 工具配置（不跑业务 job） ---
expect_only_flags 'DESIGN.md' design
expect_only_flags 'web/DESIGN.md'
expect_only_flags '.mcp.json'
expect_only_flags '.impeccable/config.json'
expect_only_flags '.agents/plugins/playwright/mcp_config.json'

# --- 6. --all 与无可用 base 事件兜底 ---
got=$(GITHUB_OUTPUT='' bash "$script" --all 2>/dev/null)
for flag in "${flags[@]}"; do
  grep -qx "${flag}=true" <<<"$got" || {
    printf 'FAIL [--all] %s is not true\n' "$flag" >&2
    failures=$((failures + 1))
  }
done

got=$(cd "$root" && CI_EVENT_NAME=workflow_dispatch GITHUB_OUTPUT='' bash "$script" 2>/dev/null)
grep -qx 'sdk_python=true' <<<"$got" && grep -qx 'ha=true' <<<"$got" || {
  printf 'FAIL [workflow_dispatch] expected full run\n' >&2
  failures=$((failures + 1))
}
got=$(cd "$root" && CI_EVENT_NAME=push CI_BEFORE_SHA=0000000000000000000000000000000000000000 GITHUB_OUTPUT='' \
  bash "$script" 2>/dev/null)
grep -qx 'web=true' <<<"$got" || {
  printf 'FAIL [push from zero sha] expected full run\n' >&2
  failures=$((failures + 1))
}

if ((failures > 0)); then
  printf 'ci-changes-test: %d failure(s)\n' "$failures" >&2
  exit 1
fi
printf 'PASS: ci-changes full-run fallbacks and gate input routing\n'
