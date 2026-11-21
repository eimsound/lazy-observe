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
  local dir=$work/case-$name rc=0 call
  mkdir -p "$dir/fakebin" "$dir/home" "$dir/fake-tool-dir/bin"
  : >"$dir/fake-tool-dir/bin/dummy-tool"
  cp "$script" "$dir/ci-setup-mise.sh"
  printf 'MISE_BOOTSTRAP_VERSION=%s\nMISE_BOOTSTRAP_SHA256_AMD64=%s\nMISE_BOOTSTRAP_SHA256_ARM64=%s\n' \
    "$MISE_BOOTSTRAP_VERSION" "$sha" "$sha" >"$dir/mise-release.env"
  printf '#!/bin/sh\nprintf "x86_64\\n"\n' >"$dir/fakebin/uname"
  cat >"$dir/fakebin/curl" <<'CURL'
#!/bin/sh
out=
for arg in "$@"; do
  printf '%s\n' "$arg" >>"${CURL_CALL_LOG:?}"
done
while [ $# -gt 0 ]; do
  case $1 in
  -o) out=$2; shift 2 ;;
  *) shift ;;
  esac
done
cp "$FAKE_MISE_PAYLOAD" "$out"
CURL
  chmod 0755 "$dir/fakebin/uname" "$dir/fakebin/curl"
  : >"$dir/mise.log"
  : >"$dir/curl.log"
  : >"$dir/github_path"

  env -i \
    HOME="$dir/home" \
    PATH="$dir/fakebin:/usr/bin:/bin" \
    GITHUB_PATH="$dir/github_path" \
    MISE_CI_TOOLS="$tools" \
    MISE_CALL_LOG="$dir/mise.log" \
    CURL_CALL_LOG="$dir/curl.log" \
    FAKE_MISE_PAYLOAD="$payload" \
    FAKE_MISE_SOURCE="$source" \
    FAKE_MISE_WHERE_DIR="$dir/fake-tool-dir" \
    "$bash_bin" "$dir/ci-setup-mise.sh" >"$dir/stdout" 2>"$dir/stderr" || rc=$?

  if [[ $rc -ne $expected_rc ]]; then
    fail "$name: exit $rc, want $expected_rc; stderr: $(cat "$dir/stderr")"
    return
  fi
  if [[ $name == matching-sha ]]; then
    for flag in --retry --retry-delay --retry-all-errors --connect-timeout --max-time; do
      grep -Fxq -- "$flag" "$dir/curl.log" || fail "$name: curl missing transient retry flag $flag"
    done
  fi
  if [[ $expected_rc -ne 0 ]]; then
    ! grep -q '^mise install' "$dir/mise.log" || fail "$name: mise install ran despite the failing check"
  fi
  if [[ $name == wrong-sha ]]; then
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

if ((failures > 0)); then
  printf 'ci-setup-mise test: %d failure(s)\n' "$failures" >&2
  exit 1
fi
printf 'PASS: ci-setup-mise enforces sha256 verification, pinned tools in mise.toml, and explicit tools in mise exec\n'
