#!/usr/bin/env bash
set -euo pipefail

# scripts/ci-changes.sh 的「悄悄漏跑」防线：被跳过的 job 在 ci-ok 里算通过，所以只守三条兜底——
#   1. CI 自身、工具链、未分类路径一律全量；
#   2. 取不到 base 的事件一律全量；
#   3. 服务端 Go 代码一定打开 go（Go 检查不能被分组误判跳过）。
# 各改动面的精确分组（哪个路径开哪几个标志、sdk_journeys_only 的语言范围）是实现细节，不在这里断言。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
script="$root/scripts/ci-changes.sh"
flags=(go build e2e ha web meta_pg arm64 sdk_go sdk_node sdk_browser sdk_python sdk_journeys pins)
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

expect_go() { # files
  grep -qx 'go=true' <<<"$(changes_for "$1")" || {
    printf 'FAIL [%s] go must be true\n' "$1" >&2
    failures=$((failures + 1))
  }
}

expect_full '.github/workflows/ci.yml'
expect_full 'mise.toml'
expect_full 'scripts/ci-changes.sh'
expect_full 'Dockerfile'
expect_full 'scripts/mise-release.env'
expect_full 'Makefile' # 未分类路径

expect_go 'internal/gateway/router.go'
expect_go 'cmd/lazy-observe/main.go'
expect_go 'go.mod'

got=$(GITHUB_OUTPUT='' bash "$script" --all 2>/dev/null)
for flag in "${flags[@]}"; do
  grep -qx "${flag}=true" <<<"$got" || {
    printf 'FAIL [--all] %s is not true\n' "$flag" >&2
    failures=$((failures + 1))
  }
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
printf 'PASS: ci-changes full-run fallbacks\n'
