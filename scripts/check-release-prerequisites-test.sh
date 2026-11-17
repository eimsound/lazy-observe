#!/usr/bin/env bash
set -euo pipefail

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
checker_source="$source_root/scripts/check-release-prerequisites.sh"
[[ -x $checker_source ]] || {
  printf 'release prerequisite checker is not executable: %s\n' "$checker_source" >&2
  exit 1
}

work=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-release-prerequisites.XXXXXX")
trap 'rm -rf -- "$work"' EXIT HUP INT TERM
repo="$work/repo"
stub_bin="$work/bin"
stdout_file="$work/stdout"
stderr_file="$work/stderr"
calls_file="$work/goreleaser.calls"
secret_value='release-token-MUST-NOT-APPEAR'

mkdir -p "$repo/scripts" "$stub_bin"
cp "$checker_source" "$repo/scripts/check-release-prerequisites.sh"
chmod 0755 "$repo/scripts/check-release-prerequisites.sh"

cat >"$stub_bin/goreleaser" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[[ $# == 1 && $1 == check ]] || {
  printf 'unexpected goreleaser arguments\n' >&2
  exit 90
}
for name in GITHUB_TOKEN GH_TOKEN GORELEASER_FORCE_TOKEN; do
  if [[ -v $name ]]; then
    printf 'publishing credential reached goreleaser check: %s\n' "$name" >&2
    exit 91
  fi
done
[[ ${GORELEASER_STUB_FAIL:-0} != 1 ]] || exit 92
printf 'check\n' >>"${GORELEASER_CALLS:?}"
STUB
chmod 0755 "$stub_bin/goreleaser"

cat >"$repo/.goreleaser.yaml" <<'GORELEASER'
version: 2

snapshot:
  version_template: "0.0.0-SNAPSHOT-{{ .ShortCommit }}"
GORELEASER
printf 'FROM scratch\n' >"$repo/Dockerfile"
printf '# Test repository\n' >"$repo/README.md"
cat >"$repo/LICENSE" <<'LICENSE'
lazy-observe licensing notice

The project has not yet published a software license. No permission to use,
copy, modify, or distribute the software is granted except by an explicit
agreement with the copyright holder. Replace this notice with the project's
chosen license before a public release.
LICENSE

status=0
run_capture() {
  : >"$stdout_file"
  : >"$stderr_file"
  set +e
  (cd "$repo" && "$@") >"$stdout_file" 2>"$stderr_file"
  status=$?
  set -e
  if grep -Fq -- "$secret_value" "$stdout_file" "$stderr_file"; then
    printf 'publishing secret appeared in checker output\n' >&2
    exit 1
  fi
}

expect_ok() {
  local description=$1
  shift
  run_capture "$@"
  if ((status != 0)); then
    printf 'expected success (%s), got status %d\n' "$description" "$status" >&2
    exit 1
  fi
}

expect_failure() {
  local description=$1 expected=$2
  shift 2
  run_capture "$@"
  if ((status == 0)); then
    printf 'expected failure (%s)\n' "$description" >&2
    exit 1
  fi
  if ! grep -Fq -- "$expected" "$stderr_file"; then
    printf 'failure did not report expected diagnostic (%s): %s\n' "$description" "$expected" >&2
    exit 1
  fi
}

checker="$repo/scripts/check-release-prerequisites.sh"
clean_env=(
  env
  -u GITHUB_TOKEN
  -u GH_TOKEN
  -u GORELEASER_FORCE_TOKEN
  "PATH=$stub_bin:$PATH"
  "GORELEASER_CALLS=$calls_file"
)

# Snapshot validation must not require a Git repository, a publishable LICENSE,
# a tag, or publishing credentials.
expect_ok 'snapshot without release state' \
  "${clean_env[@]}" "$checker" --snapshot

sed -i 's/0.0.0-SNAPSHOT-{{ .ShortCommit }}/{{ incpatch .Version }}-snapshot/' \
  "$repo/.goreleaser.yaml"
expect_failure 'snapshot version derived from legacy tag' \
  'snapshot version_template must be independent from repository tags' \
  "${clean_env[@]}" "$checker" --snapshot
sed -i 's/{{ incpatch .Version }}-snapshot/0.0.0-SNAPSHOT-{{ .ShortCommit }}/' \
  "$repo/.goreleaser.yaml"

mv "$repo/Dockerfile" "$repo/Dockerfile.saved"
expect_failure 'snapshot missing release file' 'missing required release file: Dockerfile' \
  "${clean_env[@]}" "$checker" --snapshot
mv "$repo/Dockerfile.saved" "$repo/Dockerfile"

printf '%s\n' \
  'Test project license' \
  'Permission is granted for this temporary test fixture.' >"$repo/LICENSE"
git -C "$repo" init -q
git -C "$repo" config user.name 'Release Prerequisite Test'
git -C "$repo" config user.email release-prerequisite-test@example.invalid
git -C "$repo" add .
git -C "$repo" commit -qm 'initial fixture'
git -C "$repo" tag v1.2.3

publish_env=(
  env
  -u GITHUB_TOKEN
  -u GH_TOKEN
  -u GORELEASER_FORCE_TOKEN
  "PATH=$stub_bin:$PATH"
  "GORELEASER_CALLS=$calls_file"
  "GITHUB_TOKEN=$secret_value"
  "GH_TOKEN=$secret_value"
  "GORELEASER_FORCE_TOKEN=github"
)
expect_ok 'stable release tag' "${publish_env[@]}" "$checker"

git -C "$repo" commit --allow-empty -qm 'prerelease fixture'
git -C "$repo" tag v2.0.0-rc.1
expect_ok 'SemVer prerelease tag' "${publish_env[@]}" "$checker"

for invalid_tag in v01.2.3 v1.2.3-01 v1.2.3+build v1.2; do
  git -C "$repo" commit --allow-empty -qm "invalid tag fixture $invalid_tag"
  git -C "$repo" tag "$invalid_tag"
  expect_failure "invalid tag $invalid_tag" 'release tag must match vMAJOR.MINOR.PATCH' \
    "${publish_env[@]}" "$checker"
  git -C "$repo" tag -d "$invalid_tag" >/dev/null
done

expect_failure 'missing exact tag' 'HEAD must have exactly one release tag; found none' \
  "${publish_env[@]}" "$checker"

git -C "$repo" tag release-1.2.3
git -C "$repo" tag sdk-node-v1.2.3
expect_failure 'non-server tags are ignored' 'HEAD must have exactly one release tag; found none' \
  "${publish_env[@]}" "$checker"
git -C "$repo" tag v2.1.0
git -C "$repo" tag sdk/go/v2.1.0
expect_ok 'server tag alongside SDK tags' "${publish_env[@]}" "$checker"
git -C "$repo" tag -d release-1.2.3 sdk-node-v1.2.3 v2.1.0 sdk/go/v2.1.0 >/dev/null

git -C "$repo" tag v3.0.0
git -C "$repo" tag v3.0.1
expect_failure 'multiple exact tags' 'HEAD must have exactly one release tag; found 2' \
  "${publish_env[@]}" "$checker"
git -C "$repo" tag -d v3.0.1 >/dev/null

cat >"$repo/LICENSE" <<'LICENSE'
lazy-observe licensing notice

The project has not yet published a software license. No permission to use,
copy, modify, or distribute the software is granted except by an explicit
agreement with the copyright holder. Replace this notice with the project's
chosen license before a public release.
LICENSE
expect_failure 'placeholder LICENSE' 'LICENSE still contains the pre-release placeholder notice' \
  "${publish_env[@]}" "$checker"
printf '%s\n' \
  'Test project license' \
  'Permission is granted for this temporary test fixture.' >"$repo/LICENSE"

expect_failure 'missing GitHub token' 'GITHUB_TOKEN must be set and non-empty' \
  "${clean_env[@]}" "GH_TOKEN=$secret_value" "$checker"
expect_failure 'blank GitHub token' 'GITHUB_TOKEN must be set and non-empty' \
  "${clean_env[@]}" 'GITHUB_TOKEN=   ' "$checker"

expect_failure 'invalid command arguments' 'usage: check-release-prerequisites.sh [--snapshot]' \
  "${clean_env[@]}" "$checker" --unknown

expect_failure 'GoReleaser validation failure' 'GoReleaser configuration validation failed' \
  "${clean_env[@]}" GORELEASER_STUB_FAIL=1 "$checker" --snapshot

[[ $(wc -l <"$calls_file") -eq 4 ]] || {
  printf 'unexpected number of successful goreleaser checks\n' >&2
  exit 1
}

printf 'PASS: release prerequisite checker accepts only safe snapshot and release inputs\n'
