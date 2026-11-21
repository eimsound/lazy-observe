#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

hash_files() {
  local label=$1
  shift

  if (($# == 0)); then
    printf 'no files found for %s cache key\n' "$label" >&2
    return 1
  fi

  local file
  for file in "$@"; do
    if [[ ! -f $file ]]; then
      printf 'missing %s cache key input: %s\n' "$label" "$file" >&2
      return 1
    fi
  done

  sha256sum -- "$@" | sha256sum | awk '{print $1}'
}

mapfile -d '' go_manifests < <(
  find . -type f \
    \( -name go.mod -o -name go.sum -o -name entc.mod -o -name entc.sum \) \
    -not -path './.git/*' -print0 | sort -z
)

# 只对 mise.toml 的 [tools] 表算哈希（去掉整行/行尾注释与空白行）：改 [tasks.*] 或注释不改变安装的工具集，
# 不应该让所有 mise-* 缓存整批失效；脚本本身（ci-setup-mise.sh）同样不进 key。
mise_tools=$(awk '
  function strip_comment(line,    i, c, quote, out) {
    quote = ""
    out = ""
    for (i = 1; i <= length(line); i++) {
      c = substr(line, i, 1)
      if (quote != "") {
        out = out c
        if (c == "\\" && quote == "\"" && i < length(line)) {
          i++
          out = out substr(line, i, 1)
        } else if (c == quote) {
          quote = ""
        }
      } else if (c == "\"" || c == "\x27") {
        quote = c
        out = out c
      } else if (c == "#") {
        break
      } else {
        out = out c
      }
    }
    sub(/^[[:space:]]+/, "", out)
    sub(/[[:space:]]+$/, "", out)
    return out
  }
  /^[[:space:]]*\[tools\][[:space:]]*(#.*)?$/ { in_tools = 1; next }
  /^[[:space:]]*\[/ { in_tools = 0 }
  in_tools {
    cleaned = strip_comment($0)
    if (cleaned != "") print cleaned
  }
' mise.toml)
if [[ -z $mise_tools ]]; then
  printf 'cannot read the [tools] table from mise.toml\n' >&2
  exit 1
fi
mise_hash=$(printf '%s\n' "$mise_tools" | sha256sum | awk '{print $1}')
mise_bin_hash=$(hash_files mise-bin scripts/mise-release.env)
go_hash=$(hash_files go "${go_manifests[@]}")

# 从已去注释的 [tools] 条目里提取工具版本，兼容单/双引号、等号两侧空白和内联表（{ version = "..." }）。
extract_tool_version() {
  local tool=$1
  awk -v tool="$tool" '
    function unquote(s,    q) {
      sub(/^[[:space:]]+/, "", s)
      sub(/[[:space:]]+$/, "", s)
      q = substr(s, 1, 1)
      if ((q == "\"" || q == "\x27") && length(s) >= 2 && substr(s, length(s), 1) == q) {
        return substr(s, 2, length(s) - 2)
      }
      return ""
    }
    {
      eq = index($0, "=")
      if (eq == 0) next
      key = substr($0, 1, eq - 1)
      sub(/^[[:space:]]+/, "", key)
      sub(/[[:space:]]+$/, "", key)
      if (key == "\"" tool "\"" || key == "\x27" tool "\x27") key = tool
      if (key != tool) next
      val = substr($0, eq + 1)
      sub(/^[[:space:]]+/, "", val)
      sub(/[[:space:]]+$/, "", val)
      if (substr(val, 1, 1) == "{") {
        if (match(val, /(^|[{,[[:space:]])version[[:space:]]*=[[:space:]]*("[^"]*"|\x27[^\x27]*\x27)/)) {
          part = substr(val, RSTART, RLENGTH)
          sub(/^.*version[[:space:]]*=[[:space:]]*/, "", part)
          print unquote(part)
        }
      } else {
        print unquote(val)
      }
      exit
    }
  ' <<<"$mise_tools"
}

# Go 与 pnpm 的工具版本单独输出，分别供 gobuild 与 mise-sdk-node 缓存 key 使用：
# 其它工具（helm、rg、golangci-lint……）升版不该让 gobuild 或 Node SDK 的 mise 缓存换新；
# Go 自己升版必须换 key 且不跨版本回退，避免把旧版本编出来的 GOCACHE 拖进新缓存。
go_toolchain=$(extract_tool_version go)
if [[ ! $go_toolchain =~ ^[0-9]+(\.[0-9]+)+$ ]]; then
  printf 'cannot read the go toolchain version from the [tools] table of mise.toml (got: %s)\n' "$go_toolchain" >&2
  exit 1
fi
pnpm_toolchain=$(extract_tool_version pnpm)
if [[ ! $pnpm_toolchain =~ ^[0-9]+(\.[0-9]+)+$ ]]; then
  printf 'cannot read the pnpm toolchain version from the [tools] table of mise.toml (got: %s)\n' "$pnpm_toolchain" >&2
  exit 1
fi
storage_hash=$(hash_files storage \
  scripts/download-storage-bins.sh \
  scripts/prepare-embedded-storage.sh)
pnpm_web_hash=$(hash_files pnpm-web web/pnpm-lock.yaml)

playwright_lock=sdk/browser/pnpm-lock.yaml
if [[ ! -f $playwright_lock ]]; then
  printf 'missing playwright cache key input: %s\n' "$playwright_lock" >&2
  exit 1
fi
# pnpm 12 lockfile 含多文档 YAML，从 packages: 区块提取解析后的 playwright-core 精确版本；
# 仅当匹配到唯一且合法的语义化版本时放行，否则 fail-closed 退出非零。
mapfile -t playwright_versions < <(awk '
  /^packages:[[:space:]]*$/ { in_packages = 1; next }
  /^[^[:space:]#]/ { in_packages = 0 }
  in_packages && /^  ["'\'']?playwright-core@[^[:space:]"'\'':]+["'\'']?:[[:space:]]*$/ {
    ver = $1
    gsub(/^["'\'']|["'\'']?:$/, "", ver)
    sub(/^playwright-core@/, "", ver)
    versions[ver] = 1
  }
  END {
    for (v in versions) print v
  }
' "$playwright_lock" | sort)
if ((${#playwright_versions[@]} != 1)) || [[ ! ${playwright_versions[0]} =~ ^[0-9]+(\.[0-9]+)+(-[0-9A-Za-z.-]+)?$ ]]; then
  printf 'cannot read a unique playwright-core version from %s (got: %s)\n' \
    "$playwright_lock" "${playwright_versions[*]:-<none>}" >&2
  exit 1
fi
playwright_sdk_browser=${playwright_versions[0]}

output=${GITHUB_OUTPUT:-/dev/stdout}
{
  printf 'mise=%s\n' "$mise_hash"
  printf 'mise-bin=%s\n' "$mise_bin_hash"
  printf 'go=%s\n' "$go_hash"
  printf 'go-toolchain=%s\n' "$go_toolchain"
  printf 'pnpm-toolchain=%s\n' "$pnpm_toolchain"
  printf 'storage=%s\n' "$storage_hash"
  printf 'pnpm-web=%s\n' "$pnpm_web_hash"
  printf 'playwright-sdk-browser=%s\n' "$playwright_sdk_browser"
} >>"$output"
