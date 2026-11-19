#!/usr/bin/env bash
set -euo pipefail

# scripts/ci-changes.sh 的分组规则测试：喂文件列表，断言输出的标志。
# expect "<files, 逗号分隔>" "<应为 true 的标志, 空格分隔>"；未列出的标志必须为 false。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
script="$root/scripts/ci-changes.sh"
flags=(go build e2e ha web meta_pg arm64 sdk_go sdk_node sdk_browser sdk_python sdk_journeys pins)
all="${flags[*]}"
failures=0

expect() {
  local files=$1 want=$2 got flag value expected
  got=$(tr ',' '\n' <<<"$files" | GITHUB_OUTPUT='' bash "$script" --stdin 2>/dev/null)
  for flag in "${flags[@]}"; do
    value=$(sed -n "s/^${flag}=//p" <<<"$got")
    expected=false
    [[ " $want " == *" $flag "* ]] && expected=true
    if [[ $value != "$expected" ]]; then
      printf 'FAIL [%s] %s=%s, want %s\n' "$files" "$flag" "${value:-<missing>}" "$expected" >&2
      failures=$((failures + 1))
    fi
  done
}

expect_matrix() {
  local files=$1 want=$2 got
  got=$(tr ',' '\n' <<<"$files" | GITHUB_OUTPUT='' bash "$script" --stdin 2>/dev/null | sed -n 's/^build_matrix=//p')
  if [[ $got != "$want" ]]; then
    printf 'FAIL [%s] build_matrix=%s, want %s\n' "$files" "$got" "$want" >&2
    failures=$((failures + 1))
  fi
}

# 纯文档：所有业务 job 跳过。
expect 'README.md' ''
expect 'docs/adr/0012-x.md,CHANGELOG.md,LICENSE,.agents/skills/do/SKILL.md' ''
expect 'CLAUDE.md,sdk/CLAUDE.md,.claude/skills' ''
expect 'sdk/node/README.md,docs/sdk/go.md,.github/ISSUE_TEMPLATE/bug_report.yml' ''
expect '' ''

# SDK：只跑对应语言；Go SDK 额外跑服务端 Go 检查。
expect 'sdk/node/packages/node/src/index.ts' 'sdk_node sdk_journeys'
expect 'sdk/browser/packages/react/package.json' 'sdk_browser sdk_journeys'
expect 'sdk/python/lazyobserve/client.py' 'sdk_python sdk_journeys'
expect 'sdk/go/delivery.go' 'sdk_go go build e2e ha sdk_journeys'
expect 'sdk/scripts/sdk-publish.sh' 'sdk_go sdk_node sdk_browser sdk_python sdk_journeys'

# 前端：web + 嵌入前端的 amd64 构建。
expect 'web/src/App.tsx' 'web build sdk_journeys'

# 服务端：Go 代码同时打开多副本 HA 功能门禁。
expect 'internal/gateway/router.go' 'go build e2e ha sdk_journeys'
expect 'cmd/lazy-observe/main.go' 'go build e2e ha sdk_journeys'
expect 'internal/meta/store.go' 'go build e2e ha meta_pg sdk_journeys'
expect 'internal/db/schema/rule.go' 'go build e2e ha meta_pg sdk_journeys'
expect 'tools/entgen/main.go' 'go build e2e ha meta_pg sdk_journeys'
expect 'scripts/e2e-restart.sh' 'go e2e'
expect 'scripts/e2e.sh' 'go e2e'
expect 'deploy/helm/lazy-observe/values.yaml' 'go'
expect 'config.example.yaml' 'go'

