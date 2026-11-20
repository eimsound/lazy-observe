#!/usr/bin/env bash
set -euo pipefail

# GitHub's unauthenticated API is rate-limited to 60 req/h, which 403s aqua's
# version-tag resolution during `mise install`. Workflows pass the job's
# GITHUB_TOKEN to raise the limit. Without a token (local runs) we fall back to
# disabling sigstore attestation (checksums are still verified) so installs can
# still proceed via direct release downloads on the remaining quota.
if [[ -z ${GITHUB_TOKEN:-} ]]; then
  export AQUA_DISABLE_ATTESTATION=1
fi

# runner 上没有 mise 时，从 GitHub 的不可变 release 下载固定版本并校验 sha256；本机已装 mise 时直接复用。
#
# 版本与两个架构的 sha256 只在 scripts/mise-release.env 维护，Dockerfile 的 builder 阶段读同一份文件。
# 升级时先 `gh release view -R jdx/mise <tag> --json assets` 确认资产名，再改那一个文件。
# 不用上游安装脚本：它本身不固定、也不做校验。
if command -v mise >/dev/null 2>&1; then
  mise_bin=$(command -v mise)
  printf 'mise already available at %s; skipping bootstrap\n' "$mise_bin"
else
  mise_release_env=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/mise-release.env
  # shellcheck source=scripts/mise-release.env
  . "$mise_release_env"

  case "$(uname -m)" in
  x86_64 | amd64)
    mise_asset=mise-${MISE_BOOTSTRAP_VERSION}-linux-x64
    mise_sha256=$MISE_BOOTSTRAP_SHA256_AMD64
    ;;
  aarch64 | arm64)
    mise_asset=mise-${MISE_BOOTSTRAP_VERSION}-linux-arm64
    mise_sha256=$MISE_BOOTSTRAP_SHA256_ARM64
    ;;
  *)
    printf 'unsupported host architecture for the pinned mise: %s\n' "$(uname -m)" >&2
    exit 1
    ;;
  esac

  install_dir=${HOME}/.local/bin
  mise_bin=${install_dir}/mise

  mkdir -p "$install_dir"
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$work/$mise_asset" \
    "https://github.com/jdx/mise/releases/download/${MISE_BOOTSTRAP_VERSION}/${mise_asset}"
  printf '%s  %s\n' "$mise_sha256" "$work/$mise_asset" | sha256sum --check --strict -
  install -m 0755 "$work/$mise_asset" "$mise_bin"
  rm -rf "$work"
  trap - EXIT

  if [[ -z ${GITHUB_PATH:-} ]]; then
    printf 'GITHUB_PATH is not set; this setup script must run inside GitHub Actions\n' >&2
    exit 1
  fi
  printf '%s\n' "$install_dir" >>"$GITHUB_PATH"
fi

"$mise_bin" trust "$PWD/mise.toml"

profile=${MISE_CI_PROFILE:-base}
# profile 只写工具名；版本由 mise 从 mise.toml 的 [tools] 解析（`mise install <名字>` 装配置里的那一版，
# 不是 latest），这里不再写版本。要给某个 job 装别的版本，用 MISE_CI_TOOLS 写 `node@22.23.5` 这样的精确版本。
# mise 对不在配置里的名字会静默装 latest，所以下面的 verify_pins 在安装前后各查一遍，不满足就退出 1。
tools_from_env=0
if [[ -n ${MISE_CI_TOOLS:-} ]]; then
  read -r -a ci_tools <<<"$MISE_CI_TOOLS"
  tools_from_env=1
else
  case "$profile" in
  # base 含 uv：build 的 check-embedded-artifact.sh 与 e2e-ha 的 e2e-multi-replica.sh 用 uv 受管的 Python（scripts/lib/uv-python.sh），不用系统 python3。
  base)
    ci_tools=(go node pnpm jq uv)
    ;;
  go-ci)
    ci_tools=(
      go
      rg
      actionlint
      golangci-lint
      go:github.com/golangci/golines
      go:github.com/google/go-licenses/v2
      go:golang.org/x/vuln/cmd/govulncheck
      helm
    )
    ;;
  meta-pg)
    ci_tools=(go)
    ;;
  sdk-python)
    ci_tools=(uv)
    ;;
  # SDK journeys：服务端构建（go、web 的 node/pnpm）、journey 的 jq/rg 与 sdk-python 任务的 uv（解释器由 uv 取）。
  sdk-journeys)
    ci_tools=(go node pnpm jq rg uv)
    ;;
  # 性能门禁（bench.yml、release.yml 的 performance-gates）：base 的工具加 uv；
  # check-release-smoke.sh 的内嵌校验脚本用 uv 管理的固定版本 Python 跑，不用系统 python3。
  perf-gates)
    ci_tools=(go node pnpm jq uv)
    ;;
  # release：.goreleaser.yaml 的 before hook 跑 dist-cross 与 gate-release-artifacts，
  # 其中 check-embedded-artifact.sh、check-release-smoke.sh 用 uv 受管的 Python（scripts/lib/uv-python.sh）。
  release)
    ci_tools=(
      go node pnpm jq uv
      goreleaser
      go:github.com/google/go-licenses/v2
    )
    ;;
  *)
    printf 'unknown MISE_CI_PROFILE: %s\n' "$profile" >&2
    exit 1
    ;;
  esac
