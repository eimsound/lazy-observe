#!/usr/bin/env bash
set -euo pipefail

# scripts/ci-setup-mise.sh 的供应链自测，守三条（被悄悄跳过时 CI 会执行未经验证的二进制、装上未固定的 latest，或触发无关非 lazy 工具安装）：
# - 下载的 mise 二进制 sha256 对不上时必须非零退出，且在 trust/install 之前停下；
# - 请求的工具在 mise.toml [tools] 里没有固定版本时必须非零退出，且不调用 `mise install`；
# - go:* 校验与 pnpm store 配置路径中所有 mise exec 均必须在 `--` 前显式指名工具，防止静默安装 mise.toml 中无关非 lazy 工具。
# 用 `env -i` + 假 curl + 假 mise + 临时 HOME 跑真实脚本，不在宿主机装任何工具，也不下载真实发布包。
# URL、PATH、安装步骤、报错文案等实现细节不断言。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
script=$root/scripts/ci-setup-mise.sh
release_env=$root/scripts/mise-release.env
bash_bin=$(command -v bash)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=scripts/mise-release.env
. "$release_env"

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}

# 假 mise 同时充当「下载的发布包」，这样 sha256 可以直接写进夹具。
# `mise tool` 按 FAKE_MISE_SOURCE 回答配置来源（版本固定为 1.0.0 且已安装），`where`/`exec` 返回可控夹具输出，其余命令只记日志。
payload=$work/fake-mise
cat >"$payload" <<'PAYLOAD'
#!/bin/sh
printf 'mise %s\n' "$*" >>"${MISE_CALL_LOG:?}"
case $1 in
tool)
  case $3 in
  --config-source) printf '%s\n' "${FAKE_MISE_SOURCE:?}" ;;
  --requested | --installed) printf '1.0.0\n' ;;
  esac
  ;;
where)
  printf '%s\n' "${FAKE_MISE_WHERE_DIR:?}"
  ;;
exec | x)
  shift
  while [ $# -gt 0 ]; do
    if [ "$1" = -- ]; then
      shift
      break
    fi
    shift
  done
  if [ "${1:-}" = go ] && [ "${2:-}" = env ]; then
    printf 'go1.27.4\n'
  elif [ "${1:-}" = go ] && [ "${2:-}" = version ]; then
    printf '%s: go1.27.4\n' "${3:-}"
  fi
  ;;
esac
PAYLOAD
chmod 0755 "$payload"
payload_sha=$(sha256sum "$payload" | awk '{print $1}')
zero_sha=0000000000000000000000000000000000000000000000000000000000000000

run_case() { # name sha expected_rc [config_source] [ci_tools]
  local name=$1 sha=$2 expected_rc=$3 source=${4:-$root/mise.toml} tools=${5:-go}
  local dir=$work/case-$name rc=0 call call_dir count=0
  local test_path=$dir/fakebin:/usr/bin:/bin
  local token=${FAKE_GITHUB_TOKEN:-}
  local api_fail=${FAKE_API_FAIL:-}
  local posix_bin=$dir/posixbin
  call_dir=$dir/curl-calls
  mkdir -p "$dir/fakebin" "$dir/home" "$dir/fake-tool-dir/bin" "$call_dir"
  : >"$dir/fake-tool-dir/bin/dummy-tool"
  cp "$script" "$dir/ci-setup-mise.sh"
  printf 'MISE_BOOTSTRAP_VERSION=%s\nMISE_BOOTSTRAP_SHA256_AMD64=%s\nMISE_BOOTSTRAP_SHA256_ARM64=%s\n' \
    "$MISE_BOOTSTRAP_VERSION" "$sha" "$sha" >"$dir/mise-release.env"
  printf '#!/bin/sh\nprintf "x86_64\\n"\n' >"$dir/fakebin/uname"
  cat >"$dir/fakebin/curl" <<'CURL'
#!/bin/sh
set -eu
out=
url=
wfmt=
max_redirs=
has_L=0
call_dir=${CURL_CALL_DIR:?}
count_file=$call_dir/count
n=0
if [ -f "$count_file" ]; then
  n=$(cat "$count_file")
fi
n=$((n + 1))
printf '%s\n' "$n" >"$count_file"
args_file=$call_dir/call-$n.args
headers_file=$call_dir/call-$n.headers
mode_file=$call_dir/call-$n.header_mode
: >"$args_file"
: >"$headers_file"
for arg in "$@"; do
  printf '%s\n' "$arg" >>"$args_file"
  printf '%s\n' "$arg" >>"${CURL_CALL_LOG:?}"
done
printf '%s\n' '---' >>"${CURL_CALL_LOG:?}"
while [ $# -gt 0 ]; do
  case $1 in
  -o | --output)
    out=$2
    shift 2
    ;;
  -w | --write-out)
    wfmt=$2
    shift 2
    ;;
  --max-redirs)
    max_redirs=$2
    shift 2
    ;;
  -H | --header)
    hval=$2
    case $hval in
    @*)
      hpath=${hval#@}
      stat -c '%a' "$hpath" >>"$mode_file"
      cat "$hpath" >>"$headers_file"
      ;;
    *)
      printf '%s\n' "$hval" >>"$headers_file"
      ;;
    esac
    shift 2
    ;;
  -K | --config)
    cpath=$2
    stat -c '%a' "$cpath" >>"$mode_file"
    cat "$cpath" >>"$headers_file"
    shift 2
    ;;
  --proto | --proto-redir | --connect-timeout | --max-time | --retry | --retry-delay | --retry-max-time)
    shift 2
    ;;
  -L | --location)
    has_L=1
    shift
    ;;
  --)
    shift
    break
    ;;
  -*)
    case $1 in
    -*L*) has_L=1 ;;
    esac
    shift
    ;;
  *)
    url=$1
    shift
    ;;
  esac
