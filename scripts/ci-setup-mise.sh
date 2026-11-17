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

# runner 上没有 mise 时从 mise.run 引导；本机已装 mise 时直接复用。
if command -v mise >/dev/null 2>&1; then
  mise_bin=$(command -v mise)
  printf 'mise already available at %s; skipping bootstrap\n' "$mise_bin"
else
  mise_version=${MISE_CI_VERSION:-v2026.5.3}
  install_dir=${HOME}/.local/bin
  mise_bin=${install_dir}/mise

  mkdir -p "$install_dir"
  curl --proto '=https' --tlsv1.2 -fsSL https://mise.run |
    MISE_VERSION="$mise_version" MISE_INSTALL_PATH="$mise_bin" sh

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
      helm@3.21.3
    )
    ;;
  meta-pg)
    ci_tools=(go)
    ;;
  sdk-python)
    ci_tools=(python@3.13.5)
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