fi
if ((${#ci_tools[@]} == 0)); then
  printf 'MISE_CI_TOOLS must select at least one tool\n' >&2
  exit 1
fi

# 供应链闸（fail-closed）：每个请求都要落到一个精确版本，否则 CI 会装上未固定的 latest。
# - 裸名字：版本必须来自本仓库 mise.toml 的 [tools]（不认环境变量 MISE_<TOOL>_VERSION、mise.local.toml、全局配置），
#   且是精确版本（不认 latest、`26` 这类前缀）。
# - 名字@版本：只允许出现在 MISE_CI_TOOLS（如 SDK 的 Node 矩阵），版本必须精确；profile 只写名字。
# - 同一工具只能请求一次：mise 对同名请求只装一个版本。
# 安装前查一遍（不满足就不装），安装后再查装上的就是那一版。mise 没有「拒装未配置工具」的选项
# （--locked 只要求 lockfile 里有下载 URL，本仓库没有 lockfile），所以在这里查。
# 只用 mise 自己的单字段输出，不依赖 jq；输出格式一旦变化只会让这里判不匹配而失败，不会放行。
config_file=$PWD/mise.toml
# `mise tool --config-source` 把 $HOME 显示成 ~，两种写法都认。
config_file_display=$config_file
if [[ $config_file == "$HOME"/* ]]; then
  config_file_display="~${config_file#"$HOME"}"
fi
exact_version_re='^v?[0-9]+(\.[0-9]+)+([-+][0-9A-Za-z.+-]+)?$'
pin_names=()
pin_versions=()
verify_pins() { # phase: before|after
  local phase=$1 spec name version source installed found failures=0 i v
  if [[ $phase == before ]]; then
    for spec in "${ci_tools[@]}"; do
      # name@version；`npm:@scope/pkg` 这种名字里的 @ 紧跟在 `:` 或 `/` 后面，不算版本分隔符。
      if [[ $spec =~ ^(.*[^:/])@([^@/]+)$ ]]; then
        name=${BASH_REMATCH[1]}
        version=${BASH_REMATCH[2]}
        if ((!tools_from_env)); then
          printf 'ci-setup-mise: profile %s pins %s; profiles list tool names only, the version comes from mise.toml\n' \
            "$profile" "$spec" >&2
          failures=$((failures + 1))
          continue
        fi
      else
        name=$spec
        if ! source=$("$mise_bin" tool "$name" --config-source) ||
          ! version=$("$mise_bin" tool "$name" --requested); then
          printf 'ci-setup-mise: cannot resolve %s with mise tool\n' "$name" >&2
          failures=$((failures + 1))
          continue
        fi
        if [[ $source != "$config_file" && $source != "$config_file_display" ]]; then
          printf 'ci-setup-mise: %s is not pinned in %s [tools] (config source: %s, requested: %s); add it there first\n' \
            "$name" "$config_file" "$source" "$version" >&2
          failures=$((failures + 1))
          continue
        fi
      fi
      if [[ ! $version =~ $exact_version_re ]]; then
        printf 'ci-setup-mise: %s resolves to %s, which is not an exact version\n' "$name" "${version:-<none>}" >&2
        failures=$((failures + 1))
        continue
      fi
      for i in "${!pin_names[@]}"; do
        if [[ ${pin_names[i]} == "$name" ]]; then
          # `mise install node node@22.23.5` 只装其中一个版本，另一个请求会被悄悄丢掉。
          printf 'ci-setup-mise: %s is requested more than once; mise installs only one version per tool\n' "$name" >&2
          failures=$((failures + 1))
          continue 2
        fi
      done
      pin_names+=("$name")
      pin_versions+=("$version")
    done
  else
    for i in "${!pin_names[@]}"; do
      name=${pin_names[i]}
      version=${pin_versions[i]#v}
      installed=$("$mise_bin" tool "$name" --installed) || installed=
      found=0
      for v in $installed; do
        [[ ${v#v} == "$version" ]] && found=1
      done
      if ((!found)); then
        printf 'ci-setup-mise: %s %s is not installed after mise install (installed: %s)\n' \
          "$name" "$version" "${installed:-none}" >&2
        failures=$((failures + 1))
      fi
    done
  fi
  if ((failures > 0)); then
    printf 'ci-setup-mise: %d tool pin check(s) failed (%s install)\n' "$failures" "$phase" >&2
    exit 1
  fi
}

verify_pins before
"$mise_bin" install "${ci_tools[@]}"
verify_pins after
printf 'ci-setup-mise: installed pinned tools:'
for i in "${!pin_names[@]}"; do
  printf ' %s@%s' "${pin_names[i]}" "${pin_versions[i]}"
done
printf '\n'

# go 后端工具（go:…）是 `go install` 用安装当时的 Go 编出来的；CI 的 mise 缓存按前缀恢复，升 go 后会复用旧 Go 编的二进制。
# 旧 Go 的 go/types 读不了新标准库（govulncheck 报 "file requires newer Go version go1.27 (application built with go1.26)"），
# 所以编译它的 Go 与当前 go 不一致就强制重装。
go_version=
for tool in "${ci_tools[@]}"; do
  [[ $tool == go:* ]] || continue
  [[ -n $go_version ]] || go_version=$("$mise_bin" exec -- go env GOVERSION)
  tool_dir=$("$mise_bin" where "$tool")
  built=
  rebuild=0
  for bin in "$tool_dir"/bin/*; do
    [[ -f $bin ]] || continue
    built=$("$mise_bin" exec -- go version "$bin" | awk '{print $NF}')
    [[ $built == "$go_version" ]] || rebuild=1
  done
  if ((rebuild)); then
    printf 'ci-setup-mise: %s was built with %s, current go is %s; reinstalling\n' "$tool" "$built" "$go_version"
    "$mise_bin" install --force "$tool"
  fi
done

for tool in "${ci_tools[@]}"; do
  if [[ $tool == pnpm || $tool == pnpm@* ]]; then
    "$mise_bin" exec -- pnpm config set store-dir "${HOME}/.pnpm-store"
    break
  fi
done
