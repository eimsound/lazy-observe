#!/usr/bin/env bash
set -euo pipefail

# scripts/check-pinned-releases.sh 的离线测试：用假 GitHub API（LO_PINNED_RELEASES_FIXTURE）和固定的「当前时间」，
# 断言门禁与报告的判定。固定点文件从仓库复制到临时目录后把版本号改成测试值，所以真实版本升级不会让测试失效。
# workflow-check 里跑它；真实 API 的检查是 mise run pinned-releases。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
script=$root/scripts/check-pinned-releases.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}

# 当前时间固定为 2026-11-19T12:00:00Z。
now=$(date -u -d 2026-11-19T12:00:00Z +%s)
export LO_PINNED_RELEASES_NOW=$now
unset GH_TOKEN GITHUB_TOKEN

# 固定点文件副本：所有版本改成测试值。每次调用都从仓库重新复制，把前面用例改过的状态恢复成基线。
fixroot=$work/root
setup_root() {
  rm -rf "$fixroot"
  mkdir -p "$fixroot/scripts"
  local file
  for file in download-storage-bins.sh mise-release.env check-image-vulns.sh install-trivy.sh e2e-multi-replica.sh; do
    [[ -f $root/scripts/$file ]] && cp "$root/scripts/$file" "$fixroot/scripts/$file"
  done
  sed -i -E \
    -e 's/^([[:space:]]*)metrics_version=.*/\1metrics_version=v1.10.0/' \
    -e 's/^([[:space:]]*)logs_version=.*/\1logs_version=v1.20.0/' \
    -e 's/^([[:space:]]*)traces_version=.*/\1traces_version=v0.30.0/' \
    -e 's/^MISE_BOOTSTRAP_VERSION=.*/MISE_BOOTSTRAP_VERSION=v2026.1.1/' \
    -e 's/^([[:space:]]*)trivy_version=.*/\1trivy_version=0.50.0/' \
    -e 's/^([[:space:]]*)TRIVY_VERSION=.*/\1TRIVY_VERSION=0.50.0/' \
    -e 's#(quay\.io/victoriametrics/vm(storage|insert|select)):v[0-9.]+-cluster-scratch#\1:v1.10.0-cluster-scratch#' \
    "$fixroot"/scripts/*
}
setup_root

fx=$work/api
mkdir -p "$fx"

# release REPO TAG DAYS_AGO [prerelease] [draft]：往假 API 里加一个 release，发布于「当前时间 - DAYS_AGO 天」。
release() {
  local repo=$1 tag=$2 days=$3 prerelease=${4:-false} draft=${5:-false} dir published
  dir=$fx/${repo//\//__}
  mkdir -p "$dir/tags"
  published=$(date -u -d "@$((now - days * 86400))" +%Y-%m-%dT%H:%M:%SZ)
  jq -n --arg tag "$tag" --arg published "$published" --argjson pre "$prerelease" --argjson draft "$draft" \
    '{tag_name: $tag, published_at: $published, prerelease: $pre, draft: $draft}' >"$dir/tags/$tag.json"
  [[ -f $dir/releases.json ]] || printf '[]\n' >"$dir/releases.json"
  jq --slurpfile r "$dir/tags/$tag.json" '. + $r' "$dir/releases.json" >"$dir/releases.tmp"
  mv "$dir/releases.tmp" "$dir/releases.json"
}

# 基线：每个固定点的当前版本都发布得足够早；最新稳定版比当前新。
release VictoriaMetrics/VictoriaMetrics v1.10.0 100
release VictoriaMetrics/VictoriaLogs v1.20.0 100
release VictoriaMetrics/VictoriaTraces v0.30.0 100
release jdx/mise v2026.1.1 100
release aquasecurity/trivy v0.50.0 100
# VictoriaMetrics 的干扰项：-cluster/-enterprise 后缀、prerelease、draft、不满 7 天、LTS 线（semver 更小但发布更晚）。
release VictoriaMetrics/VictoriaMetrics v1.12.0 30
release VictoriaMetrics/VictoriaMetrics v1.99.0-cluster 20
release VictoriaMetrics/VictoriaMetrics v1.99.0-enterprise 20
release VictoriaMetrics/VictoriaMetrics v1.98.0 20 true
release VictoriaMetrics/VictoriaMetrics v1.97.0 20 false true
release VictoriaMetrics/VictoriaMetrics v1.96.0 6
release VictoriaMetrics/VictoriaMetrics v1.5.3 8

requests=$work/requests.log
run() { # run MODE...：输出到 $work/out，退出码放 rc，假 API 收到的请求记在 $requests
  rc=0
  : >"$requests"
  LO_PINNED_RELEASES_ROOT=$fixroot LO_PINNED_RELEASES_FIXTURE=$fx LO_PINNED_RELEASES_FIXTURE_LOG=$requests \
    bash "$script" "$@" >"$work/out" 2>&1 || rc=$?
}
expect_rc() {
  [[ $rc -eq $1 ]] || fail "$2: exit $rc, want $1"$'\n'"$(cat "$work/out")"
}
expect_out() {
  [[ $(<"$work/out") == *"$1"* ]] || fail "$2: output lacks '$1'"$'\n'"$(cat "$work/out")"
}
reject_out() {
  [[ $(<"$work/out") != *"$1"* ]] || fail "$2: output must not contain '$1'"$'\n'"$(cat "$work/out")"
}

# 1. 门禁：所有固定点都够老 -> 通过。
run --gate
expect_rc 0 'gate passes when every pin is old enough'
expect_out 'OK: every pinned release is at least 7 days old' 'gate pass message'
expect_out 'VictoriaLogs' 'gate lists VictoriaLogs'
expect_out 'e2e image' 'gate covers the e2e cluster images'

# 2. 报告：最新稳定版取已满 7 天、严格 X.Y.Z、非 prerelease/draft 里 semver 最大的（v1.12.0），
#    不是 -cluster/-enterprise、prerelease、draft、不满 7 天的 v1.96.0，也不是 LTS 补丁 v1.5.3。
run --report
expect_rc 1 'report exits 1 when something is outdated'
expect_out 'v1.12.0' 'report picks the highest eligible VictoriaMetrics release'
expect_out 'outdated' 'report marks outdated pins'
reject_out 'v1.99.0' 'report ignores -cluster/-enterprise tags'
reject_out 'v1.98.0' 'report ignores prereleases'
reject_out 'v1.97.0' 'report ignores drafts'
reject_out 'v1.96.0' 'report ignores releases younger than 7 days'

# 3. 报告：升级后无 outdated -> 退出 0。VictoriaLogs/Traces/mise/Trivy 没有更新的 release，只有 VictoriaMetrics 落后，
#    把 VictoriaMetrics 的固定点（含 e2e 镜像）改到 v1.12.0 即全部最新。
sed -i -E \
  -e 's/^([[:space:]]*)metrics_version=.*/\1metrics_version=v1.12.0/' \
  -e 's#(quay\.io/victoriametrics/vm(storage|insert|select)):v[0-9.]+-cluster-scratch#\1:v1.12.0-cluster-scratch#' \
  "$fixroot"/scripts/*
run --report --markdown "$work/report.md"
expect_rc 0 'report passes when every pin is the latest eligible release'
expect_out 'OK: all pinned releases are up to date' 'report ok message'
[[ -s $work/report.md ]] && [[ $(<"$work/report.md") == *'| VictoriaLogs |'* ]] || fail 'report --markdown must write a table'

# 4. 门禁：固定点引用的 release 发布不满 7 天 -> 失败，并报出固定点、版本、发布时间与还要等多久。
release VictoriaMetrics/VictoriaLogs v1.21.0 3
sed -i -E 's/^([[:space:]]*)logs_version=.*/\1logs_version=v1.21.0/' "$fixroot/scripts/download-storage-bins.sh"
run --gate
expect_rc 1 'gate fails for a pin younger than 7 days'
expect_out 'too-new' 'gate marks the young pin'
expect_out 'logs_version' 'gate names the pin location'
expect_out 'v1.21.0' 'gate names the version'
expect_out 'published 20' 'gate shows the publish time'
expect_out 'wait until 2026-11-23T12:00:00Z (4d 0h from now)' 'gate shows how long to wait'

# 5. 边界：恰好 7 天算通过，差一秒不算。
release VictoriaMetrics/VictoriaLogs v1.22.0 7
sed -i -E 's/^([[:space:]]*)logs_version=.*/\1logs_version=v1.22.0/' "$fixroot/scripts/download-storage-bins.sh"
run --gate
expect_rc 0 'gate accepts a release exactly 7 days old'
LO_PINNED_RELEASES_NOW=$((now - 1)) run --gate
expect_rc 1 'gate rejects a release one second short of 7 days'

# 6. prerelease 被固定也失败。
release VictoriaMetrics/VictoriaLogs v1.23.0 30 true
sed -i -E 's/^([[:space:]]*)logs_version=.*/\1logs_version=v1.23.0/' "$fixroot/scripts/download-storage-bins.sh"
run --gate
expect_rc 1 'gate rejects a pinned prerelease'
expect_out 'prerelease' 'gate explains the prerelease'

# 7. 报错一律失败：API 查不到（网络失败/404/限流）-> 退出 2，不放行。
release VictoriaMetrics/VictoriaLogs v1.20.0 100
sed -i -E 's/^([[:space:]]*)logs_version=.*/\1logs_version=v1.24.0/' "$fixroot/scripts/download-storage-bins.sh"
run --gate
expect_rc 2 'gate fails closed when the release cannot be fetched'
expect_out 'failed' 'gate explains the fetch failure'

# 8. 固定点格式漂移（变量没了）-> 退出 2，不当作「没有固定点」放行。
sed -i -E '/^([[:space:]]*)logs_version=/d' "$fixroot/scripts/download-storage-bins.sh"
run --gate
expect_rc 2 'gate fails when a pin can no longer be parsed'
expect_out 'logs_version' 'gate names the missing pin'

# ---------------------------------------------------------------------------
# 认证 403：aquasecurity 这类开了 IP allow list 的组织会拒绝 Actions 的 GITHUB_TOKEN。来源顺序：带认证 API → github.com
# release 网页 → 匿名 API（最后备选）。下面的用例故意让三个来源给出不同的答案（API 与匿名说 100 天，网页说别的），
# 用结果判断到底采信了哪一个。
# ---------------------------------------------------------------------------
setup_root
trivy_tag=$fx/aquasecurity__trivy/tags/v0.50.0.json
trivy_path=repos/aquasecurity/trivy/releases/tags/v0.50.0
trivy_web_path=aquasecurity/trivy/releases/tag/v0.50.0
trivy_page=$fx/aquasecurity__trivy/web/tags/v0.50.0.html
now_plus_hour=$((now + 3600))
# inject_error KIND CONTENT：给 trivy 当前 tag 的请求注入失败（KIND=auth|anon|web）；CONTENT 是迷你 HTTP 响应。
inject_error() {
  case $1 in
  web) printf '%s' "$2" >"$trivy_page.web-error" ;;
  *) printf '%s' "$2" >"$trivy_tag.$1-error" ;;
  esac
}
clear_errors() { rm -f "$trivy_tag".auth-error "$trivy_tag".anon-error "$trivy_page".web-error; }
requests_of() { # 某类请求在日志里出现的次数
  local kind=$1 line n=0 want=$trivy_path
  [[ $kind != web ]] || want=$trivy_web_path
  while IFS= read -r line; do
    [[ $line == "$kind $want" ]] && n=$((n + 1))
  done <"$requests"
  printf '%s\n' "$n"
}
expect_requests() { # expect_requests AUTH WEB ANON 说明
  [[ $(requests_of auth) == "$1" && $(requests_of web) == "$2" && $(requests_of anon) == "$3" ]] ||
    fail "$4: want auth=$1 web=$2 anon=$3 requests, got auth=$(requests_of auth) web=$(requests_of web) anon=$(requests_of anon)"
}

