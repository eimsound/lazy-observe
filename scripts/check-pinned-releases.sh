#!/usr/bin/env bash
set -euo pipefail

# 手工固定点的「最小发布年龄」门禁与新版本报告。
#
# Dependabot 只认 go.mod、pnpm-lock.yaml、Dockerfile 等清单，看不到下面这些手工固定在脚本里的 GitHub release
# 版本；它们既不会被投毒防线（cooldown）覆盖，也没人提醒升级（Victoria 就是这样落后了好几个版本）。
# 这个脚本从固定点所在文件里直接解析版本号，到 GitHub API 查对应 release 的发布时间：
#
#   --gate    门禁：任何固定点引用的 release 发布不满 MIN_RELEASE_AGE_DAYS 天、或是 prerelease/draft 就失败，
#             防止有人把刚发布、可能被投毒的版本钉进来。只查被固定的那几个 release，调用量小。
#   --report  报告（默认）：列出每个固定点的当前版本，以及「已满 N 天的最新稳定版」；落后的标 outdated 并给出升级目标。
#             有 outdated 或 too-new 时退出 1，供每周定时 workflow 变红提醒。
#             --markdown FILE 额外把报告写成 Markdown（定时 workflow 用它同步 issue）。
#   --latest-eligible OWNER/REPO
#             只在 stdout 打印该仓库「已满 N 天的最新稳定版」的 tag，供别的脚本（如镜像扫描门禁判断「pin 是否已是
#             最新合格版」）机读。规则与报告完全相同：严格 vX.Y.Z、排除 draft/prerelease、按 published_at 截止、
#             semver 取最大。不读固定点文件。没有合格 release 时 stdout 为空、退出 1；API 失败退出 2。
#
# 退出码：0 通过；1 有违规（gate: 太新/prerelease；report: outdated/too-new；latest-eligible: 没有合格 release）；
# 2 脚本自身出错（解析不到固定点、网络失败、API 限流或拒绝访问、缺命令）。出错一律失败，不会静默放行。
#
# 固定点（GitHub release 型），每项都要知道「仓库 + tag 前缀」，tag 必须严格匹配 <前缀>X.Y.Z：
#   - Victoria 三个仓库：scripts/download-storage-bins.sh 的 metrics_version / logs_version / traces_version；
#     VictoriaMetrics 仓库里还有 -cluster、-enterprise 以及 LTS 线（v1.136.x）的 tag，严格匹配会把带后缀的排除，
#     「最新」按 semver 取最大而不是按发布时间，LTS 补丁发布得晚也不会被当成最新；
#   - e2e 集群镜像 scripts/e2e-multi-replica.sh 的 vmstorage/vminsert/vmselect：tag 形如 v1.147.0-cluster-scratch，
#     对应 VictoriaMetrics 的 v1.147.0 release。只校验 tag 对应 release 的年龄，不校验 sha256 digest 是否真属于该 tag；
#   - mise：scripts/mise-release.env 的 MISE_BOOTSTRAP_VERSION（jdx/mise）；
#   - Trivy：aquasecurity/trivy。版本号在 scripts/install-trivy.sh（拆出安装脚本之后）或
#     scripts/check-image-vulns.sh（拆分之前）的 trivy_version=，取第一个含这个变量的文件，不用改本脚本。
#
# 暂不检查（这些固定点没有「tag -> 发布时间」的 GitHub release 可查，要走容器仓库 API，留作后续）：
#   workflow 里 BuildKit 镜像的 digest、e2e 的 SeaweedFS/Valkey/Postgres 镜像、mise.toml 的工具版本、
#   scripts/ci-setup-mise.sh 的工具清单、entc.mod/entc.sum。
#
# 认证：GH_TOKEN 或 GITHUB_TOKEN；都没有时匿名访问（60 次/小时，可能被限流），会在 stderr 提示。
# 带认证的请求返回 403 且不是限流（响应体 message 不含 rate limit，也没有 x-ratelimit-remaining: 0）时，
# 说明 token 本身被拒：有的组织（如 aquasecurity）开了 IP allow list，会拦截 Actions 的 GITHUB_TOKEN。
# 单个 release 的发布时间按下面的顺序取，每一步失败才走下一步，全部失败退出 2（fail-closed）：
#   1. 带认证的 API（repos/OWNER/REPO/releases/tags/TAG 的 published_at）；
#   2. 仅当 1 是「非限流的 403」：github.com 的 release 网页 https://github.com/OWNER/REPO/releases/tag/TAG。
#      取 "released this <relative-time datetime=...>" 的时间，页面带 Pre-release 标签算 prerelease。
#      它不走 API，不受 token 的 IP allow list 与 API 限流影响（Actions 上是否受影响要看 PR CI）。
#      实测 aquasecurity/trivy、VictoriaMetrics 三个仓库、jdx/mise 共 31 个 release：这个时间与 API 的 published_at
#      逐个相同（0 秒差）。别的来源不能用：release 资产的 last-modified 是上传时间，VictoriaMetrics 最多早近 3 天
#      published_at（会把不满 7 天的版本当成已满）；releases.atom 的 updated 是最近一次编辑时间，
#      只含最近约 10 条，且与 published_at 不稳定地一致；
#   3. 匿名 API：最后的备选。GitHub 托管 runner 的出口 IP 被很多人共享，匿名额度（每 IP 每小时 60 次）常被别人用光，
#      所以不可依赖，只在 2 失败时多试一次，不改变任何判定。
# 限流（429，或 403 且命中上述限流特征）不走 2、3，直接退出 2 并写明限流。每次失败都会打印响应体的 message。
#
# release 列表（报告模式的「最新合格版」要用）没有网页等价物，被拒绝的仓库降级：
#   - 报告：取 https://github.com/OWNER/REPO/releases/latest 重定向到的 tag（GitHub 标记的 Latest），再按 2 取它的发布时间。
#     tag 严格匹配 <前缀>X.Y.Z 且已满 N 天：它就是「最新」；固定点等于它为 ok，低于它为 outdated。
#     不满 N 天、不是严格 tag、或比固定点还旧：无法知道「已满 N 天的最新版」，该项标 unknown（不算 outdated，
#     不让每周 job 误报红；固定点本身太新、是 prerelease 仍然照常算 too-new/unstable）。
#   - --latest-eligible：被拒绝的仓库直接退出 2。GitHub 的 Latest 不一定是 semver 最大（VictoriaMetrics 有 LTS 补丁线），
#     而调用方（check-storage-bin-vulns.sh）会据此把「pin >= 最新合格版」的命中降为警告，答错就是放行，所以不降级。
#
# 测试钩子（scripts/check-pinned-releases-test.sh 用，离线）：
#   LO_PINNED_RELEASES_ROOT     固定点文件所在的根目录，默认仓库根；
#   LO_PINNED_RELEASES_NOW      当前时间（epoch 秒），默认 date +%s；
#   LO_PINNED_RELEASES_FIXTURE  假 API 目录：<owner>__<repo>/releases.json 与 <owner>__<repo>/tags/<tag>.json。
#                               想让某个请求失败，在对应文件旁放 <文件>.auth-error（带认证的请求）或
#                               <文件>.anon-error（匿名请求），内容是一个迷你 HTTP 响应：第一行状态码，
#                               随后若干响应头行，空行，再是响应体；
#                               假 github.com 网页：<owner>__<repo>/web/tags/<tag>.html 是 release 页面，
#                               <owner>__<repo>/web/latest 是 releases/latest 的迷你 HTTP 响应（302 + location），
#                               想让网页请求失败，在对应文件旁放 <文件>.web-error（同样的迷你 HTTP 响应）；
#   LO_PINNED_RELEASES_FIXTURE_LOG  若设置，每个假请求追加一行 "auth|anon|web <路径>"，测试用它断言走了哪些来源。

