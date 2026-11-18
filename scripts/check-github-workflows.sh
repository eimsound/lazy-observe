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

# 供应链：任何 uses: 都必须按 40 位提交 SHA 固定（docker:// 按 sha256 digest），只有本仓 ./ 路径例外。
# 与上面的白名单检查互补：覆盖 .github 下所有 YAML 与任意位置的 composite action（action.yml），
# 也覆盖流式写法（- { uses: ... }）、reusable workflow 与 docker:// 引用。
unpinned_uses() {
  local file n line ref
  # 任何形如 uses: 的键都要检查；解析不出引用的行同样算失败。
  local key_re='(^|[^A-Za-z0-9_-])["'\'']?uses["'\'']?[[:space:]]*:'
  local ref_re='^[[:space:]]*(-[[:space:]]+)?(\{[[:space:]]*)?["'\'']?uses["'\'']?[[:space:]]*:[[:space:]]*["'\'']?([^"'\''[:space:],}#]+)'
  local comment_re='^[[:space:]]*#'
  for file in "$@"; do
    n=0
    while IFS= read -r line || [[ -n $line ]]; do
      n=$((n + 1))
      [[ $line =~ $key_re ]] || continue
      [[ $line =~ $comment_re ]] && continue
      if [[ $line =~ $ref_re ]]; then
        ref=${BASH_REMATCH[3]}
        case $ref in
        ./*) continue ;;
        docker://*) [[ $ref =~ @sha256:[0-9a-f]{64}$ ]] && continue ;;
        *) [[ $ref =~ ^[^@]+@[0-9a-f]{40}$ ]] && continue ;;
        esac
      fi
      printf '%s:%s: %s\n' "$file" "$n" "$line"
    done <"$file"
  done
  return 0
}
pin_fixture=$(mktemp -d)
cat >"$pin_fixture/bad.yml" <<'EOF'
      - uses: actions/checkout@v4
      - { uses: actions/setup-go@v5 }
        uses: "docker://alpine:3.20"
    uses: eimsound/shared/.github/workflows/ci.yml@main
      - uses: actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9a
EOF
cat >"$pin_fixture/good.yml" <<'EOF'
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - { uses: actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 }
      - uses: ./.github/actions/local
        uses: docker://alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000
      # - uses: actions/checkout@v4
          causes: not-a-uses-key
EOF
pin_bad=$(unpinned_uses "$pin_fixture/bad.yml")
pin_good=$(unpinned_uses "$pin_fixture/good.yml")
rm -rf "$pin_fixture"
pin_bad_count=$(grep -c . <<<"$pin_bad" || true)
[[ $pin_bad_count -eq 5 ]] || fail "SHA pin self-test: expected 5 rejected refs, got: $pin_bad"
[[ -z $pin_good ]] || fail "SHA pin self-test: pinned refs were rejected: $pin_good"
mapfile -t pin_files < <(git ls-files --cached --others --exclude-standard -- \
  '.github/*.yml' '.github/*.yaml' 'action.yml' 'action.yaml' '*/action.yml' '*/action.yaml' | sort -u)
unpinned=$(unpinned_uses "${workflows[@]}" "${pin_files[@]}" | sort -u)
if [[ -n $unpinned ]]; then
  printf '%s\n' "$unpinned" >&2
  fail 'every uses: must pin a full 40-character commit SHA (docker:// must pin a sha256 digest)'
fi

# mise 引导：版本与两个架构的 sha256 只在 scripts/mise-release.env 维护，CI 脚本与 Dockerfile 都读它，
# 避免两份常量漂移或退回不固定的安装脚本。
mise_release=scripts/mise-release.env
[[ -f $mise_release ]] || fail "$mise_release is missing"
grep -Eq '^MISE_BOOTSTRAP_VERSION=v[0-9]+\.[0-9]+\.[0-9]+$' "$mise_release" ||
  fail "$mise_release must pin MISE_BOOTSTRAP_VERSION=vX.Y.Z"
for var in MISE_BOOTSTRAP_SHA256_AMD64 MISE_BOOTSTRAP_SHA256_ARM64; do
  grep -Eq "^${var}=[0-9a-f]{64}$" "$mise_release" ||
    fail "$mise_release must pin $var to a 64-character sha256"
done
for consumer in Dockerfile scripts/ci-setup-mise.sh; do
  [[ -f $consumer ]] || fail "$consumer is missing"
  grep -Fq 'mise-release.env' "$consumer" ||
    fail "$consumer must read the pinned mise release from $mise_release"
done
unverified_mise=$(grep -Hn 'https://mise\.run' Dockerfile scripts/ci-setup-mise.sh |
  grep -vE ':[0-9]+:[[:space:]]*#' || true)
if [[ -n $unverified_mise ]]; then
  printf '%s\n' "$unverified_mise" >&2
  fail 'mise must be downloaded from the pinned GitHub release, not the mise.run installer'
fi

# setup-buildx-action 默认拉未固定的 moby/buildkit:buildx-stable-1；每处都要用 driver-opts 钉具体版本 + digest。
buildkit_pin_re='^[[:space:]]+driver-opts:[[:space:]]*image=moby/buildkit:v[0-9]+\.[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}[[:space:]]*$'
unpinned_buildkit=$(for file in "${workflows[@]}"; do
  n=0 pending=
  while IFS= read -r line || [[ -n $line ]]; do
    n=$((n + 1))
    if [[ $line =~ uses:[[:space:]]+docker/setup-buildx-action@ ]]; then
      if [[ -n $pending ]]; then printf '%s\n' "$pending"; fi
      pending="$file:$n"
    elif [[ -n $pending ]]; then
      if [[ $line =~ $buildkit_pin_re ]]; then
        pending=
      elif [[ $line =~ ^[[:space:]]*-[[:space:]] || $line =~ ^[^[:space:]#] ]]; then
        printf '%s\n' "$pending"
        pending=
      fi
    fi
  done <"$file"
  if [[ -n $pending ]]; then printf '%s\n' "$pending"; fi
done)
if [[ -n $unpinned_buildkit ]]; then
  printf '%s\n' "$unpinned_buildkit" >&2
  fail 'docker/setup-buildx-action must set driver-opts: image=moby/buildkit:vX.Y.Z@sha256:<index digest>'
fi

# 按 job 名截取 workflow 里的 job 块（$1 文件，$2 job 名）。下面的断言必须按 job 定位：
# 否则「docker job 里写了 contents: write」这种检查会因为别的 job 满足而误判通过。
release_job() {
  awk -v header="  $2:" '$0 == header { on = 1; next } on && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { exit } on { print }' "$1"
}

# 定位器自测：只用一段假 workflow 证明截取确实按 job 边界停下来。
job_fixture=$(mktemp)
cat >"$job_fixture" <<'JOBFIXTURE'
jobs:
  docker:
    permissions:
      contents: read
  helm-values:
    needs: docker
    permissions:
      contents: write
  other:
    steps:
      - run: echo other
JOBFIXTURE
locator_docker=$(release_job "$job_fixture" docker)
locator_values=$(release_job "$job_fixture" helm-values)
locator_missing=$(release_job "$job_fixture" missing)
rm -f "$job_fixture"
[[ $locator_docker == *'contents: read'* ]] || fail 'job locator self-test: docker block not found'
[[ $locator_docker != *'contents: write'* ]] || fail 'job locator self-test: docker block ran into helm-values'
[[ $locator_values == *'contents: write'* ]] || fail 'job locator self-test: helm-values block not found'
[[ $locator_values != *'echo other'* ]] || fail 'job locator self-test: helm-values block ran into other'
[[ -z $locator_missing ]] || fail 'job locator self-test: unknown job returned text'

# 发版前的镜像漏洞扫描必须挡在发布之前：container-check 跑扫描，archive 依赖 container-check。
release_wf=.github/workflows/release.yml
if [[ -f $release_wf ]]; then
  container_check=$(release_job "$release_wf" container-check)
  [[ $container_check == *'run: bash scripts/check-image-vulns.sh lazy-observe:preflight-'* ]] ||
    fail "$release_wf: container-check must scan the preflight image with scripts/check-image-vulns.sh"
  archive_job=$(release_job "$release_wf" archive)
  [[ $archive_job == *'needs: [container-check, '* ]] ||
    fail "$release_wf: archive must need container-check so the image scan blocks publishing"

  # chart 不单独发布（没有 helm package/push），发版产物里带的是 digest 固定的 Helm values 片段。
  # 自家镜像的 digest 只有 docker/build-push-action 推完镜像才存在，所以回填落在 docker 之后的
  # helm-values job：由 scripts/render-helm-release-values.sh 生成、作为 Release 资产上传。
  # 该 job 单独存在是为了最小权限：docker job 会把 GITHUB_TOKEN 当 BuildKit secret 挂进构建，
  # 不能同时持有写仓库的权限。发版流程不能在本地真跑，这些静态断言是它唯一的门禁。
  docker_job=$(release_job "$release_wf" docker)
  helm_values_job=$(release_job "$release_wf" helm-values)
  [[ -n $helm_values_job ]] ||
    fail "$release_wf: missing a helm-values job that attaches the digest-pinned Helm values"

  # docker：只读，且把 digest 与版本号经 outputs 交出去。
  [[ $docker_job == *'contents: read'* ]] ||
    fail "$release_wf: docker must stay contents: read (it mounts the token into the build)"
  [[ $docker_job != *'contents: write'* ]] ||
    fail "$release_wf: docker must not hold contents: write; attach release assets from helm-values"
  [[ $docker_job == *'digest: ${{ steps.build.outputs.digest }}'* ]] ||
    fail "$release_wf: docker must expose steps.build.outputs.digest as a job output"
  [[ $docker_job == *'version: ${{ steps.build-meta.outputs.version }}'* ]] ||
    fail "$release_wf: docker must expose steps.build-meta.outputs.version as a job output"
  [[ $docker_job != *'gh release upload'* ]] ||
    fail "$release_wf: docker must not upload release assets; helm-values owns that step"

  # helm-values：只拿 contents: write，只在 tag 推送时跑，消费 docker 的 outputs。
  [[ $helm_values_job == *'needs: docker'* ]] ||
    fail "$release_wf: helm-values must need docker so the digest exists before it runs"
  [[ $helm_values_job == *"if: github.event_name == 'push'"* ]] ||
    fail "$release_wf: helm-values must only run on tag pushes (workflow_dispatch publishes nothing)"
  [[ $helm_values_job == *'contents: write'* ]] ||
    fail "$release_wf: helm-values needs contents: write to attach the Helm values asset"
  [[ $helm_values_job == *'GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}'* ]] ||
    fail "$release_wf: helm-values must authenticate gh with the job token"
  [[ $helm_values_job == *'needs.docker.outputs.digest'* ]] ||
    fail "$release_wf: helm-values must pin the digest docker published"
  [[ $helm_values_job == *'bash scripts/render-helm-release-values.sh'* ]] ||
    fail "$release_wf: helm-values must render the values fragment with scripts/render-helm-release-values.sh"
  [[ $helm_values_job == *'gh release upload'* ]] ||
    fail "$release_wf: helm-values must attach the digest-pinned Helm values to the release"
  [[ $helm_values_job == *'--clobber'* ]] ||
    fail "$release_wf: helm-values must pass --clobber so a re-run replaces the asset"
  [[ $helm_values_job == *'--repo "$GITHUB_REPOSITORY"'* ]] ||
    fail "$release_wf: helm-values must name the repository explicitly when uploading"
fi

# 镜像扫描例外清单：每条都要有理由和到期日，过期条目由 Trivy 自动失效。
trivyignore=.trivyignore.yaml
[[ -f $trivyignore ]] || fail "$trivyignore is missing"
bad_ignores=$(awk '
  function flush() { if (id != "" && !(statement && expiry)) print id; id = ""; statement = 0; expiry = 0 }
  /^[[:space:]]*#/ { next }
  /^[[:space:]]*-[[:space:]]+id:/ { flush(); id = $0; sub(/^.*id:[[:space:]]*/, "", id); next }
  /^[[:space:]]+statement:[[:space:]]*[^[:space:]]/ { statement = 1 }
  /^[[:space:]]+expired_at:[[:space:]]*[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9][[:space:]]*$/ { expiry = 1 }
  /^[^[:space:]-]/ { flush() }
  END { flush() }
' "$trivyignore")
if [[ -n $bad_ignores ]]; then
  printf '%s\n' "$bad_ignores" >&2
  fail "$trivyignore entries must carry a statement and an expired_at date (YYYY-MM-DD)"
fi

# Dependabot 覆盖面：每个依赖清单所在目录都要有对应生态的条目。
dependabot=.github/dependabot.yml
[[ -f $dependabot ]] || fail "$dependabot is missing"
dependabot_entries=$(awk '
  /^[[:space:]]*-[[:space:]]+package-ecosystem:/ { eco = $NF; gsub(/["'\'']/, "", eco) }
  /^[[:space:]]+directory:/ { dir = $NF; gsub(/["'\'']/, "", dir); print eco " " dir }
' "$dependabot")
expected_entries=$(
  printf 'github-actions /\n'
  git ls-files --cached --others --exclude-standard -- '*go.mod' '*pnpm-lock.yaml' '*pyproject.toml' '*Dockerfile' |
    while IFS= read -r manifest; do
      case ${manifest##*/} in
      go.mod) eco=gomod ;;
      pnpm-lock.yaml) eco=npm ;;
      pyproject.toml) eco=pip ;;
      Dockerfile) eco=docker ;;
      *) continue ;;
      esac
      dir=${manifest%/*}
      [[ $dir == "$manifest" ]] && dir=
      printf '%s /%s\n' "$eco" "$dir"
    done
)
missing_dependabot=$(comm -23 <(sort -u <<<"$expected_entries") <(sort -u <<<"$dependabot_entries"))
if [[ -n $missing_dependabot ]]; then
  printf '%s\n' "$missing_dependabot" >&2
  fail "$dependabot must cover every manifest directory listed above"
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