# web_page FILE DAYS_AGO [prerelease]：写一个假的 release 页面。真实页面里「released this」之前还有别的
# <relative-time>（这里放一个 2000 年的诱饵），之后有 tag 的提交时间，解析必须锚定在「released this」上。
web_page() {
  local file=$1 days=$2 pre=${3:-false} published
  published=$(date -u -d "@$((now - days * 86400))" +%Y-%m-%dT%H:%M:%SZ)
  mkdir -p "$(dirname "$file")"
  {
    printf '<html><body>\n<relative-time datetime="2000-01-01T00:00:00Z">Jan 2000</relative-time>\n'
    if [[ $pre == true ]]; then
      printf '<span data-view-component="true" class="Label Label--warning Label--large v-align-text-bottom">Pre-release</span>\n'
    fi
    printf '  <a class="text-bold" href="/someone">someone</a>\n\n    released this\n\n'
    printf '      <relative-time class="no-wrap" prefix="" datetime="%s">\n        some day\n      </relative-time>\n' "$published"
    printf '<relative-time datetime="1999-12-31 00:00:00 UTC" threshold="PT0S">tag time</relative-time>\n</body></html>\n'
  } >"$file"
}
deny_403='403
content-type: application/json
x-ratelimit-remaining: 4999

{"message":"Forbidden by IP allow list"}'

