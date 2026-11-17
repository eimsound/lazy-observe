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

# Only hash mise.toml: the installed toolset is defined by [tools], not by
# ci-setup-mise.sh. Keeping the setup script out of the key means editing it
# (e.g. bumping a ci_tool or tweaking auth) no longer invalidates the mise
# cache; any genuinely new tool still gets installed on restore via
# `mise install`, and the restore-keys prefix fallback covers the rest.
mise_hash=$(hash_files mise mise.toml)
go_hash=$(hash_files go "${go_manifests[@]}")
storage_hash=$(hash_files storage \
  scripts/download-storage-bins.sh \
  scripts/prepare-embedded-storage.sh)
pnpm_web_hash=$(hash_files pnpm-web web/pnpm-lock.yaml)

output=${GITHUB_OUTPUT:-/dev/stdout}
{
  printf 'mise=%s\n' "$mise_hash"
  printf 'go=%s\n' "$go_hash"
  printf 'storage=%s\n' "$storage_hash"
  printf 'pnpm-web=%s\n' "$pnpm_web_hash"
} >>"$output"