done
if [ -z "$url" ] && [ $# -gt 0 ]; then
  url=$1
fi
printf '%s\n' "$url" >"$call_dir/call-$n.url"
printf '%s\n' "$has_L" >"$call_dir/call-$n.has_L"
printf '%s\n' "$max_redirs" >"$call_dir/call-$n.max_redirs"

case "$url" in
https://api.github.com/repos/jdx/mise/releases/tags/*)
  if [ "${FAKE_API_FAIL:-}" = tags ]; then
    exit 22
  fi
  cat >"$out" <<JSON
{"url":"https://api.github.com/repos/jdx/mise/releases/1","author":{"url":"https://api.github.com/users/jdx"},"assets":[{"id":111,"name":"mise-${FAKE_MISE_VERSION}-linux-x64.tar.gz","url":"https://api.github.com/repos/jdx/mise/releases/assets/111","uploader":{"url":"https://api.github.com/users/actions"}},{"id":987654321,"uploader":{"url":"https://api.github.com/users/actions"},"url":"https://api.github.com/repos/jdx/mise/releases/assets/987654321","name":"mise-${FAKE_MISE_VERSION}-linux-x64"}]}
JSON
  ;;
https://api.github.com/repos/jdx/mise/releases/assets/987654321)
  if [ "${FAKE_API_FAIL:-}" = asset ]; then
    exit 22
  fi
  if [ -n "$wfmt" ]; then
    printf '302\nhttps://release-assets.githubusercontent.com/fake-presigned/mise?sig=abc\n'
  fi
  ;;
https://release-assets.githubusercontent.com/fake-presigned/mise?sig=abc | https://github.com/jdx/mise/releases/download/*)
  cp "${FAKE_MISE_PAYLOAD:?}" "$out"
  ;;
*)
  printf 'fake curl: unexpected URL: %s\n' "$url" >&2
  exit 22
  ;;