# 9. 认证 403（非限流）：改读 github.com 网页，网页说 100 天 -> 通过。不打匿名 API；输出里写明消息和来源。
web_page "$trivy_page" 100
inject_error auth "$deny_403"
GH_TOKEN=fake run --gate
expect_rc 0 'authenticated 403 (not a rate limit) falls back to the github.com release page'
expect_out 'OK: every pinned release is at least 7 days old' 'web fallback gate pass message'
expect_out 'Forbidden by IP allow list' 'web fallback prints the 403 message'
expect_out 'github.com/aquasecurity/trivy/releases/tag/v0.50.0' 'web fallback names the page it reads'
expect_out '[time from the github.com release page]' 'the gate marks which pins used the page time'
expect_requests 1 1 0 'a working release page needs no anonymous request'

# 9a. 网页显示不满 7 天 -> 门禁阻塞（API 与匿名都说 100 天，说明确实采信了网页而不是回头问匿名 API）。
web_page "$trivy_page" 3
GH_TOKEN=fake run --gate
expect_rc 1 'a release page that shows a young release blocks the gate'
expect_out 'too-new' 'young page marks the pin too-new'
expect_out 'v0.50.0' 'young page names the version'
expect_out 'wait until 2026-11-23T12:00:00Z (4d 0h from now)' 'young page shows how long to wait'
expect_requests 1 1 0 'a young page verdict is final: no anonymous request'
# 恰好 7 天算通过。
web_page "$trivy_page" 7
GH_TOKEN=fake run --gate
expect_rc 0 'a release page showing exactly 7 days passes'
# 页面带 Pre-release 标签 -> 失败。
web_page "$trivy_page" 100 true
GH_TOKEN=fake run --gate
expect_rc 1 'a release page labelled Pre-release blocks the gate'
expect_out 'prerelease' 'the page prerelease label is explained'
web_page "$trivy_page" 100