script_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root=${LO_PINNED_RELEASES_ROOT:-$(dirname "$script_dir")}
fixture=${LO_PINNED_RELEASES_FIXTURE:-}

# shellcheck source=scripts/release-age.env
. "$script_dir/release-age.env"
min_days=${MIN_RELEASE_AGE_DAYS:?scripts/release-age.env must set MIN_RELEASE_AGE_DAYS}
[[ $min_days =~ ^[1-9][0-9]*$ ]] || {
  printf 'MIN_RELEASE_AGE_DAYS must be a positive integer, got: %s\n' "$min_days" >&2
  exit 2
}
min_seconds=$((min_days * 86400))
now=${LO_PINNED_RELEASES_NOW:-$(date +%s)}

die() {
  printf 'pinned releases: %s\n' "$*" >&2
  exit 2
}

usage() {
  printf 'usage: check-pinned-releases.sh [--report [--markdown FILE] | --gate | --latest-eligible OWNER/REPO]\n' >&2
  exit 2
}

mode=report
mode_given=
markdown=
latest_repo=
# 三种模式互斥：同时给出不同的模式没有合理的含义，直接报 usage。
set_mode() {
  [[ -z $mode_given || $mode_given == "$1" ]] || usage
  mode_given=$1
  mode=$1
}
while (($# > 0)); do
  case $1 in
  --gate) set_mode gate ;;
  --report) set_mode report ;;
  --markdown)
    (($# >= 2)) || usage
    markdown=$2
    shift
    ;;
  --latest-eligible)
    (($# >= 2)) || usage
    set_mode latest
    latest_repo=$2
    shift
    ;;
  *) usage ;;
  esac
  shift
done
if [[ -n $markdown && $mode != report ]]; then
  usage
fi
if [[ $mode == latest ]] && ! [[ $latest_repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  printf 'pinned releases: --latest-eligible needs OWNER/REPO, got: %s\n' "$latest_repo" >&2
  exit 2
fi

for command in curl jq date sort mktemp; do
  command -v "$command" >/dev/null 2>&1 || die "missing required command: $command"
done

# ---------------------------------------------------------------------------
# 解析固定点
# ---------------------------------------------------------------------------

# 每个固定点一行，字段用 TAB 分隔：名称、仓库、tag 前缀（正则片段）、当前 tag、来源（文件:变量）。
pins=()

add_pin() {
  local name=$1 repo=$2 prefix=$3 tag=$4 source=$5
  pins+=("$name"$'\t'"$repo"$'\t'"$prefix"$'\t'"$tag"$'\t'"$source")
}

# assigned_value FILE NAME：取 FILE 里唯一一行 NAME=VALUE（允许行首空白与行尾注释）的 VALUE。
# 恰好一行才算数：0 行说明格式漂移，多行说明有歧义；两者都不能当作「没有固定点」放行。
assigned_value() {
  local file=$1 name=$2 line count=0 value=
  local re="^[[:space:]]*${name}=([^[:space:]#]+)[[:space:]]*(#.*)?\$"
  while IFS= read -r line || [[ -n $line ]]; do
    if [[ $line =~ $re ]]; then
      value=${BASH_REMATCH[1]}
      count=$((count + 1))
    fi
  done <"$file"
  ((count == 1)) || return 1
  printf '%s\n' "$value"
}

require_file() {
  [[ -f $root/$1 ]] || die "pinned file is missing: $1"
}

collect_pins() {
  storage=scripts/download-storage-bins.sh
  require_file "$storage"
  for entry in 'metrics_version:VictoriaMetrics:VictoriaMetrics' 'logs_version:VictoriaLogs:VictoriaLogs' \
    'traces_version:VictoriaTraces:VictoriaTraces'; do
    IFS=: read -r var label repo <<<"$entry"
    value=$(assigned_value "$root/$storage" "$var") || die "$storage must contain exactly one $var=vX.Y.Z line"
    [[ $value =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "$storage: $var=$value is not vX.Y.Z"
    add_pin "$label" "VictoriaMetrics/$repo" v "$value" "$storage:$var"
  done

  e2e=scripts/e2e-multi-replica.sh
  require_file "$e2e"
  e2e_count=0
  vm_image_re='quay\.io/victoriametrics/(vmstorage|vminsert|vmselect):(v[0-9]+\.[0-9]+\.[0-9]+)-cluster-scratch@sha256:[0-9a-f]{64}'
  while IFS= read -r line || [[ -n $line ]]; do
    if [[ $line =~ $vm_image_re ]]; then
      add_pin "VictoriaMetrics ${BASH_REMATCH[1]} (e2e image)" VictoriaMetrics/VictoriaMetrics v \
        "${BASH_REMATCH[2]}" "$e2e:${BASH_REMATCH[1]}"
      e2e_count=$((e2e_count + 1))
    fi
  done <"$root/$e2e"
  ((e2e_count == 3)) || die "$e2e must pin exactly 3 victoriametrics cluster images (vmstorage, vminsert, vmselect), found $e2e_count"

  mise_env=scripts/mise-release.env
  require_file "$mise_env"
  value=$(assigned_value "$root/$mise_env" MISE_BOOTSTRAP_VERSION) ||
    die "$mise_env must contain exactly one MISE_BOOTSTRAP_VERSION=vX.Y.Z line"
  [[ $value =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "$mise_env: MISE_BOOTSTRAP_VERSION=$value is not vX.Y.Z"
  add_pin mise jdx/mise v "$value" "$mise_env:MISE_BOOTSTRAP_VERSION"

  trivy_source=
  for candidate in scripts/install-trivy.sh scripts/check-image-vulns.sh; do
    [[ -f $root/$candidate ]] || continue
    for var in trivy_version TRIVY_VERSION; do
      if value=$(assigned_value "$root/$candidate" "$var"); then
        [[ $value =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "$candidate: $var=$value is not X.Y.Z"
        add_pin Trivy aquasecurity/trivy v "v${value#v}" "$candidate:$var"
        trivy_source=$candidate
        break 2
      fi
    done
  done
  [[ -n $trivy_source ]] || die 'no trivy_version= line found in scripts/install-trivy.sh or scripts/check-image-vulns.sh'
}

# ---------------------------------------------------------------------------
# GitHub API
# ---------------------------------------------------------------------------

token=${GH_TOKEN:-${GITHUB_TOKEN:-}}
if [[ -z $fixture && -z $token ]]; then
  printf 'pinned releases: no GH_TOKEN/GITHUB_TOKEN set, using anonymous GitHub API access (60 requests/hour); ' >&2
  printf 'a rate-limit failure here is not a pin problem, rerun with a token\n' >&2
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# fixture_file PATH：假 API 里 PATH 对应的文件。
#   repos/<owner>/<repo>/releases → <owner>__<repo>/releases.json
#   repos/<owner>/<repo>/releases/tags/<tag> → <owner>__<repo>/tags/<tag>.json
fixture_file() {
  local rel=${1#repos/} owner repo_name tail
  rel=${rel%%\?*}
  owner=${rel%%/*}
  rel=${rel#*/}
  repo_name=${rel%%/*}
  tail=${rel#*/}
  if [[ $tail == releases ]]; then
    printf '%s\n' "$fixture/${owner}__${repo_name}/releases.json"
  else
    printf '%s\n' "$fixture/${owner}__${repo_name}/tags/${tail#releases/tags/}.json"
  fi
}

# fixture_web_file PATH：假 github.com 网页里 PATH（<owner>/<repo>/releases/...）对应的文件。
#   <owner>/<repo>/releases/tag/<tag> → <owner>__<repo>/web/tags/<tag>.html
#   <owner>/<repo>/releases/latest → <owner>__<repo>/web/latest
fixture_web_file() {
  local owner=${1%%/*} rest repo_name tail
  rest=${1#*/}
  repo_name=${rest%%/*}
  tail=${rest#*/}
  if [[ $tail == releases/latest ]]; then
    printf '%s\n' "$fixture/${owner}__${repo_name}/web/latest"
  else
    printf '%s\n' "$fixture/${owner}__${repo_name}/web/tags/${tail#releases/tag/}.html"
  fi
}

# fixture_response FILE OUT HDR：把迷你 HTTP 响应（状态码、响应头、空行、响应体）拆进 HDR 与 OUT，stdout 打印状态码。
fixture_response() {
  local file=$1 out=$2 hdr=$3 code line
  {
    IFS= read -r code
    while IFS= read -r line; do
      line=${line%$'\r'}
      [[ -n $line ]] || break
      printf '%s\n' "$line" >>"$hdr"
    done
    cat >"$out"
  } <"$file"
  printf '%s\n' "$code"
}

# http_get PATH AUTH OUT HDR：GET 一次，响应体写进 OUT、响应头写进 HDR，stdout 打印 HTTP 状态码。
# AUTH=1 带 Authorization，0 匿名。网络层失败（连不上、超时）直接退出 2。
http_get() {
  local path=$1 auth=$2 out=$3 hdr=$4 code
  if [[ -n $fixture ]]; then
    local file kind
    file=$(fixture_file "$path")
    kind=anon
    [[ $auth == 1 ]] && kind=auth
    [[ -z ${LO_PINNED_RELEASES_FIXTURE_LOG:-} ]] || printf '%s %s\n' "$kind" "$path" >>"$LO_PINNED_RELEASES_FIXTURE_LOG"
    : >"$hdr"
    : >"$out"
    if [[ -f $file.$kind-error ]]; then
      fixture_response "$file.$kind-error" "$out" "$hdr"
    elif [[ -f $file ]]; then
      cp "$file" "$out"
      printf '200\n'
    else
      printf '{"message":"Not Found"}\n' >"$out"
      printf '404\n'
    fi
    return 0
  fi
  local args=(--proto '=https' --tlsv1.2 -sS --retry 3 --retry-all-errors --connect-timeout 10 --max-time 60
    -H 'Accept: application/vnd.github+json' -H 'X-GitHub-Api-Version: 2022-11-28'
    -D "$hdr" -o "$out" -w '%{http_code}')
  [[ $auth == 1 ]] && args+=(-H "Authorization: Bearer $token")
  code=$(curl "${args[@]}" "https://api.github.com/$path") ||
    die "GitHub API GET $path failed: network error (curl exit $?)"
  printf '%s\n' "$code"
}

# web_get PATH OUT HDR [follow]：GET https://github.com/PATH（网页，不带认证、不是 API），写法同 http_get。
# follow=0 不跟随重定向（取 releases/latest 的 location 用）。网络层失败不退出：打印 000，调用方按失败处理，
# 因为网页只是备选来源，它失败后还有匿名 API 可试。
web_get() {
  local path=$1 out=$2 hdr=$3 follow=${4:-1} code
  if [[ -n $fixture ]]; then
    local file
    file=$(fixture_web_file "$path")
    [[ -z ${LO_PINNED_RELEASES_FIXTURE_LOG:-} ]] || printf 'web %s\n' "$path" >>"$LO_PINNED_RELEASES_FIXTURE_LOG"
    : >"$hdr"
    : >"$out"
    if [[ -f $file.web-error ]]; then
      fixture_response "$file.web-error" "$out" "$hdr"
    elif [[ -f $file ]]; then
      if [[ $file == */web/latest ]]; then
        fixture_response "$file" "$out" "$hdr"
      else
        cp "$file" "$out"
        printf '200\n'
      fi
    else
      printf 'Not Found\n' >"$out"
      printf '404\n'
    fi
    return 0
  fi
  local args=(--proto '=https' --tlsv1.2 -sS --retry 3 --retry-all-errors --connect-timeout 10 --max-time 60
    -H 'Accept-Language: en-US,en;q=0.9' -D "$hdr" -o "$out" -w '%{http_code}')
  [[ $follow == 1 ]] && args+=(-L --max-redirs 5 --proto-redir '=https')
  code=$(curl "${args[@]}" "https://github.com/$path") || code=000
  printf '%s\n' "$code"
}

# header_value HDR NAME：最后一段响应头里 NAME（小写）的值，没有就是空串。
header_value() {
  local file=$1 name=$2 line key value=
  while IFS= read -r line || [[ -n $line ]]; do
    line=${line%$'\r'}
    # curl --retry 把每次响应的头都累积在同一个 -D 文件里；只认最后一段，更早的 503 带的 retry-after 之类不能算数。
    if [[ $line == HTTP/* ]]; then
      value=
      continue
    fi
    [[ $line == *:* ]] || continue
    key=${line%%:*}
    key=${key,,}
    if [[ $key == "$name" ]]; then
      value=${line#*:}
      value=${value#"${value%%[![:space:]]*}"}
    fi
  done <"$file"
  printf '%s\n' "$value"
}

# error_message BODY：失败响应的说明。优先取 JSON 的 .message；响应体不是 JSON（或没有 message）时
# 退回响应体前 200 个字符，不让 jq 的解析失败把真正的错误吞掉。
error_message() {
  local file=$1 msg=
  msg=$(jq -r '.message // empty' "$file" 2>/dev/null) || msg=
  if [[ -z $msg ]]; then
    msg=$(tr '\r\n' '  ' <"$file")
    msg=${msg:0:200}
  fi
  printf '%s\n' "${msg:-<empty response body>}"
}

# is_rate_limited CODE HDR MSG：限流特征：429、x-ratelimit-remaining: 0、retry-after、或 message 提到 rate limit
# （含 secondary rate limit）。
is_rate_limited() {
  local code=$1 hdr=$2 msg=$3
  [[ $code == 429 ]] && return 0
  [[ $(header_value "$hdr" x-ratelimit-remaining) == 0 ]] && return 0
  [[ -n $(header_value "$hdr" retry-after) ]] && return 0
  [[ ${msg,,} == *'rate limit'* ]] && return 0
  return 1
}

# rate_limit_hint HDR：限流时的提示，带上重置时间（若响应头里有）。
rate_limit_hint() {
  local reset
  reset=$(header_value "$1" x-ratelimit-reset)
  if [[ $reset =~ ^[0-9]+$ ]]; then
    printf ' (rate limited; the limit resets at %s)' "$(date -u -d "@$reset" +%Y-%m-%dT%H:%M:%SZ)"
  else
    printf ' (rate limited; wait and rerun, or use a token with a higher limit)'
  fi
}

# 带认证的 403 且不是限流（token 被组织的 IP allow list 之类拒绝）：api_get 不自己重试，返回 3，
# 响应体的 message 放进 denied_msg，由调用方决定走哪个备选来源（单个 release 走网页，列表没有备选）。
denied_msg=
# 已经被拒绝过的仓库：同一仓库后面的认证请求同样会被拒，直接跳过，不再多打一个必败的请求。
# release_json/releases_json 总是在 $(...) 子 shell 里跑，变量带不出来，所以记在 $work 下的文件里（内容是 message）。
mark_denied() { printf '%s\n' "$2" >"$work/denied-${1//\//__}"; }
denied_msg_of() { # 没被拒绝过返回 1
  [[ -f $work/denied-${1//\//__} ]] || return 1
  denied_msg=$(<"$work/denied-${1//\//__}")
}

# api_get PATH OUT：把 JSON 写进 OUT，返回 0。失败时带真实 HTTP 状态码和响应体的 message 退出，不返回空结果；
# 唯一的例外是上面说的「带认证的非限流 403」：返回 3。
api_get() {
  local path=$1 out=$2 hdr=$work/headers auth=0 code msg hint=
  [[ -n $token ]] && auth=1
  code=$(http_get "$path" "$auth" "$out" "$hdr") || return 2
  [[ $code == 200 ]] && return 0
  msg=$(error_message "$out")
  if [[ $code == 403 && $auth == 1 ]] && ! is_rate_limited "$code" "$hdr" "$msg"; then
    denied_msg=$msg
    return 3
  fi
  if is_rate_limited "$code" "$hdr" "$msg"; then
    hint=$(rate_limit_hint "$hdr")
  elif [[ $code == 404 ]]; then
    hint=' (release or repository not found)'
  elif [[ $code == 403 ]]; then
    hint=' (access denied, not a rate limit)'
  fi
  die "GitHub API GET $path failed: HTTP $code ($msg)$hint"
}

# anon_get PATH OUT TRAIL：最后的备选，匿名 GET 一次（见文件头「认证」第 3 步）。成功返回 0；失败退出 2，
# 错误里带上前面每一步失败的经过 TRAIL。
anon_get() {
  local path=$1 out=$2 trail=$3 hdr=$work/headers code msg hint=
  code=$(http_get "$path" 0 "$out" "$hdr") || return 2
  [[ $code == 200 ]] && return 0
  msg=$(error_message "$out")
  if is_rate_limited "$code" "$hdr" "$msg"; then hint=$(rate_limit_hint "$hdr"); fi
  die "GitHub API GET $path failed: $trail, then anonymous HTTP $code ($msg)$hint"
}

# web_release_json REPO TAG OUT：从 github.com 的 release 页面取发布时间，写成和 API 同形的最小 JSON
# （tag_name、published_at、draft、prerelease、source）。成功返回 0；失败返回 1，原因放进 web_err。
# 页面上「released this」后面紧跟的 <relative-time datetime="YYYY-MM-DDTHH:MM:SSZ"> 是发布时间。
# 页面没有这个结构（改版、或这个 tag 没有 release 只有 git tag）、HTTP 非 200，都算失败，不猜。
# draft 对匿名访问是 404，所以走到这里的页面一定不是 draft。
web_err=
web_release_json() {
  local repo=$1 tag=$2 out=$3 hdr=$work/web-headers body=$work/web-body code html published prerelease=false
  local time_re='released this[[:space:]]*<relative-time[^>]*[[:space:]]datetime="([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z)"'
  local pre_re='class="Label[^"]*"[^>]*>[[:space:]]*Pre-release[[:space:]]*</span>'
  code=$(web_get "$repo/releases/tag/$tag" "$body" "$hdr")
  if [[ $code != 200 ]]; then
    web_err="HTTP $code"
    [[ $code == 404 ]] && web_err+=' (release page not found)'
    [[ $code == 000 ]] && web_err='network error'
    return 1
  fi
  html=$(<"$body")
  if ! [[ $html =~ $time_re ]]; then
    web_err='the page has no "released this <relative-time datetime=...>" (layout changed, or the tag has no release)'
    return 1
  fi
  published=${BASH_REMATCH[1]}
  [[ $html =~ $pre_re ]] && prerelease=true
  jq -n --arg tag "$tag" --arg published "$published" --argjson pre "$prerelease" \
    '{tag_name: $tag, published_at: $published, prerelease: $pre, draft: false, source: "github.com release page"}' >"$out"
}

# release_json REPO TAG：单个 release（缓存）。来源顺序见文件头「认证」。
release_json() {
  local repo=$1 tag=$2 cache path rc=0 trail skipped=
  cache=$work/rel-${repo//\//__}-$tag.json
  path=repos/$repo/releases/tags/$tag
  if [[ ! -f $cache ]]; then
    if denied_msg_of "$repo"; then
      rc=3
      skipped=1
    else
      api_get "$path" "$cache" || rc=$?
    fi
    case $rc in
    0) ;;
    3)
      mark_denied "$repo" "$denied_msg"
      if [[ -n $skipped ]]; then
        printf 'pinned releases: GET %s: skipped, an earlier authenticated request to %s got HTTP 403 (%s), not a rate limit; ' \
          "$path" "$repo" "$denied_msg" >&2
      else
        printf 'pinned releases: GET %s: the authenticated request got HTTP 403 (%s), which is not a rate limit; ' \
          "$path" "$denied_msg" >&2
      fi
      printf 'reading the publish time from https://github.com/%s/releases/tag/%s instead\n' "$repo" "$tag" >&2
      if ! web_release_json "$repo" "$tag" "$cache"; then
        trail="authenticated HTTP 403 ($denied_msg), then github.com release page failed ($web_err)"
        printf 'pinned releases: the github.com release page of %s %s failed (%s); trying the anonymous API as a last resort\n' \
          "$repo" "$tag" "$web_err" >&2
        anon_get "$path" "$cache" "$trail" || return 2
      fi
      ;;
    *) return 2 ;;
    esac
  fi
  printf '%s\n' "$cache"
}

# releases_json REPO：最新的最多 300 条 release（缓存），拼成一个 JSON 数组。
# 没有网页等价物：带认证被拒（api_get 返回 3）时返回 3，调用方降级或失败。
releases_json() {
  local repo=$1 cache page rc=0
  cache=$work/list-${repo//\//__}.json
  if [[ ! -f $cache ]]; then
    ! denied_msg_of "$repo" || return 3
    if [[ -n $fixture ]]; then
      api_get "repos/$repo/releases" "$cache" || rc=$?
    else
      local pages=()
      for page in 1 2 3; do
        api_get "repos/$repo/releases?per_page=100&page=$page" "$work/page-$page.json" || { rc=$?; break; }
        pages+=("$work/page-$page.json")
        [[ $(jq 'length' "$work/page-$page.json") -ge 100 ]] || break
      done
      ((rc != 0)) || jq -s 'add' "${pages[@]}" >"$cache"
    fi
    if ((rc == 3)); then
      mark_denied "$repo" "$denied_msg"
      return 3
    fi
    ((rc == 0)) || return 2
  fi
  printf '%s\n' "$cache"
}

# ---------------------------------------------------------------------------
# 判定
# ---------------------------------------------------------------------------

findings=0
rows=()      # 报告行（TSV）
notes=()     # 报告里需要说明的降级（unknown）

fmt_time() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }
fmt_duration() { # 秒 -> "Xd Yh"
  local s=$1
  printf '%dd %dh' $((s / 86400)) $((s % 86400 / 3600))
}
fmt_days() { # 秒 -> 一位小数的天数
  printf '%d.%d' $(($1 / 86400)) $(($1 % 86400 * 10 / 86400))
}

# 取 jq 过滤出的单个字段；字段缺失（null）说明 API 返回异常，直接报错。
release_field() {
  local file=$1 filter=$2 value
  value=$(jq -r "$filter // empty" "$file")
  [[ -n $value ]] || die "unexpected GitHub API payload in $file: $filter is empty"
  printf '%s\n' "$value"
}

# 当前固定点自己的发布信息。
check_pin() {
  local name repo prefix tag source file published draft prerelease published_at age
  IFS=$'\t' read -r name repo prefix tag source <<<"$1"
  file=$(release_json "$repo" "$tag") || exit 2
  published=$(release_field "$file" '.published_at') || exit 2
  draft=$(jq -r '.draft' "$file") || die "unexpected GitHub API payload in $file"
  prerelease=$(jq -r '.prerelease' "$file") || die "unexpected GitHub API payload in $file"
  published_at=$(jq -rn --arg t "$published" '$t | fromdateiso8601') || die "invalid published_at: $published"
  age=$((now - published_at))

  status=ok
  detail=
  if [[ $draft == true || $prerelease == true ]]; then
    status=unstable
    detail="$repo $tag is a $([[ $draft == true ]] && echo draft || echo prerelease) release"
  elif ((age < min_seconds)); then
    status=too-new
    local eligible=$((published_at + min_seconds))
    detail="$repo $tag was published $published ($(fmt_days "$age") days ago), the minimum release age is $min_days days: "
    detail+="wait until $(fmt_time "$eligible") ($(fmt_duration $((eligible - now))) from now), or pin an older release"
  fi
  cur_published=$published
  cur_age=$age
  cur_source=$(jq -r '.source // empty' "$file")
}

# 已满 N 天的最新稳定版：严格匹配 <前缀>X.Y.Z，排除 draft/prerelease，按 semver 取最大。
latest_eligible() {
  local repo=$1 prefix=$2 list
  list=$(releases_json "$repo") || return $?
  jq -r --arg prefix "$prefix" --argjson cutoff $((now - min_seconds)) '
    [ .[]
      | select(.draft == false and .prerelease == false and .published_at != null)
      | select((.published_at | fromdateiso8601) <= $cutoff)
      | (.tag_name | capture("^" + $prefix + "(?<a>[0-9]+)\\.(?<b>[0-9]+)\\.(?<c>[0-9]+)$")?) as $v
      | select($v != null)
      | {tag: .tag_name, key: [$v.a, $v.b, $v.c | tonumber]} ]
    | if length == 0 then "" else (max_by(.key) | .tag) end' "$list"
}

# web_latest REPO PREFIX：被拒绝的仓库的降级（见文件头「release 列表」）。结果放进全局：
#   web_latest_tag   GitHub 标记的 Latest 的 tag，已满 N 天、严格匹配 <前缀>X.Y.Z、不是 prerelease 才有值，否则为空；
#   web_latest_note  为空时的原因。
# releases/latest 或这个 tag 的页面取不到是脚本出错，退出 2（不当成 unknown 放过）。
web_latest_tag=
web_latest_note=
web_latest() {
  local repo=$1 prefix=$2 hdr=$work/web-headers body=$work/web-body code location tag file published prerelease published_at
  local re="/releases/tag/(${prefix}[0-9]+\\.[0-9]+\\.[0-9]+)\$"
  web_latest_tag=
  web_latest_note=
  code=$(web_get "$repo/releases/latest" "$body" "$hdr" 0)
  location=$(header_value "$hdr" location)
  if [[ ! $code =~ ^30[1278]$ || -z $location ]]; then
    die "https://github.com/$repo/releases/latest did not redirect to the Latest release: HTTP $code (the release list is denied to the token too)"
  fi
  if ! [[ $location =~ $re ]]; then
    web_latest_note="the Latest release of $repo is ${location##*/}, not a stable $prefix<X>.<Y>.<Z> tag"
    return 0
  fi
  tag=${BASH_REMATCH[1]}
  file=$(release_json "$repo" "$tag") || exit 2
  published=$(release_field "$file" '.published_at') || exit 2
  published_at=$(jq -rn --arg t "$published" '$t | fromdateiso8601') || die "invalid published_at: $published"
  prerelease=$(jq -r '.prerelease' "$file")
  if [[ $prerelease == true ]]; then
    web_latest_note="the Latest release of $repo, $tag, is a prerelease"
  elif ((now - published_at < min_seconds)); then
    web_latest_note="the Latest release of $repo, $tag, was published $published (less than $min_days days ago), so the newest release that is old enough is unknown"
  else
    web_latest_tag=$tag
  fi
}

# version_lt A B：A 的 semver 小于 B（两者都是严格 X.Y.Z，可带相同前缀）。
version_lt() {
  local lowest
  [[ $1 != "$2" ]] || return 1
  lowest=$(printf '%s\n%s\n' "$1" "$2" | sort -V)
  lowest=${lowest%%$'\n'*}
  [[ $lowest == "$1" ]]
}

if [[ $mode == latest ]]; then
  rc=0
  latest=$(latest_eligible "$latest_repo" v) || rc=$?
  if ((rc == 3)); then
    denied_msg_of "$latest_repo" || true
    # 不降级：见文件头「认证」。
    die "GitHub API GET repos/$latest_repo/releases failed: authenticated HTTP 403 ($denied_msg), not a rate limit;" \
      "the release list has no github.com page equivalent, so the latest eligible release of $latest_repo cannot be determined (failing closed)"
  fi
  ((rc == 0)) || exit 2
  if [[ -z $latest ]]; then
    printf 'pinned releases: %s has no stable vX.Y.Z release published at least %s days ago\n' "$latest_repo" "$min_days" >&2
    exit 1
  fi
  printf '%s\n' "$latest"
  exit 0
fi

collect_pins
failures=()
unknown_note=
for pin in "${pins[@]}"; do
  IFS=$'\t' read -r name repo prefix tag source <<<"$pin"
  check_pin "$pin"
  latest=-
  line_status=$status
  upgrade=
  if [[ $mode == report ]]; then
    rc=0
    latest=$(latest_eligible "$repo" "$prefix") || rc=$?
    if ((rc == 3)); then
      # 列表被拒绝：降级（文件头「release 列表」）。Latest 严格比固定点新才能断言 outdated；
      # 其余（不满 N 天、不是严格 tag、比固定点还旧、固定点正好是它）都不能证明「已是最新合格版」以外的事，
      # 固定点等于它是 ok，别的标 unknown。unknown 不算 finding：不让每周 job 因为查不到而红。
      web_latest "$repo" "$prefix"
      if [[ -z $web_latest_tag ]]; then
        latest=unknown
        unknown_note="$web_latest_note"
      elif [[ $tag == "$web_latest_tag" ]]; then
        latest=$web_latest_tag
      elif version_lt "$tag" "$web_latest_tag"; then
        latest=$web_latest_tag
        if [[ $status == ok ]]; then
          line_status=outdated
          upgrade="upgrade to $latest in $source"
        fi
      else
        latest=unknown
        unknown_note="the Latest release of $repo, $web_latest_tag, is older than the pinned $tag"
      fi
      if [[ $latest == unknown ]]; then
        [[ $status != ok ]] || line_status=unknown
        notes+=("$name: latest eligible release unknown: $unknown_note (listing denied to the token: authenticated HTTP 403, not a rate limit; read from github.com pages)")
      fi
    elif ((rc != 0)); then
      exit 2
    elif [[ -z $latest ]]; then
      latest=-
    elif [[ $status == ok ]] && version_lt "$tag" "$latest"; then
      line_status=outdated
      upgrade="upgrade to $latest in $source"
    fi
  fi
  if [[ $line_status != ok && $line_status != unknown ]]; then
    findings=$((findings + 1))
    [[ -n $detail ]] && failures+=("$name ($source): $detail")
  fi
  via=api # 发布时间的来源：api（GitHub API）或 web（github.com release 页面，见文件头「认证」）
  [[ -z $cur_source ]] || via=web
  rows+=("$name"$'\t'"$tag"$'\t'"$cur_published"$'\t'"$(fmt_days "$cur_age")"$'\t'"$latest"$'\t'"$line_status"$'\t'"$source"$'\t'"$via"$'\t'"$upgrade")
done

if [[ $mode == gate ]]; then
  printf 'minimum release age: %s days (scripts/release-age.env)\n' "$min_days"
  for row in "${rows[@]}"; do
    IFS=$'\t' read -r name tag published age latest st source via upgrade <<<"$row"
    marker=
    [[ $via != web ]] || marker=' [time from the github.com release page]'
    printf '  %-8s %-40s %-9s published %s (%s days ago)%s\n' "$st" "$name" "$tag" "$published" "$age" "$marker"
  done
  if ((${#failures[@]} > 0)); then
    printf '\nFAIL: %d pinned release(s) violate the minimum release age:\n' "${#failures[@]}" >&2
    printf '  - %s\n' "${failures[@]}" >&2
    exit 1
  fi
  printf 'OK: every pinned release is at least %s days old\n' "$min_days"
  exit 0
fi

# 报告模式
printf 'minimum release age: %s days (scripts/release-age.env); latest = highest stable semver released >= %s days ago\n' \
  "$min_days" "$min_days"
printf '%-40s %-10s %-21s %6s  %-10s %-9s %s\n' pin current 'published (UTC)' 'age(d)' latest status source
for row in "${rows[@]}"; do
  IFS=$'\t' read -r name tag published age latest st source via upgrade <<<"$row"
  printf '%-40s %-10s %-21s %6s  %-10s %-9s %s\n' "$name" "$tag" "$published" "$age" "$latest" "$st" "$source"
done
for row in "${rows[@]}"; do
  IFS=$'\t' read -r name tag published age latest st source via upgrade <<<"$row"
  [[ -n $upgrade ]] && printf '  outdated: %s %s -> %s (%s)\n' "$name" "$tag" "$latest" "$upgrade"
done
if ((${#failures[@]} > 0)); then
  printf '  %s\n' "${failures[@]}"
fi
if ((${#notes[@]} > 0)); then
  printf '  unknown: %s\n' "${notes[@]}"
fi
for row in "${rows[@]}"; do
  IFS=$'\t' read -r name tag published age latest st source via upgrade <<<"$row"
  [[ $via != web ]] || printf '  note: %s: the authenticated API request was denied (403, not a rate limit); published time read from the github.com release page\n' "$name"
done
printf 'not checked (no GitHub release to look up): BuildKit image digests, e2e SeaweedFS/Valkey/Postgres images, '
printf 'mise.toml tool versions, scripts/ci-setup-mise.sh tool list, entc.mod/entc.sum\n'

if [[ -n $markdown ]]; then
  {
    printf '手工固定点的新版本检查（最小发布年龄 %s 天，来源 `scripts/release-age.env`）。「最新」是已发布满 %s 天的最高稳定 semver。\n\n' \
      "$min_days" "$min_days"
    printf '| 固定点 | 当前 | 发布时间 (UTC) | 已发布(天) | 最新(满 %s 天) | 状态 | 位置 |\n' "$min_days"
    printf '| --- | --- | --- | ---: | --- | --- | --- |\n'
    for row in "${rows[@]}"; do
      IFS=$'\t' read -r name tag published age latest st source via upgrade <<<"$row"
      printf '| %s | `%s` | %s | %s | `%s` | %s | `%s` |\n' "$name" "$tag" "$published" "$age" "$latest" "$st" "$source"
    done
    if ((${#notes[@]} > 0)); then
      printf '\n状态 `unknown`：查不到「已满 %s 天的最新版」，不算 outdated，也不让本次检查变红：\n\n' "$min_days"
      printf -- '- %s\n' "${notes[@]}"
    fi
    printf '\n升级时只改上表「位置」里的版本号和对应 sha256，不改本脚本；升级后重新跑 `mise run pinned-releases`。\n'
    printf '暂不检查：BuildKit 镜像 digest、e2e 的 SeaweedFS/Valkey/Postgres 镜像、mise.toml 工具版本、'
    printf 'scripts/ci-setup-mise.sh 工具清单、entc.mod/entc.sum。\n'
  } >"$markdown"
fi

if ((findings > 0)); then
  printf 'FAIL: %d pinned release(s) are outdated or too new\n' "$findings" >&2
  exit 1
fi
if ((${#notes[@]} > 0)); then
  printf 'OK: no pinned release is outdated or too new (%d with an unknown latest eligible release, see above)\n' "${#notes[@]}"
else
  printf 'OK: all pinned releases are up to date\n'
fi
