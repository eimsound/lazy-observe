#!/usr/bin/env bash
set +x
set -euo pipefail

usage() {
  printf 'usage: %s [--snapshot]\n' "${0##*/}" >&2
}

fail() {
  printf 'release prerequisite check failed: %s\n' "$*" >&2
  exit 1
}

require_nonblank_env() {
  local name=$1 value=
  if [[ -v $name ]]; then
    value=${!name}
  fi
  [[ $value =~ [^[:space:]] ]] || fail "$name must be set and non-empty"
}

mode=release
case $# in
  0) ;;
  1)
    [[ $1 == --snapshot ]] || { usage; exit 2; }
    mode=snapshot
    ;;
  *)
    usage
    exit 2
    ;;
esac

# GoReleaser publishes the GitHub Release with GITHUB_TOKEN (the workflow's
# job token with contents: write).
if [[ $mode == release ]]; then
  require_nonblank_env GITHUB_TOKEN
fi

# Prerequisite validation never needs credential values. Remove them before
# invoking path, Git, license, or GoReleaser subprocesses.
unset \
  GITHUB_TOKEN \
  GH_TOKEN \
  GORELEASER_FORCE_TOKEN

root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$root"

required_files=(.goreleaser.yaml Dockerfile LICENSE README.md)
for file in "${required_files[@]}"; do
  [[ -f $file ]] || fail "missing required release file: $file"
done

# A snapshot template derived from .Version (for example through incpatch)
# fails before any artifact is built when HEAD has no reachable SemVer tag (a
# fresh clone, a fork, or the first release). Keep snapshot identities
# independent from Git tags.
if ! grep -Fqx '  version_template: "0.0.0-SNAPSHOT-{{ .ShortCommit }}"' .goreleaser.yaml; then
  fail "snapshot version_template must be independent from repository tags"
fi

check_goreleaser_config() {
  local goreleaser_bin
  goreleaser_bin=$(command -v goreleaser) || fail "goreleaser is not available in PATH"

  # Configuration validation does not require publishing credentials. Keep them
  # out of this subprocess so a validator, plugin, or wrapper cannot observe them.
  if ! env \
      -u GITHUB_TOKEN \
      -u GH_TOKEN \
      -u GORELEASER_FORCE_TOKEN \
      "$goreleaser_bin" check; then
    fail "GoReleaser configuration validation failed"
  fi
}

if [[ $mode == snapshot ]]; then
  check_goreleaser_config
  printf 'PASS: snapshot release configuration is valid\n'
  exit 0
fi

git rev-parse --verify 'HEAD^{commit}' >/dev/null 2>&1 || \
  fail "release mode requires a Git repository with a HEAD commit"

exact_tags=()
# 只看服务端 v* tag：同一提交上的 SDK tag（sdk/go/v*、sdk-*-v*）不参与服务端发版。
mapfile -t exact_tags < <(git tag --points-at HEAD -l 'v*')
case ${#exact_tags[@]} in
  0) fail "HEAD must have exactly one release tag; found none" ;;
  1) ;;
  *) fail "HEAD must have exactly one release tag; found ${#exact_tags[@]}" ;;
esac
tag=${exact_tags[0]}

# SemVer 2.0 core numbers cannot contain leading zeroes. Prerelease identifiers
# are dot-separated ASCII alphanumerics/hyphens; numeric identifiers likewise
# cannot contain leading zeroes. Build metadata is intentionally not accepted by
# the release-tag contract.
semver_number='(0|[1-9][0-9]*)'
semver_prerelease_nonnumeric='[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*'
semver_prerelease_identifier="(${semver_number}|${semver_prerelease_nonnumeric})"
release_tag_re="^v${semver_number}\\.${semver_number}\\.${semver_number}(-${semver_prerelease_identifier}(\\.${semver_prerelease_identifier})*)?$"
[[ $tag =~ $release_tag_re ]] || \
  fail "release tag must match vMAJOR.MINOR.PATCH with an optional SemVer prerelease"

[[ -s LICENSE ]] || fail "LICENSE must not be empty for a release"
placeholder_markers=(
  'The project has not yet published a software license.'
  'No permission to use,'
  "Replace this notice with the project's"
)
for marker in "${placeholder_markers[@]}"; do
  if grep -Fq -- "$marker" LICENSE; then
    fail "LICENSE still contains the pre-release placeholder notice"
  fi
done

# GoReleaser must create a draft Release first so the release workflow only
# publishes it after the immutable container image, signatures, attestations,
# and digest-pinned helm-values asset all succeed.
release_block=$(awk '
  /^release:[[:space:]]*$/ { on = 1; next }
  on && /^[^[:space:]#]/ { exit }
  on { print }
' .goreleaser.yaml)
if ! grep -Eq '^[[:space:]]+draft:[[:space:]]*true[[:space:]]*(#.*)?$' <<<"$release_block"; then
  fail ".goreleaser.yaml release.draft must be true (publish-release publishes the draft after all assets succeed)"
fi

check_goreleaser_config
printf 'PASS: release prerequisites are valid for %s\n' "$tag"