esac
CURL
  chmod 0755 "$dir/fakebin/uname" "$dir/fakebin/curl"
  : >"$dir/mise.log"
  : >"$dir/curl.log"
  : >"$dir/github_path"

  case "$name" in
  cached-matching-sha)
    mkdir -p "$dir/home/.local/bin"
    cp "$payload" "$dir/home/.local/bin/mise"
    chmod 0755 "$dir/home/.local/bin/mise"
    ;;
  cached-mismatched-sha)
    mkdir -p "$dir/home/.local/bin"
    printf '#!/bin/sh\nexit 0\n' >"$dir/home/.local/bin/mise"
    chmod 0755 "$dir/home/.local/bin/mise"
    test_path="$dir/home/.local/bin:$test_path"
    ;;
  github-token-two-step-no-jq)
    mkdir -p "$posix_bin"
    for cmd in awk sha256sum install mkdir mktemp rm tr cat dirname chmod stat cp; do
      if cmd_path=$(command -v "$cmd" 2>/dev/null); then
        ln -sf "$cmd_path" "$posix_bin/$cmd"
      fi
    done
    test_path="$dir/fakebin:$posix_bin"
    ;;
  esac

  env -i \
    HOME="$dir/home" \
    PATH="$test_path" \
    GITHUB_PATH="$dir/github_path" \
    GITHUB_TOKEN="$token" \
    MISE_CI_TOOLS="$tools" \
    MISE_CALL_LOG="$dir/mise.log" \
    CURL_CALL_LOG="$dir/curl.log" \
    CURL_CALL_DIR="$call_dir" \
    FAKE_MISE_PAYLOAD="$payload" \
    FAKE_MISE_VERSION="$MISE_BOOTSTRAP_VERSION" \
    FAKE_MISE_SOURCE="$source" \
    FAKE_MISE_WHERE_DIR="$dir/fake-tool-dir" \
    FAKE_API_FAIL="$api_fail" \
    "$bash_bin" "$dir/ci-setup-mise.sh" >"$dir/stdout" 2>"$dir/stderr" || rc=$?

  if [[ $rc -ne $expected_rc ]]; then
    fail "$name: exit $rc, want $expected_rc; stderr: $(cat "$dir/stderr")"
    return
  fi
  if [[ -f $call_dir/count ]]; then
    count=$(cat "$call_dir/count")
  fi
  if [[ $name == matching-sha ]]; then
    for flag in --proto --tlsv1.2 --proto-redir --retry --retry-all-errors --retry-max-time --connect-timeout --max-time; do
      grep -Fxq -- "$flag" "$dir/curl.log" || fail "$name: curl missing required flag $flag"
    done
    ! grep -Fxq -- '--retry-delay' "$dir/curl.log" || fail "$name: curl must not set fixed --retry-delay"
    grep -Fxq -- "$dir/home/.local/bin" "$dir/github_path" ||
      fail "$name: expected $dir/home/.local/bin in GITHUB_PATH"
  fi
  if [[ $name == cached-matching-sha ]]; then
    if ((count != 0)); then
      fail "$name: expected 0 curl calls on verified cache hit, got $count"
    fi
    grep -Fxq -- "$dir/home/.local/bin" "$dir/github_path" ||
      fail "$name: expected $dir/home/.local/bin in GITHUB_PATH on cache hit"
    grep -q '^mise install' "$dir/mise.log" ||
      fail "$name: expected mise install to run after verified cache hit"
  fi
  if [[ $name == cached-mismatched-sha ]]; then
    if ((count == 0)); then
      fail "$name: expected curl download after cached sha256 mismatch"
    fi
    cmp -s "$payload" "$dir/home/.local/bin/mise" ||
      fail "$name: cached mise binary was not replaced with verified download"
    grep -Fxq -- "$dir/home/.local/bin" "$dir/github_path" ||
      fail "$name: expected $dir/home/.local/bin in GITHUB_PATH after re-download"
  fi
  if [[ $name == github-token-two-step || $name == github-token-two-step-no-jq ]]; then
    if ((count != 3)); then
      fail "$name: expected 3 curl calls for two-step API download, got $count"
      return
    fi
    ! grep -Fq -- "$token" "$dir/curl.log" ||
      fail "$name: GITHUB_TOKEN leaked into curl argv"
    for i in 1 2 3; do
      for flag in --proto --tlsv1.2 --proto-redir --retry --retry-all-errors --retry-max-time --connect-timeout --max-time; do
        grep -Fxq -- "$flag" "$call_dir/call-$i.args" ||
          fail "$name: call $i missing required flag $flag"
      done
      ! grep -Fxq -- '--retry-delay' "$call_dir/call-$i.args" ||
        fail "$name: call $i must not set fixed --retry-delay"
    done
    [[ $(cat "$call_dir/call-1.url") == "https://api.github.com/repos/jdx/mise/releases/tags/${MISE_BOOTSTRAP_VERSION}" ]] ||
      fail "$name: call 1 unexpected URL: $(cat "$call_dir/call-1.url")"
    [[ $(cat "$call_dir/call-1.has_L") == 0 && $(cat "$call_dir/call-1.max_redirs") == 0 ]] ||
      fail "$name: call 1 must disable redirects"
    [[ $(cat "$call_dir/call-1.header_mode") == 600 ]] ||
      fail "$name: call 1 header file must be 0600, got: $(cat "$call_dir/call-1.header_mode" 2>/dev/null || true)"
    grep -Fxq -- "Authorization: Bearer $token" "$call_dir/call-1.headers" ||
      fail "$name: call 1 missing Bearer token header"

    [[ $(cat "$call_dir/call-2.url") == "https://api.github.com/repos/jdx/mise/releases/assets/987654321" ]] ||
      fail "$name: call 2 unexpected URL: $(cat "$call_dir/call-2.url")"
    [[ $(cat "$call_dir/call-2.has_L") == 0 && $(cat "$call_dir/call-2.max_redirs") == 0 ]] ||
      fail "$name: call 2 must disable redirects"
    [[ $(cat "$call_dir/call-2.header_mode") == 600 ]] ||
      fail "$name: call 2 header file must be 0600, got: $(cat "$call_dir/call-2.header_mode" 2>/dev/null || true)"
    grep -Fxq -- "Authorization: Bearer $token" "$call_dir/call-2.headers" ||
      fail "$name: call 2 missing Bearer token header"
    grep -Fxq -- "Accept: application/octet-stream" "$call_dir/call-2.headers" ||
      fail "$name: call 2 missing octet-stream Accept header"

    [[ $(cat "$call_dir/call-3.url") == "https://release-assets.githubusercontent.com/fake-presigned/mise?sig=abc" ]] ||
      fail "$name: call 3 unexpected URL: $(cat "$call_dir/call-3.url")"
    [[ $(cat "$call_dir/call-3.has_L") == 1 ]] ||
      fail "$name: call 3 expected -L for CDN download"
    [[ ! -s $call_dir/call-3.headers ]] ||
      fail "$name: call 3 CDN request must not send any headers (found: $(cat "$call_dir/call-3.headers"))"
    ! grep -Eq '^(-H|--header|-K|--config)$' "$call_dir/call-3.args" ||
      fail "$name: call 3 CDN request must not pass -H/-K flags"

    cmp -s "$payload" "$dir/home/.local/bin/mise" ||
      fail "$name: installed mise binary does not match payload"
    grep -Fxq -- "$dir/home/.local/bin" "$dir/github_path" ||
      fail "$name: expected $dir/home/.local/bin in GITHUB_PATH"
  fi
  if [[ $name == github-token-api-fallback ]]; then
    if ((count != 2)); then
      fail "$name: expected 2 curl calls (failed API + direct fallback), got $count"
      return
    fi
    [[ $(cat "$call_dir/call-2.url") == https://github.com/jdx/mise/releases/download/* ]] ||
      fail "$name: fallback call did not use direct release URL: $(cat "$call_dir/call-2.url")"
    [[ ! -s $call_dir/call-2.headers ]] ||
      fail "$name: direct fallback must not send Authorization headers"
    cmp -s "$payload" "$dir/home/.local/bin/mise" ||
      fail "$name: installed mise binary does not match payload"
  fi
  if [[ $expected_rc -ne 0 ]]; then
    ! grep -q '^mise install' "$dir/mise.log" || fail "$name: mise install ran despite the failing check"
  fi
  if [[ $name == wrong-sha || $name == github-token-api-fallback-wrong-sha ]]; then
    [[ ! -s $dir/mise.log ]] || fail "$name: trust/install ran despite the failing checksum"
    [[ ! -e $dir/home/.local/bin/mise ]] || fail "$name: mise was installed despite the failing checksum"
  fi
  if [[ $expected_rc -eq 0 ]]; then
    while IFS= read -r call; do
      [[ $call =~ ^mise[[:space:]]+(exec|x)[[:space:]]+([^-][^[:space:]]*([[:space:]]+[^-][^[:space:]]*)*)[[:space:]]+--([[:space:]]|$) ]] ||
        fail "$name: mise exec must explicitly name tool(s) before '--', got: $call"
    done < <(grep -E '^mise (exec|x)([[:space:]]|$)' "$dir/mise.log" || true)
  fi
  if [[ $name == explicit-exec-go-and-pnpm ]]; then
    grep -Fxq -- 'mise exec go -- go env GOVERSION' "$dir/mise.log" ||
      fail "$name: expected explicit 'mise exec go -- go env GOVERSION', got: $(cat "$dir/mise.log")"
    grep -Fxq -- "mise exec go -- go version $dir/fake-tool-dir/bin/dummy-tool" "$dir/mise.log" ||
      fail "$name: expected explicit 'mise exec go -- go version ...', got: $(cat "$dir/mise.log")"
    grep -Fxq -- "mise exec pnpm -- pnpm config set store-dir $dir/home/.pnpm-store" "$dir/mise.log" ||
      fail "$name: expected explicit 'mise exec pnpm -- pnpm config set ...', got: $(cat "$dir/mise.log")"
  fi
  if [[ $name == explicit-exec-pinned-pnpm ]]; then
    grep -Fxq -- "mise exec pnpm@1.0.0 -- pnpm config set store-dir $dir/home/.pnpm-store" "$dir/mise.log" ||
      fail "$name: expected explicit 'mise exec pnpm@1.0.0 -- pnpm config set ...', got: $(cat "$dir/mise.log")"
  fi
}

run_case matching-sha "$payload_sha" 0 # 对照组：证明失败不是夹具本身出的问题
run_case wrong-sha "$zero_sha" 1
# `mise install <名字>` 对没配置的名字会静默装 latest。
run_case unpinned-tool "$payload_sha" 1 '[none]'
run_case explicit-exec-go-and-pnpm "$payload_sha" 0 "$root/mise.toml" 'go go:example.com/tool pnpm'
run_case explicit-exec-pinned-pnpm "$payload_sha" 0 "$root/mise.toml" 'pnpm@1.0.0 node@1.0.0'
run_case cached-matching-sha "$payload_sha" 0
run_case cached-mismatched-sha "$payload_sha" 0
FAKE_GITHUB_TOKEN=test-secret-token-value run_case github-token-two-step "$payload_sha" 0
FAKE_GITHUB_TOKEN=test-secret-token-value run_case github-token-two-step-no-jq "$payload_sha" 0
FAKE_GITHUB_TOKEN=test-secret-token-value FAKE_API_FAIL=tags run_case github-token-api-fallback "$payload_sha" 0
FAKE_GITHUB_TOKEN=test-secret-token-value FAKE_API_FAIL=tags run_case github-token-api-fallback-wrong-sha "$zero_sha" 1

if ((failures > 0)); then
  printf 'ci-setup-mise test: %d failure(s)\n' "$failures" >&2
  exit 1
fi
printf 'PASS: ci-setup-mise enforces sha256 verification, pinned tools in mise.toml, and explicit tools in mise exec\n'
