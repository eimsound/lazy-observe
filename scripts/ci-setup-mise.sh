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
if [[ -n ${MISE_CI_TOOLS:-} ]]; then
  read -r -a ci_tools <<<"$MISE_CI_TOOLS"
else
  case "$profile" in
  base)
    ci_tools=(go node pnpm jq)
    ;;
  go-ci)
    ci_tools=(
      go
      rg
      actionlint@1.7.12
      golangci-lint@2.12.2
      go:github.com/golangci/golines@0.15.0
      go:github.com/google/go-licenses/v2@2.0.1
      go:golang.org/x/vuln/cmd/govulncheck@1.9.0
      helm@3.21.3
    )
    ;;
  meta-pg)
    ci_tools=(go)
    ;;
  sdk-python)
    ci_tools=(uv@0.13.7)
    ;;
  # SDK journeys：服务端构建（go、web 的 node/pnpm）、journey 的 jq/rg 与 sdk-python 任务的 uv（解释器由 uv 取）。
  sdk-journeys)
    ci_tools=(go node pnpm jq rg uv@0.13.7)
    ;;
  # 性能门禁（bench.yml、release.yml 的 performance-gates）：base 的工具加 uv；
  # check-release-smoke.sh 的内嵌校验脚本用 uv 管理的固定版本 Python 跑，不用系统 python3。
  perf-gates)
    ci_tools=(go node pnpm jq uv@0.13.7)
    ;;
  release)
    ci_tools=(
      go node pnpm jq
      goreleaser@2.17.0
      go:github.com/google/go-licenses/v2@2.0.1
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
"$mise_bin" install "${ci_tools[@]}"

for tool in "${ci_tools[@]}"; do
  if [[ $tool == pnpm || $tool == pnpm@* ]]; then
    "$mise_bin" exec -- pnpm config set store-dir "${HOME}/.pnpm-store"
    break
  fi
done
