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
# 说明 token 本身被拒：有的组织（如 aquasecurity）开了 IP allow list，会拦截 Actions 的 GITHUB_TOKEN，
# 而公开仓库的匿名读取不受影响。这种 403 去掉 Authorization 匿名重试一次，匿名也失败才退出 2。
# 限流（429，或 403 且命中上述限流特征）不做匿名重试，直接退出 2 并写明限流。每次失败都会打印响应体的 message。
#
# 测试钩子（scripts/check-pinned-releases-test.sh 用，离线）：
#   LO_PINNED_RELEASES_ROOT     固定点文件所在的根目录，默认仓库根；
#   LO_PINNED_RELEASES_NOW      当前时间（epoch 秒），默认 date +%s；
#   LO_PINNED_RELEASES_FIXTURE  假 API 目录：<owner>__<repo>/releases.json 与 <owner>__<repo>/tags/<tag>.json。
#                               想让某个请求失败，在对应文件旁放 <文件>.auth-error（带认证的请求）或
#                               <文件>.anon-error（匿名请求），内容是一个迷你 HTTP 响应：第一行状态码，
#                               随后若干响应头行，空行，再是响应体；
#   LO_PINNED_RELEASES_FIXTURE_LOG  若设置，每个假请求追加一行 "auth|anon <API 路径>"，测试用它断言是否走了匿名重试。

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