# 9b. curl --retry 会把多次响应的头累积在同一个文件里：更早的 503 带 retry-after，最后一次才是真正的 403（无限流特征）。
#     只能看最后一段头，否则这个 403 会被误判成限流而不改读网页。
inject_error auth $'403\nHTTP/1.1 503 Service Unavailable\nretry-after: 1\nx-ratelimit-remaining: 0\nHTTP/1.1 403 Forbidden\ncontent-type: application/json\nx-ratelimit-remaining: 4999\n\n{"message":"Forbidden by IP allow list"}'
GH_TOKEN=fake run --gate
expect_rc 0 'only the last response header block counts: an earlier 503 retry-after must not make a plain 403 look rate limited'
expect_requests 1 1 0 'plain 403 after a retried 503 reads the release page'
# 反过来：最后一段头才是限流，不读网页、不匿名。
inject_error auth $'403\nHTTP/1.1 503 Service Unavailable\nHTTP/1.1 403 Forbidden\nretry-after: 30\n\n{"message":"Forbidden"}'
GH_TOKEN=fake run --gate
expect_rc 2 'a rate limit in the last header block is still a rate limit'
expect_requests 1 0 0 'a rate limit in the last header block must neither read the page nor retry anonymously'
inject_error auth "$deny_403"

# 9c. 网页来源失败（404、5xx、没有「released this」结构、网络错误）-> 最后备选匿名 API；匿名 200（说 100 天）-> 通过。
#     这是最后一步，且打印了网页为什么失败。
for failure in $'404\n\nNot Found' $'502\n\nbad gateway' $'000\n\n'; do
  inject_error web "$failure"
  GH_TOKEN=fake run --gate
  expect_rc 0 "a failing release page ($(head -1 <<<"$failure")) falls back to the anonymous API"
  expect_out 'trying the anonymous API as a last resort' 'the last-resort anonymous request is announced'
  expect_requests 1 1 1 "a failing release page ($(head -1 <<<"$failure")) costs exactly one anonymous request"
done
clear_errors
inject_error auth "$deny_403"
printf '<html><body>no release here, only a git tag</body></html>\n' >"$trivy_page"
GH_TOKEN=fake run --gate
expect_rc 0 'a page without the released-this structure is not trusted: falls back to the anonymous API'
expect_out 'no "released this <relative-time datetime=...>"' 'the missing page structure is named'
expect_requests 1 1 1 'a page without the released-this structure costs one anonymous request'
web_page "$trivy_page" 100