# HA 门禁自身、它现场编译的辅助程序、读取的预算与存储二进制下载。
expect 'scripts/e2e-multi-replica.sh' 'go e2e ha pins'
expect 'scripts/e2e-path-router/main.go' 'go e2e ha'
expect 'scripts/e2e-otlp-payload/main.go' 'go e2e ha'
expect 'scripts/e2e-otlp-sink/main.go' 'go e2e ha'
expect 'bench/performance-gates.json' 'go ha'
expect 'bench/baseline.json' 'go'
expect 'scripts/install.sh' 'go'
expect '.gitignore' 'go'
# Dependabot 配置与镜像扫描例外清单只影响 GitHub 服务和 release.yml，由 go job 的 workflow-check 校验。
expect '.github/dependabot.yml' 'go'
expect '.trivyignore.yaml' 'go'
expect 'scripts/check-image-vulns.sh' 'go pins'
# 手工固定点：改动要触发 pins 门禁（联网查 release 发布时间）；门禁脚本自身、年龄常量同理，其测试只需 go。
expect 'scripts/mise-release.env' 'go pins'
expect 'scripts/install-trivy.sh' 'go pins'
expect 'scripts/check-pinned-releases.sh' 'go pins'
expect 'scripts/release-age.env' 'go pins'
expect 'scripts/check-pinned-releases-test.sh' 'go'
expect 'scripts/check-js-licenses.sh' 'web sdk_node sdk_browser'

# 打包链路：加 arm64。
expect 'go.mod' 'go build e2e ha meta_pg arm64 sdk_journeys'
expect 'go.sum' 'go build e2e ha meta_pg arm64 sdk_journeys'
expect 'internal/embed/bins/bins_linux_amd64.go' 'go build e2e ha arm64 sdk_journeys'
expect 'scripts/download-storage-bins.sh' 'go build e2e ha arm64 sdk_journeys pins'
expect 'scripts/prepare-web-assets.sh' 'go build e2e arm64 sdk_journeys'
expect '.goreleaser.yaml' 'go build arm64'
expect_matrix 'go.mod' '[{"arch":"amd64","runner":"ubuntu-24.04"},{"arch":"arm64","runner":"ubuntu-24.04-arm"}]'
expect_matrix 'internal/gateway/router.go' '[{"arch":"amd64","runner":"ubuntu-24.04"}]'

# SDK journey：SDK、服务端 Go、前端与 storage 下载变化时跑；纯文档、Helm、基准与 HA 脚本不跑。
expect 'sdk/go/e2e.sh' 'sdk_go go build e2e ha sdk_journeys'
expect 'sdk/browser/e2e/performance-journey.spec.ts' 'sdk_browser sdk_journeys'
expect 'sdk/node/e2e.sh' 'sdk_node sdk_journeys'
expect 'sdk/python/e2e.sh' 'sdk_python sdk_journeys'
expect 'scripts/sdk-journeys.sh' 'go sdk_journeys'
expect 'sdk/browser/README.md,sdk/python/README.md' ''

# CI 自身、工具链与未知路径：全量。
expect '.github/workflows/ci.yml' "$all"
expect 'mise.toml' "$all"
expect 'scripts/ci-changes.sh' "$all"
expect 'Dockerfile' "$all"
expect 'Makefile' "$all"

# 不打开 HA 门禁的改动面：前端、非 Go SDK、文档、Helm。
expect 'web/src/App.tsx,sdk/node/packages/node/src/index.ts,sdk/browser/packages/react/package.json' \
  'web build sdk_node sdk_browser sdk_journeys'
expect 'docs/deployment.md,deploy/helm/lazy-observe/templates/deployment.yaml,sdk/python/lazyobserve/client.py' \
  'go sdk_python sdk_journeys'

# 组合：取并集。
expect 'README.md,sdk/python/pyproject.toml,web/package.json' 'sdk_python web build sdk_journeys'

# --all：全量。
got=$(GITHUB_OUTPUT='' bash "$script" --all 2>/dev/null)
for flag in "${flags[@]}"; do
  if ! grep -qx "${flag}=true" <<<"$got"; then
    printf 'FAIL [--all] %s is not true\n' "$flag" >&2
    failures=$((failures + 1))
  fi
done

# 没有可用 base 的事件按全量处理。
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
printf 'PASS: ci-changes grouping rules\n'