# http_get PATH AUTH OUT HDR：GET 一次，响应体写进 OUT、响应头写进 HDR，stdout 打印 HTTP 状态码。
# AUTH=1 带 Authorization，0 匿名。网络层失败（连不上、超时）直接退出 2。
http_get() {
  local path=$1 auth=$2 out=$3 hdr=$4 code
  if [[ -n $fixture ]]; then
    local file kind line
    file=$(fixture_file "$path")
    kind=anon
    [[ $auth == 1 ]] && kind=auth
    [[ -z ${LO_PINNED_RELEASES_FIXTURE_LOG:-} ]] || printf '%s %s\n' "$kind" "$path" >>"$LO_PINNED_RELEASES_FIXTURE_LOG"
    : >"$hdr"
    : >"$out"
    if [[ -f $file.$kind-error ]]; then
      # 迷你 HTTP 响应：状态码、响应头、空行、响应体。
      {
        IFS= read -r code
        while IFS= read -r line; do
          line=${line%$'\r'}
          [[ -n $line ]] || break
          printf '%s\n' "$line" >>"$hdr"
        done
        cat >"$out"
      } <"$file.$kind-error"
      printf '%s\n' "$code"
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

# api_get PATH OUT：把 JSON 写进 OUT。失败时带真实 HTTP 状态码和响应体的 message 退出，不返回空结果。
# 带认证的 403 若不是限流，去掉认证匿名重试一次（见文件头「认证」）；限流不重试。
api_get() {
  local path=$1 out=$2 hdr=$work/headers auth=0 code msg hint=
  [[ -n $token ]] && auth=1
  code=$(http_get "$path" "$auth" "$out" "$hdr") || return 2
  [[ $code == 200 ]] && return 0
  msg=$(error_message "$out")
  if [[ $code == 403 && $auth == 1 ]] && ! is_rate_limited "$code" "$hdr" "$msg"; then
    printf 'pinned releases: GET %s: the authenticated request got HTTP 403 (%s), which is not a rate limit; retrying anonymously\n' \
      "$path" "$msg" >&2
    local anon_code anon_msg
    anon_code=$(http_get "$path" 0 "$out" "$hdr") || return 2
    [[ $anon_code == 200 ]] && return 0
    anon_msg=$(error_message "$out")
    if is_rate_limited "$anon_code" "$hdr" "$anon_msg"; then hint=$(rate_limit_hint "$hdr"); fi
    die "GitHub API GET $path failed: authenticated HTTP 403 ($msg), then anonymous HTTP $anon_code ($anon_msg)$hint"
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

# release_json REPO TAG：单个 release（缓存）。
release_json() {
  local repo=$1 tag=$2 cache
  cache=$work/rel-${repo//\//__}-$tag.json
  [[ -f $cache ]] || api_get "repos/$repo/releases/tags/$tag" "$cache" || return 2
  printf '%s\n' "$cache"
}

# releases_json REPO：最新的最多 300 条 release（缓存），拼成一个 JSON 数组。
releases_json() {
  local repo=$1 cache page
  cache=$work/list-${repo//\//__}.json
  if [[ ! -f $cache ]]; then
    if [[ -n $fixture ]]; then
      api_get "repos/$repo/releases" "$cache" || return 2
    else
      local pages=()
      for page in 1 2 3; do
        api_get "repos/$repo/releases?per_page=100&page=$page" "$work/page-$page.json" || return 2
        pages+=("$work/page-$page.json")
        [[ $(jq 'length' "$work/page-$page.json") -ge 100 ]] || break
      done
      jq -s 'add' "${pages[@]}" >"$cache"
    fi
  fi
  printf '%s\n' "$cache"
}

# ---------------------------------------------------------------------------
# 判定
# ---------------------------------------------------------------------------

findings=0
rows=()      # 报告行（TSV）

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
}

# 已满 N 天的最新稳定版：严格匹配 <前缀>X.Y.Z，排除 draft/prerelease，按 semver 取最大。
latest_eligible() {
  local repo=$1 prefix=$2 list
  list=$(releases_json "$repo") || return 2
  jq -r --arg prefix "$prefix" --argjson cutoff $((now - min_seconds)) '
    [ .[]
      | select(.draft == false and .prerelease == false and .published_at != null)
      | select((.published_at | fromdateiso8601) <= $cutoff)
      | (.tag_name | capture("^" + $prefix + "(?<a>[0-9]+)\\.(?<b>[0-9]+)\\.(?<c>[0-9]+)$")?) as $v
      | select($v != null)
      | {tag: .tag_name, key: [$v.a, $v.b, $v.c | tonumber]} ]
    | if length == 0 then "" else (max_by(.key) | .tag) end' "$list"
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
  latest=$(latest_eligible "$latest_repo" v) || exit 2
  if [[ -z $latest ]]; then
    printf 'pinned releases: %s has no stable vX.Y.Z release published at least %s days ago\n' "$latest_repo" "$min_days" >&2
    exit 1
  fi
  printf '%s\n' "$latest"
  exit 0
fi

collect_pins
failures=()
for pin in "${pins[@]}"; do
  IFS=$'\t' read -r name repo prefix tag source <<<"$pin"
  check_pin "$pin"
  latest=-
  line_status=$status
  upgrade=
  if [[ $mode == report ]]; then
    latest=$(latest_eligible "$repo" "$prefix") || exit 2
    if [[ -z $latest ]]; then
      latest=-
    elif [[ $status == ok ]] && version_lt "$tag" "$latest"; then
      line_status=outdated
      upgrade="upgrade to $latest in $source"
    fi
  fi
  if [[ $line_status != ok ]]; then
    findings=$((findings + 1))
    [[ -n $detail ]] && failures+=("$name ($source): $detail")
  fi
  rows+=("$name"$'\t'"$tag"$'\t'"$cur_published"$'\t'"$(fmt_days "$cur_age")"$'\t'"$latest"$'\t'"$line_status"$'\t'"$source"$'\t'"$upgrade")
done

if [[ $mode == gate ]]; then
  printf 'minimum release age: %s days (scripts/release-age.env)\n' "$min_days"
  for row in "${rows[@]}"; do
    IFS=$'\t' read -r name tag published age latest st source upgrade <<<"$row"
    printf '  %-8s %-40s %-9s published %s (%s days ago)\n' "$st" "$name" "$tag" "$published" "$age"
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
  IFS=$'\t' read -r name tag published age latest st source upgrade <<<"$row"
  printf '%-40s %-10s %-21s %6s  %-10s %-9s %s\n' "$name" "$tag" "$published" "$age" "$latest" "$st" "$source"
done
for row in "${rows[@]}"; do
  IFS=$'\t' read -r name tag published age latest st source upgrade <<<"$row"
  [[ -n $upgrade ]] && printf '  outdated: %s %s -> %s (%s)\n' "$name" "$tag" "$latest" "$upgrade"
done
if ((${#failures[@]} > 0)); then
  printf '  %s\n' "${failures[@]}"
fi
printf 'not checked (no GitHub release to look up): BuildKit image digests, e2e SeaweedFS/Valkey/Postgres images, '
printf 'mise.toml tool versions, scripts/ci-setup-mise.sh tool list, entc.mod/entc.sum\n'

if [[ -n $markdown ]]; then
  {
    printf '手工固定点的新版本检查（最小发布年龄 %s 天，来源 `scripts/release-age.env`）。「最新」是已发布满 %s 天的最高稳定 semver。\n\n' \
      "$min_days" "$min_days"
    printf '| 固定点 | 当前 | 发布时间 (UTC) | 已发布(天) | 最新(满 %s 天) | 状态 | 位置 |\n' "$min_days"
    printf '| --- | --- | --- | ---: | --- | --- | --- |\n'
    for row in "${rows[@]}"; do
      IFS=$'\t' read -r name tag published age latest st source upgrade <<<"$row"
      printf '| %s | `%s` | %s | %s | `%s` | %s | `%s` |\n' "$name" "$tag" "$published" "$age" "$latest" "$st" "$source"
    done
    printf '\n升级时只改上表「位置」里的版本号和对应 sha256，不改本脚本；升级后重新跑 `mise run pinned-releases`。\n'
    printf '暂不检查：BuildKit 镜像 digest、e2e 的 SeaweedFS/Valkey/Postgres 镜像、mise.toml 工具版本、'
    printf 'scripts/ci-setup-mise.sh 工具清单、entc.mod/entc.sum。\n'
  } >"$markdown"
fi

if ((findings > 0)); then
  printf 'FAIL: %d pinned release(s) are outdated or too new\n' "$findings" >&2
  exit 1
fi
printf 'OK: all pinned releases are up to date\n'