# 10. 认证 403、网页失败、匿名也 403 -> 退出 2，三个来源各自的状态码与 message 都要出现。
inject_error web $'404\n\nNot Found'
inject_error anon $'403\n\n{"message":"Blocked for all"}'
GH_TOKEN=fake run --gate
expect_rc 2 'gate fails closed when the release page and the anonymous retry both fail'
expect_out 'authenticated HTTP 403 (Forbidden by IP allow list), then github.com release page failed (HTTP 404 (release page not found)), then anonymous HTTP 403 (Blocked for all)' \
  'all three failures are reported with their messages'
clear_errors

# 10a. 事故现场（main push run 44487084652）：认证 403、网页取不到、匿名被别人用光限流 -> 退出 2，写明限流与重置时间。
inject_error auth "$deny_403"
inject_error web $'500\n\nserver error'
inject_error anon $'403\nx-ratelimit-remaining: 0\nx-ratelimit-reset: '"$now_plus_hour"$'\n\n{"message":"API rate limit exceeded for 98.89.53.239."}'
GH_TOKEN=fake run --gate
expect_rc 2 'authenticated 403 + failing page + rate-limited anonymous API fails closed'
expect_out 'github.com release page failed (HTTP 500)' 'the page failure is reported'
expect_out 'then anonymous HTTP 403 (API rate limit exceeded for 98.89.53.239.)' 'the anonymous rate limit message is reported'
expect_out 'resets at 2026-11-19T13:00:00Z' 'the anonymous rate limit reset time is shown'
expect_requests 1 1 1 'the incident path tries every source exactly once'
clear_errors

# 10b. 网页 200 却解析不出时间，匿名也失败 -> 退出 2，不放行。
inject_error auth "$deny_403"
inject_error anon $'403\n\n{"message":"Blocked for all"}'
printf '<html>released this <relative-time datetime="not-a-time">x</relative-time></html>\n' >"$trivy_page"
GH_TOKEN=fake run --gate
expect_rc 2 'an unparsable release page must not let the gate pass'
expect_out 'no "released this <relative-time datetime=...>"' 'the unparsable page is named'
web_page "$trivy_page" 100
clear_errors

# 11. 限流 403（x-ratelimit-remaining: 0）-> 退出 2，写明限流和重置时间，不读网页、不走匿名。
inject_error auth $'403\nx-ratelimit-remaining: 0\nx-ratelimit-reset: '"$now_plus_hour"$'\n\n{"message":"API rate limit exceeded for 1.2.3.4."}'
GH_TOKEN=fake run --gate
expect_rc 2 'a rate-limited 403 is a failure'
expect_out 'rate limited' 'rate limit is named'
expect_out 'resets at 2026-11-19T13:00:00Z' 'rate limit reset time is shown'
expect_out 'API rate limit exceeded' 'rate limit message is shown'
expect_requests 1 0 0 'a rate-limited 403 must neither read the page nor retry anonymously'
clear_errors

# 12. 次级限流：403 没有 x-ratelimit 头，只有 message 提到 rate limit -> 同样不读网页、不走匿名。
inject_error auth $'403\n\n{"message":"You have exceeded a secondary rate limit. Please wait."}'
GH_TOKEN=fake run --gate
expect_rc 2 'a secondary rate limit is a failure'
expect_out 'secondary rate limit' 'secondary rate limit message is shown'
expect_requests 1 0 0 'a secondary rate limit must neither read the page nor retry anonymously'
clear_errors

# 13. 响应体不是 JSON：jq 解析失败不能吞掉错误，退回响应体文本；网页可用则通过。
inject_error auth $'403\ncontent-type: text/html\n\n<html>blocked by gateway</html>'
GH_TOKEN=fake run --gate
expect_rc 0 'a non-JSON 403 body still falls back to the release page'
expect_out 'blocked by gateway' 'a non-JSON error body is printed instead of being swallowed'
clear_errors

# 14. 没有 token：匿名请求的 403 不改读网页（没有认证可去掉，网页也不是答案），直接失败；也不是限流时写明「不是限流」。
inject_error anon $'403\n\n{"message":"Forbidden"}'
run --gate
expect_rc 2 'an anonymous 403 is a failure'
expect_out 'HTTP 403 (Forbidden)' 'anonymous 403 message is shown'
expect_out 'not a rate limit' 'anonymous 403 is not blamed on the rate limit'
expect_requests 0 0 1 'an anonymous 403 must not be retried or read from the page'
clear_errors

# 15. 429 -> 限流，不重试。
inject_error auth $'429\nretry-after: 60\n\n{"message":"Too many requests"}'
GH_TOKEN=fake run --gate
expect_rc 2 'HTTP 429 is a failure'
expect_out 'rate limited' '429 is named as a rate limit'
expect_requests 1 0 0 'HTTP 429 must neither read the page nor retry anonymously'
clear_errors

