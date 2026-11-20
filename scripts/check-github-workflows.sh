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

# 供应链：任何 uses: 都必须按 40 位提交 SHA 固定（docker:// 按 sha256 digest），只有本仓 ./ 路径例外。
# 覆盖 .github 下所有 YAML 与任意位置的 composite action（action.yml），
# 也覆盖流式写法（- { uses: ... }）、reusable workflow 与 docker:// 引用。
# 任何形如 uses: 的键都要检查；解析不出引用的行同样算失败。
uses_key_re='(^|[^A-Za-z0-9_-])["'\'']?uses["'\'']?[[:space:]]*:'
uses_ref_re='^[[:space:]]*(-[[:space:]]+)?(\{[[:space:]]*)?["'\'']?uses["'\'']?[[:space:]]*:[[:space:]]*["'\'']?([^"'\''[:space:],}#]+)'
uses_comment_re='^[[:space:]]*#'
unpinned_uses() {
  local file n line ref
  for file in "$@"; do
    n=0
    while IFS= read -r line || [[ -n $line ]]; do
      n=$((n + 1))
      [[ $line =~ $uses_key_re ]] || continue
      [[ $line =~ $uses_comment_re ]] && continue
      if [[ $line =~ $uses_ref_re ]]; then
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

# 供应链：action 属主白名单。SHA 固定只保证代码不被换掉，管不了「该不该信任这个属主」；
# 引入新属主等于让没审过的代码带着仓库 token 运行，所以先审属主、再加进这里。docker:// 没有属主，不收。
trusted_action_owners='actions|docker|pypa'
untrusted_uses() {
  local file n line
  for file in "$@"; do
    n=0
    while IFS= read -r line || [[ -n $line ]]; do
      n=$((n + 1))
      [[ $line =~ $uses_comment_re ]] && continue
      [[ $line =~ $uses_ref_re ]] || continue
      [[ ${BASH_REMATCH[3]} == ./* || ${BASH_REMATCH[3]} =~ ^($trusted_action_owners)/ ]] && continue
      printf '%s:%s: %s\n' "$file" "$n" "$line"
    done <"$file"
  done
  return 0
}
untrusted=$(untrusted_uses "${workflows[@]}" "${pin_files[@]}" | sort -u)
if [[ -n $untrusted ]]; then
  printf '%s\n' "$untrusted" >&2
  fail "uses: owner must be one of: ${trusted_action_owners//|/, } (review a new owner before adding it to trusted_action_owners)"
fi

# 供应链：job 容器的镜像同样带着仓库 token 跑步骤，却不受上面 uses: 的固定与属主检查约束。
# 工具由 scripts/ci-setup-mise.sh 按 sha256 安装，不用 job 容器。
job_containers=$(grep -En '^    container:' "${workflows[@]}" || true)
if [[ -n $job_containers ]]; then
  printf '%s\n' "$job_containers" >&2
  fail 'jobs must not use a job container: its image runs with the repository token outside the uses: pin and owner checks'
fi

# mise 引导：版本与两个架构的 sha256 只在 scripts/mise-release.env 维护，CI 脚本与 Dockerfile 都读它，
# 避免两份常量漂移或退回不固定的安装脚本。
mise_release=scripts/mise-release.env
[[ -f $mise_release ]] || fail "$mise_release is missing"
grep -Eq '^MISE_BOOTSTRAP_VERSION=v[0-9]+\.[0-9]+\.[0-9]+$' "$mise_release" ||
  fail "$mise_release must pin MISE_BOOTSTRAP_VERSION=vX.Y.Z"
for var in MISE_BOOTSTRAP_SHA256_AMD64 MISE_BOOTSTRAP_SHA256_ARM64; do
  grep -Eq "^${var}=[0-9a-f]{64}( # v[0-9][^[:space:]]*)?$" "$mise_release" ||
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

  # 嵌入的 Victoria 二进制是 gzip 后 go:embed 的 payload，镜像扫描看不到；storage-bin-scan 单独扫原始二进制，
  # archive 必须依赖它（docker 经 archive 传递依赖），否则这道门禁形同虚设。
  storage_scan=$(release_job "$release_wf" storage-bin-scan)
  # run 行必须整行精确匹配：后缀 `|| true` 之类会让扫描失败也不挡发版。
  grep -Eq '^[[:space:]]+run:[[:space:]]+bash scripts/check-storage-bin-vulns\.sh[[:space:]]*$' <<<"$storage_scan" ||
    fail "$release_wf: storage-bin-scan must run exactly 'bash scripts/check-storage-bin-vulns.sh'"
  # 扫描 job 不能被吞掉失败，也不能被条件跳过：continue-on-error 会让失败的 job 对 archive 算成功。
  [[ $storage_scan != *continue-on-error* ]] ||
    fail "$release_wf: storage-bin-scan must not set continue-on-error (a failed scan would not block publishing)"
  # job 级（4 空格）和 step 级（`- if:` 或 8 空格 `if:`）的 if: 都不允许：被条件跳过的扫描 step 照样让 job 变绿。
  if grep -Eq '^[[:space:]]+(-[[:space:]]+)?if:' <<<"$storage_scan"; then
    fail "$release_wf: storage-bin-scan must not be conditional (no job- or step-level if:)"
  fi
  # archive 的 needs 必须是单行列表且包含 storage-bin-scan（精确到列表元素，不是子串）。
  archive_needs=$(grep -E '^    needs:' <<<"$archive_job" || true)
  [[ $(grep -c . <<<"$archive_needs") -eq 1 && $archive_needs == *'['*']'* ]] ||
    fail "$release_wf: archive must declare a single flow-style needs: [...] list"
  archive_needs=${archive_needs#*\[}
  archive_needs=${archive_needs%%\]*}
  [[ ",${archive_needs// /}," == *,storage-bin-scan,* ]] ||
    fail "$release_wf: archive must need storage-bin-scan so the embedded-binary scan blocks publishing"
  # Trivy 的版本与 sha256 只在 scripts/install-trivy.sh 维护，两个扫描脚本都必须经它安装，不得各自内联下载。
  for scanner in scripts/check-image-vulns.sh scripts/check-storage-bin-vulns.sh; do
    [[ -f $scanner ]] || fail "$scanner is missing"
    grep -Fq 'scripts/install-trivy.sh' "$scanner" ||
      fail "$scanner must install Trivy through scripts/install-trivy.sh"
    unpinned_trivy=$(grep -n 'releases/download' "$scanner" || true)
    [[ -z $unpinned_trivy ]] || fail "$scanner must not download Trivy itself: $unpinned_trivy"
  done
  grep -Eq '^trivy_version=[0-9]+\.[0-9]+\.[0-9]+$' scripts/install-trivy.sh ||
    fail 'scripts/install-trivy.sh must pin trivy_version=X.Y.Z'
  [[ $(grep -Ec '^[[:space:]]+sha256=[0-9a-f]{64}$' scripts/install-trivy.sh) -eq 2 ]] ||
    fail 'scripts/install-trivy.sh must pin one sha256 per architecture (amd64, arm64)'

  # 发版附带的 digest 固定 Helm values 片段由 helm-values job 上传。最小权限：docker job 会把 GITHUB_TOKEN 当
  # BuildKit secret 挂进构建，不能同时持有写仓库的权限；workflow_dispatch 不得发布任何东西。
  # 发版流程不能在本地真跑，这些静态断言是它唯一的门禁。
  docker_job=$(release_job "$release_wf" docker)
  helm_values_job=$(release_job "$release_wf" helm-values)
  [[ $docker_job == *'contents: read'* ]] ||
    fail "$release_wf: docker must stay contents: read (it mounts the token into the build)"
  [[ $docker_job != *'contents: write'* ]] ||
    fail "$release_wf: docker must not hold contents: write; attach release assets from helm-values"
  [[ $docker_job != *'gh release upload'* ]] ||
    fail "$release_wf: docker must not upload release assets; helm-values owns that step"
  [[ $helm_values_job == *"if: github.event_name == 'push'"* ]] ||
    fail "$release_wf: helm-values must only run on tag pushes (workflow_dispatch publishes nothing)"
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

# 最小发布年龄：天数只在 scripts/release-age.env 维护，renovate.json 的 minimumReleaseAge（顶层与 vulnerabilityAlerts）、
# pnpm minimumReleaseAge（分钟）、uv exclude-newer 都要与它一致。不一致会让某一侧悄悄放行更新的版本。
release_age_env=scripts/release-age.env
[[ -f $release_age_env ]] || fail "$release_age_env is missing"
min_release_age_days=$(
  MIN_RELEASE_AGE_DAYS=
  # shellcheck source=scripts/release-age.env
  . "$release_age_env"
  printf '%s' "$MIN_RELEASE_AGE_DAYS"
)
[[ $min_release_age_days =~ ^[1-9][0-9]*$ ]] || fail "$release_age_env must set MIN_RELEASE_AGE_DAYS to a positive integer"
renovate=.github/renovate.json
[[ -f $renovate ]] || fail "$renovate is missing"
# 顶层与 vulnerabilityAlerts 都必须显式写（Renovate 默认安全更新不等待）；文件里出现的每个 minimumReleaseAge 都得是这个值。
# 不用 jq：go-ci profile 没装它，文件由 JSON 缩进固定的 2 空格格式维护。
grep -Eq "^  \"minimumReleaseAge\":[[:space:]]*\"${min_release_age_days} days\",?[[:space:]]*\$" "$renovate" ||
  fail "$renovate: top-level minimumReleaseAge must be \"${min_release_age_days} days\" (MIN_RELEASE_AGE_DAYS in $release_age_env)"
awk -v want="\"minimumReleaseAge\": \"${min_release_age_days} days\"" '
  /^  "vulnerabilityAlerts":[[:space:]]*\{/ { in_block = 1; next }
  in_block && /^  \}/ { in_block = 0 }
  in_block && index($0, want) { found = 1 }
  END { exit !found }
' "$renovate" ||
  fail "$renovate: vulnerabilityAlerts.minimumReleaseAge must be \"${min_release_age_days} days\" (security updates would otherwise skip the wait)"
age_total=$(grep -Ec '^[[:space:]]*"minimumReleaseAge":' "$renovate" || true)
age_ok=$(grep -Ec "^[[:space:]]*\"minimumReleaseAge\":[[:space:]]*\"${min_release_age_days} days\",?[[:space:]]*\$" "$renovate" || true)
((age_total == age_ok)) ||
  fail "$renovate: every minimumReleaseAge must be \"${min_release_age_days} days\" (MIN_RELEASE_AGE_DAYS in $release_age_env)"
# 年龄闸要靠 internalChecksFilter: strict 才生效：flexible/none 会在所有新版本都不满 7 天时照样开 PR（安全更新也一样）。
filter_total=$(grep -Ec '^[[:space:]]*"internalChecksFilter":' "$renovate" || true)
filter_strict=$(grep -Ec '^[[:space:]]*"internalChecksFilter":[[:space:]]*"strict",?[[:space:]]*$' "$renovate" || true)
grep -Eq '^  "internalChecksFilter":[[:space:]]*"strict",?[[:space:]]*$' "$renovate" && ((filter_total == filter_strict)) ||
  fail "$renovate: internalChecksFilter must be \"strict\" at the top level and wherever it appears (otherwise minimumReleaseAge does not hold PRs back)"
while IFS= read -r lock; do
  workspace=${lock%pnpm-lock.yaml}pnpm-workspace.yaml
  [[ -f $workspace ]] || fail "$workspace is missing: pnpm projects must set minimumReleaseAge there"
  grep -Eq "^minimumReleaseAge:[[:space:]]*$((min_release_age_days * 1440))[[:space:]]*$" "$workspace" ||
    fail "$workspace must set minimumReleaseAge: $((min_release_age_days * 1440)) (MIN_RELEASE_AGE_DAYS=$min_release_age_days x 1440 minutes)"
done < <(git ls-files --cached --others --exclude-standard -- '*pnpm-lock.yaml')

# Python 侧同一个最小发布年龄：各 pyproject.toml 的 [tool.uv] exclude-newer 必须是 scripts/release-age.env 的「N days」。
# uv pip install、uv build 的构建后端解析都读它；缺了它 Python 依赖就不受最小发布年龄约束。
while IFS= read -r manifest; do
  grep -Eq '^\[tool\.uv\][[:space:]]*$' "$manifest" ||
    fail "$manifest must have a [tool.uv] table with exclude-newer = \"$min_release_age_days days\" (MIN_RELEASE_AGE_DAYS=$min_release_age_days)"
  grep -Eq "^exclude-newer[[:space:]]*=[[:space:]]*\"$min_release_age_days days\"[[:space:]]*(#.*)?$" "$manifest" ||
    fail "$manifest must set exclude-newer = \"$min_release_age_days days\" under [tool.uv] (MIN_RELEASE_AGE_DAYS=$min_release_age_days)"
done < <(git ls-files --cached --others --exclude-standard -- '*pyproject.toml')

# ci-ok 汇总：被跳过的 job 在 ci-ok 里算通过，所以 ci-ok 必须 if: always()，且 needs 覆盖 ci.yml 里所有其它 job，
# 否则某个 job 失败不会挡住合并。
ci=.github/workflows/ci.yml
[[ -f $ci ]] || fail "$ci is missing"
mapfile -t ci_jobs < <(awk '
  /^jobs:[[:space:]]*$/ { in_jobs = 1; next }
  /^[^[:space:]#]/ { in_jobs = 0 }
  in_jobs && /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { name = $1; sub(/:$/, "", name); print name }
' "$ci")
ci_ok=$(release_job "$ci" ci-ok)
[[ -n $ci_ok ]] || fail "$ci: job ci-ok is missing"
[[ $ci_ok == *'if: always()'* ]] || fail "$ci: ci-ok must run with if: always()"
mapfile -t ci_ok_needs < <(sed -n '/^    needs:$/,/^    [a-z-]*:/p' <<<"$ci_ok" |
  sed -n 's/^      - \([A-Za-z0-9_-]*\)$/\1/p')
# 先收齐再匹配：pipefail 下 printf | grep -q 会在 grep 提前退出时让 printf 收到 SIGPIPE（141），门禁偶发变红。
ci_ok_needs_list=$(printf '%s\n' "${ci_ok_needs[@]}")
for job in "${ci_jobs[@]}"; do
  [[ $job == ci-ok ]] && continue
  [[ $'\n'$ci_ok_needs_list$'\n' == *$'\n'"$job"$'\n'* ]] || fail "$ci: ci-ok must need job $job"
done

# continue-on-error 能让 ci-ok 对失败的 job 视而不见，所以 ci.yml 里只许 sdk-python 矩阵这一处用，
# 且只能写成 `${{ matrix.experimental }}`，矩阵里只有 3.16 预发布一格可以是 experimental: true（ADR 0014）。
mapfile -t ci_continue_on_error < <(grep -nE '^[[:space:]]*continue-on-error:' "$ci" || true)
((${#ci_continue_on_error[@]} == 1)) ||
  fail "$ci: continue-on-error may appear exactly once (the sdk-python matrix job), found ${#ci_continue_on_error[@]}"
sdk_python_block=$(release_job "$ci" sdk-python)
[[ -n $sdk_python_block ]] || fail "$ci: job sdk-python is missing"
[[ $sdk_python_block == *'    continue-on-error: ${{ matrix.experimental }}'* ]] ||
  fail "$ci: sdk-python must set continue-on-error: \${{ matrix.experimental }} (the only continue-on-error in ci.yml)"
sdk_python_experimental=$(awk '
  /^[[:space:]]+- python:/ { py = $3; gsub(/"/, "", py); next }
  /^[[:space:]]+experimental:/ && py != "" { if ($2 == "true") print py; py = "" }
' <<<"$sdk_python_block" | tr '\n' ' ')
[[ $sdk_python_experimental == '3.16 ' ]] ||
  fail "$ci: only the 3.16 pre-release may be experimental: true in the sdk-python matrix, got: ${sdk_python_experimental:-none}"

# changes 作业输出的每个标志都要在 ci.yml 里声明为 job output：漏声明时 `needs.changes.outputs.<flag> == 'true'` 恒为假，对应 job 被悄悄跳过。
for flag in $(bash scripts/ci-changes.sh --all 2>/dev/null | sed -n 's/=.*//p'); do
  grep -Eq "^      ${flag}: \\$\\{\\{ steps\\.detect\\.outputs\\.${flag} \\}\\}$" "$ci" ||
    fail "$ci: changes job does not expose output $flag"
done

# 显式关闭 actionlint 的 shellcheck 集成：runner 与开发机上的 shellcheck 版本不一
#（甚至是坏掉的 mise shim），结果不确定。shell 片段的 lint
# 要做就把 shellcheck 钉进 mise.toml 再打开。
actionlint -shellcheck= "${workflows[@]}"
