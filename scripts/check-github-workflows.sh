#!/usr/bin/env bash
set -euo pipefail

# 静态校验 .github/workflows：不需要 runner，CI 的 go job 与本地 mise run workflow-check 都跑它。

shopt -s nullglob
workflows=(.github/workflows/*.yml .github/workflows/*.yaml)
if ((${#workflows[@]} == 0)); then
  printf 'no GitHub workflows found under .github/workflows\n' >&2
  exit 1
fi

fail() {
  printf 'workflow check: %s\n' "$*" >&2
  exit 1
}

# 迁移遗留：Gitea 上下文、自建 runner、代理与私有 SDK 拉取都不应再出现。
if grep -En 'gitea\.|GITEA_|CI_HTTP_PROXY|ci-proxy\.sh|ci-prepare-sdk\.sh|_SDK_KEY|lazy-sdk=' "${workflows[@]}"; then
  fail 'workflow still references Gitea, the CI proxy, or the private SDK checkout'
fi

# 公开仓库不挂自建 runner：fork PR 可以在自建 runner 上执行任意代码。
# 表达式形式的 runs-on 只允许 matrix.runner，或只含这两个托管 runner 字面量的表达式。
bad_runners=$(grep -En '^[[:space:]]*runs-on:' "${workflows[@]}" | while IFS= read -r line; do
  value=${line#*runs-on:}
  value=${value#"${value%%[![:space:]]*}"}
  value=${value%"${value##*[![:space:]]}"}
  case "$value" in
  ubuntu-24.04 | ubuntu-24.04-arm | '${{ matrix.runner }}') continue ;;
  '${{ '*' }}')
    literals=$(grep -oE "'[^']*'" <<<"$value" | tr -d "'" | grep -vxE 'ubuntu-24\.04|ubuntu-24\.04-arm|amd64|arm64' || true)
    [[ -z $literals && $value == *ubuntu-24.04* && $value != *self-hosted* ]] && continue
    ;;
  esac
  printf '%s\n' "$line"
done)
if [[ -n $bad_runners ]]; then
  printf '%s\n' "$bad_runners" >&2
  fail 'jobs must run on GitHub-hosted ubuntu-24.04 / ubuntu-24.04-arm runners'
fi

# 每个 workflow 都要显式声明最小权限。
for workflow in "${workflows[@]}"; do
  grep -Eq '^permissions:' "$workflow" || fail "$workflow must declare top-level permissions"
done

if grep -En '^[[:space:]]*key:.*github\.sha' "${workflows[@]}"; then
  fail 'cache keys must be derived from tool or dependency inputs, not the commit SHA'
fi

unpinned_actions=$(grep -En '^[[:space:]]*-[[:space:]]*uses:|^[[:space:]]*uses:' "${workflows[@]}" |
  grep -vE 'uses:[[:space:]]+(actions/(checkout|cache|upload-artifact|download-artifact|setup-node|setup-python)|pypa/gh-action-pypi-publish|docker/(setup-qemu-action|setup-buildx-action|login-action|metadata-action|build-push-action))@[0-9a-f]{40}[[:space:]]+#[[:space:]]+v[0-9]+(\.[0-9]+){1,2}(-[A-Za-z0-9.-]+)?[[:space:]]*$' || true)
if [[ -n $unpinned_actions ]]; then
  printf '%s\n' "$unpinned_actions" >&2
  fail 'workflow actions must use an allow-listed owner/name, a full commit SHA, and an exact version comment'
fi

# upload/download-artifact v3 在 GitHub 上已停用。
old_artifacts=$(grep -En 'uses:[[:space:]]+actions/(upload|download)-artifact@[0-9a-f]{40}[[:space:]]+#[[:space:]]+v[0-3]\.' \
  "${workflows[@]}" || true)
if [[ -n $old_artifacts ]]; then
  printf '%s\n' "$old_artifacts" >&2
  fail 'actions/upload-artifact and download-artifact must be v4 or newer on GitHub'
fi

# job 直接跑在 GitHub 托管 runner 上，工具由 mise 安装并缓存；不再使用预构建的 job 容器镜像。
job_containers=$(grep -En '^    container:' "${workflows[@]}" || true)
if [[ -n $job_containers ]]; then
  printf '%s\n' "$job_containers" >&2
  fail 'jobs must run directly on hosted runners; install tools with scripts/ci-setup-mise.sh instead of a job container'
fi

if grep -En '^[[:space:]]*path:[[:space:]]*~/.local/share/pnpm/store' "${workflows[@]}"; then
  fail 'pnpm cache must use the store directory configured by scripts/ci-setup-mise.sh'
fi

missing_arch=$(grep -En '^[[:space:]]*key:.*runner\.os' "${workflows[@]}" | grep -v 'runner\.arch' || true)
if [[ -n $missing_arch ]]; then
  printf '%s\n' "$missing_arch" >&2
  fail 'platform-specific cache keys must include runner.arch'
fi

cache_keys=$(mktemp)
trap 'rm -f "$cache_keys"' EXIT
GITHUB_OUTPUT=$cache_keys bash scripts/ci-cache-keys.sh
if [[ $(wc -l <"$cache_keys") -ne 4 ]]; then
  fail 'cache key helper must emit exactly four keys'
fi
for key in mise go storage pnpm-web; do
  grep -Eq "^${key}=[0-9a-f]{64}$" "$cache_keys" || fail "cache key helper emitted an invalid $key key"
done

# 增量 CI 契约：ci.yml 第一个 job 是 changes；ci-ok 是 if: always() 的汇总 job，
# needs 覆盖 ci.yml 里所有其它 job，否则某个 job 失败也不会挡住合并。
ci=.github/workflows/ci.yml
[[ -f $ci ]] || fail "$ci is missing"
mapfile -t ci_jobs < <(awk '
  /^jobs:[[:space:]]*$/ { in_jobs = 1; next }
  /^[^[:space:]#]/ { in_jobs = 0 }
  in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { name = $1; sub(/:$/, "", name); print name }
' "$ci")
((${#ci_jobs[@]} > 2)) || fail "$ci must define changes, business jobs, and ci-ok"
[[ ${ci_jobs[0]} == changes ]] || fail "$ci: the first job must be changes"
[[ ${ci_jobs[-1]} == ci-ok ]] || fail "$ci: the last job must be ci-ok"
ci_ok=$(sed -n '/^  ci-ok:$/,$p' "$ci")
[[ $ci_ok == *'if: always()'* ]] || fail "$ci: ci-ok must run with if: always()"
mapfile -t ci_ok_needs < <(sed -n '/^  ci-ok:$/,$p' "$ci" | sed -n '/^    needs:$/,/^    [a-z-]*:/p' |
  sed -n 's/^      - \([A-Za-z0-9_-]*\)$/\1/p')
# 先收齐再匹配：pipefail 下 printf | grep -q 会在 grep 提前退出时让 printf 收到 SIGPIPE（141），门禁偶发变红。
ci_ok_needs_list=$(printf '%s\n' "${ci_ok_needs[@]}")
for job in "${ci_jobs[@]}"; do
  [[ $job == ci-ok ]] && continue
  [[ $'\n'$ci_ok_needs_list$'\n' == *$'\n'"$job"$'\n'* ]] || fail "$ci: ci-ok must need job $job"
  if [[ $job != changes ]]; then
    job_block=$(awk -v job="$job" '
      $0 == "  " job ":" { on = 1; next }
      on && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { exit }
      on { print }
    ' "$ci")
    [[ $job_block == *'needs: changes'* && $job_block == *'if: needs.changes.outputs.'* ]] ||
      fail "$ci: job $job must depend on changes and gate on one of its outputs"
  fi
done

# changes 作业输出的每个标志都要在 ci.yml 里声明为 job output。
for flag in $(bash scripts/ci-changes.sh --all 2>/dev/null | sed -n 's/=.*//p'); do
  grep -Eq "^      ${flag}: \\$\\{\\{ steps\\.detect\\.outputs\\.${flag} \\}\\}$" "$ci" ||
    fail "$ci: changes job does not expose output $flag"
done

# 显式关闭 actionlint 的 shellcheck 集成：runner 与开发机上的 shellcheck 版本不一
#（甚至是坏掉的 mise shim），结果不确定。shell 片段的 lint
# 要做就把 shellcheck 钉进 mise.toml 再打开。
actionlint -shellcheck= "${workflows[@]}"