# 15b. 带 token 且 API 正常时不碰网页：网页只在认证 403 之后才是来源。
GH_TOKEN=fake run --gate
expect_rc 0 'a working authenticated API needs no fallback'
expect_requests 1 0 0 'a working authenticated API must not read the page'

# ---------------------------------------------------------------------------
# 报告模式：release 列表被拒绝（认证 403、非限流）的仓库没有网页等价物，降级用 releases/latest 重定向 + release 页面。
# ---------------------------------------------------------------------------
trivy_dir=$fx/aquasecurity__trivy
trivy_latest=$trivy_dir/web/latest
# 把 VictoriaMetrics、VictoriaLogs 追平（上面的用例给 VictoriaLogs 加过 v1.21.0~v1.23.0，已满 7 天的最新是 v1.22.0），
# 只让 Trivy 决定报告结果。
sed -i -E \
  -e 's/^([[:space:]]*)metrics_version=.*/\1metrics_version=v1.12.0/' \
  -e 's/^([[:space:]]*)logs_version=.*/\1logs_version=v1.22.0/' \
  -e 's#(quay\.io/victoriametrics/vm(storage|insert|select)):v[0-9.]+-cluster-scratch#\1:v1.12.0-cluster-scratch#' \
  "$fixroot"/scripts/*
deny_list() { # 组织的 IP allow list 拒绝这个仓库的所有认证请求：列表和每个 tag
  local tag
  printf '%s' "$deny_403" >"$trivy_dir/releases.json.auth-error"
  for tag in v0.49.0 v0.50.0 v0.51.0; do printf '%s' "$deny_403" >"$trivy_dir/tags/$tag.json.auth-error"; done
}
latest_redirect() { # latest_redirect TAG：releases/latest 重定向到 TAG
  mkdir -p "$trivy_dir/web"
  printf '302\nlocation: https://github.com/aquasecurity/trivy/releases/tag/%s\n\n' "$1" >"$trivy_latest"
}
cleanup_denied() { rm -f "$trivy_dir"/releases.json.auth-error "$trivy_dir"/tags/*.auth-error "$trivy_latest" "$trivy_latest".web-error "$trivy_dir"/web/tags/v0.5[0-9].*.html; }
deny_list
# trivy 的网页版本：固定点 v0.50.0 与 Latest v0.51.0（30 天前）。
web_page "$trivy_dir/web/tags/v0.51.0.html" 30
web_page "$trivy_page" 100

# R1. Latest 已满 7 天、比固定点新 -> outdated（退出 1），给出升级目标；固定点的请求已经发现这个仓库拒绝 token，列表不再打必败的请求。
latest_redirect v0.51.0
GH_TOKEN=fake run --report --markdown "$work/report-denied.md"
expect_rc 1 'report: the denied repo falls back to the Latest release and flags outdated'
expect_out 'upgrade to v0.51.0 in' 'report names the upgrade target for the denied repo'
expect_out 'outdated: Trivy v0.50.0 -> v0.51.0' 'report lists the outdated Trivy pin'
reject_out 'unknown' 'report must not call a resolvable denied repo unknown'
expect_out 'GET repos/aquasecurity/trivy/releases/tags/v0.51.0: skipped, an earlier authenticated request to aquasecurity/trivy got HTTP 403' \
  'a request skipped because the repo already denied the token is not logged as if it was sent'
[[ $(<"$work/report-denied.md") == *'`v0.51.0`'* ]] || fail 'markdown shows the fallback latest'
list_requests=0
while IFS= read -r line; do [[ $line == 'auth repos/aquasecurity/trivy/releases' ]] && list_requests=$((list_requests + 1)); done <"$requests"
((list_requests == 0)) || fail "a repo already known to deny the token must not get a doomed list request, got $list_requests"

# R2. 固定点就是 Latest 且已满 7 天 -> ok，退出 0。
latest_redirect v0.50.0
GH_TOKEN=fake run --report
expect_rc 0 'report: the pin equals the Latest release'
expect_out 'OK: all pinned releases are up to date' 'report ok message for the denied repo'

# R3. Latest 不满 7 天 -> 无法知道已满 7 天的最新版：unknown，不是 outdated 也不是 ok，退出 0（不让每周 job 误报）。
web_page "$trivy_dir/web/tags/v0.51.0.html" 3
latest_redirect v0.51.0
GH_TOKEN=fake run --report --markdown "$work/report-denied.md"
expect_rc 0 'report: an unknown latest eligible release does not turn the report red'
expect_out 'unknown' 'report marks the denied repo unknown'
expect_out 'unknown: Trivy: latest eligible release unknown' 'report explains why it is unknown'
expect_out 'less than 7 days ago' 'report says the Latest release is too young'
expect_out 'no pinned release is outdated or too new (1 with an unknown latest eligible release' 'report summary counts unknown rows'
reject_out 'outdated:' 'unknown must not be reported as outdated'
[[ $(<"$work/report-denied.md") == *'unknown'* ]] || fail 'markdown must mention the unknown row'
# 固定点本身仍照常判定：固定点不满 7 天 -> too-new（退出 1），latest 仍是 unknown。
web_page "$trivy_page" 2
GH_TOKEN=fake run --report
expect_rc 1 'report: a too-new pin is still too-new when the latest is unknown'
expect_out 'too-new' 'report keeps too-new for the denied repo'
web_page "$trivy_page" 100

# R4. Latest 重定向到的不是严格 vX.Y.Z（LTS/-rc/-cluster）-> unknown。
latest_redirect v0.51.0-rc1
GH_TOKEN=fake run --report
expect_rc 0 'report: a Latest that is not a strict vX.Y.Z tag is unknown'
expect_out 'not a stable v<X>.<Y>.<Z> tag' 'report says why the Latest tag is not usable'
# Latest 比固定点还旧（LTS 补丁线之类）-> unknown，不能据此说固定点是最新。
web_page "$trivy_dir/web/tags/v0.49.0.html" 60
latest_redirect v0.49.0
GH_TOKEN=fake run --report
expect_rc 0 'report: a Latest older than the pin is unknown'
expect_out 'is older than the pinned v0.50.0' 'report says the Latest is older than the pin'
# Latest 是 prerelease -> unknown。
web_page "$trivy_dir/web/tags/v0.51.0.html" 30 true
latest_redirect v0.51.0
GH_TOKEN=fake run --report
expect_rc 0 'report: a prerelease Latest is unknown'
expect_out 'is a prerelease' 'report says the Latest is a prerelease'

# R5. fail-closed：releases/latest 取不到、或 Latest 的页面取不到 -> 退出 2，不当 unknown 放过。
latest_redirect v0.51.0
printf '%s' $'500\n\nserver error' >"$trivy_latest.web-error"
GH_TOKEN=fake run --report
expect_rc 2 'report: a failing releases/latest redirect is a script error'
expect_out 'did not redirect to the Latest release' 'report names the failed redirect'
rm -f "$trivy_latest.web-error"
rm -f "$trivy_dir/web/tags/v0.51.0.html"
GH_TOKEN=fake run --report
expect_rc 2 'report: a Latest release page that cannot be read (and an unauthorized anonymous API) is a script error'
web_page "$trivy_dir/web/tags/v0.51.0.html" 30

# R6. --latest-eligible 对被拒绝的仓库：退出 2、stdout 为空，不降级（GitHub 的 Latest 不一定是 semver 最大，
#     而调用方会拿结果放宽「pin >= 最新合格版」的命中）；不读网页。
latest_redirect v0.51.0
rc=0
: >"$requests"
GH_TOKEN=fake LO_PINNED_RELEASES_ROOT=$work/empty LO_PINNED_RELEASES_FIXTURE=$fx LO_PINNED_RELEASES_FIXTURE_LOG=$requests \
  bash "$script" --latest-eligible aquasecurity/trivy >"$work/latest.out" 2>"$work/out" || rc=$?
expect_rc 2 'latest-eligible fails closed for a repo whose release list is denied'
[[ ! -s $work/latest.out ]] || fail "latest-eligible must print nothing for a denied repo, got: $(cat "$work/latest.out")"
expect_out 'cannot be determined (failing closed)' 'latest-eligible explains it does not degrade'
expect_out 'Forbidden by IP allow list' 'latest-eligible shows the 403 message'
[[ $(<"$requests") != *web* ]] || fail "latest-eligible must not read github.com pages for a denied repo: $(cat "$requests")"
cleanup_denied
clear_errors

# ---------------------------------------------------------------------------
# --latest-eligible：只打印已满 7 天的最新稳定版 tag，规则与报告一致，错误语义与门禁一致。
# ---------------------------------------------------------------------------
latest() { # latest ARGS...：stdout 放 $work/latest.out，stderr 放 $work/out，退出码放 rc
  rc=0
  : >"$requests"
  LO_PINNED_RELEASES_ROOT=$work/empty LO_PINNED_RELEASES_FIXTURE=$fx LO_PINNED_RELEASES_FIXTURE_LOG=$requests \
    bash "$script" "$@" >"$work/latest.out" 2>"$work/out" || rc=$?
}
mkdir -p "$work/empty"

# 16. 只在 stdout 打印 tag（没有多余输出），semver 取最大、跳过带后缀/prerelease/draft/不满 7 天；不读固定点文件。
latest --latest-eligible VictoriaMetrics/VictoriaMetrics
expect_rc 0 'latest-eligible succeeds without any pinned files'
[[ $(<"$work/latest.out") == v1.12.0 ]] ||
  fail "latest-eligible must print exactly the tag v1.12.0, got: $(cat "$work/latest.out")"

# 17. 刚好 7 天的 release 合格，差一秒不合格。
release example/boundary v1.0.0 20
release example/boundary v1.1.0 7
latest --latest-eligible example/boundary
[[ $(<"$work/latest.out") == v1.1.0 ]] || fail "a release exactly 7 days old is eligible, got: $(cat "$work/latest.out")"
LO_PINNED_RELEASES_NOW=$((now - 1)) latest --latest-eligible example/boundary
[[ $(<"$work/latest.out") == v1.0.0 ]] || fail "a release one second short of 7 days is not eligible, got: $(cat "$work/latest.out")"

# 18. 没有合格 release -> stdout 为空、退出 1。
release example/young v1.0.0 3
latest --latest-eligible example/young
expect_rc 1 'latest-eligible exits 1 when no release is old enough'
[[ ! -s $work/latest.out ]] || fail 'latest-eligible must print nothing when no release is eligible'

# 19. API 失败 -> 退出 2（仓库不存在、认证 403 且匿名也 403、限流）。
latest --latest-eligible example/missing
expect_rc 2 'latest-eligible fails closed when the API call fails'
[[ $(<"$work/out") != *'jq: error'* && $(<"$work/out") != *'jq:'* ]] ||
  fail "latest-eligible API failure must not leak a jq error: $(cat "$work/out")"
expect_out 'HTTP 404' 'latest-eligible reports the real HTTP status'
[[ ! -s $work/latest.out ]] || fail 'latest-eligible must print nothing when the API call fails'
printf '%s' $'403\n\n{"message":"denied"}' >"$fx/example__boundary/releases.json.auth-error"
printf '%s' $'403\n\n{"message":"denied"}' >"$fx/example__boundary/releases.json.anon-error"
GH_TOKEN=fake latest --latest-eligible example/boundary
expect_rc 2 'latest-eligible fails when both authenticated and anonymous requests are denied'
printf '%s' $'403\nx-ratelimit-remaining: 0\n\n{"message":"API rate limit exceeded"}' >"$fx/example__boundary/releases.json.auth-error"
GH_TOKEN=fake latest --latest-eligible example/boundary
expect_rc 2 'latest-eligible fails on a rate limit'
expect_out 'rate limited' 'latest-eligible names the rate limit'
rm -f "$fx"/example__boundary/releases.json.*-error

# 19b. 报告模式拉 release 列表失败 -> 退出 2，stderr 是 API 的真实错误，没有 jq 报错。
mv "$fx/jdx__mise/releases.json" "$work/mise-releases.json"
run --report
expect_rc 2 'report fails closed when a release list cannot be fetched'
expect_out 'GET repos/jdx/mise/releases' 'report names the failing request'
[[ $(<"$work/out") != *'jq:'* ]] || fail "report API failure must not leak a jq error: $(cat "$work/out")"
mv "$work/mise-releases.json" "$fx/jdx__mise/releases.json"

# 20. 参数不合法 -> 退出 2。
latest --latest-eligible not-a-repo
expect_rc 2 'latest-eligible rejects an argument that is not OWNER/REPO'
latest --latest-eligible
expect_rc 2 'latest-eligible needs an argument'
latest --latest-eligible VictoriaMetrics/VictoriaMetrics --gate
expect_rc 2 'latest-eligible cannot be combined with --gate'
expect_out 'usage:' 'combining modes prints usage'
latest --report --gate
expect_rc 2 '--report and --gate cannot be combined'

((failures == 0)) || {
  printf '%d check(s) failed\n' "$failures" >&2
  exit 1
}
printf 'check-pinned-releases tests passed\n'
