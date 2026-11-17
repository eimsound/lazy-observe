#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-goreleaser-adapter.XXXXXX")
trap 'rm -rf -- "$work"' EXIT

cat >"$work/go" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
for name in GITHUB_TOKEN GH_TOKEN GORELEASER_FORCE_TOKEN; do
  [[ -z ${!name+x} ]] || { printf '%s reached delegated Go process\n' "$name" >&2; exit 1; }
done
printf 'sanitized\n'
SH
chmod 0755 "$work/go"

output=$(
  GITHUB_TOKEN=sentinel GH_TOKEN=sentinel GORELEASER_FORCE_TOKEN=sentinel \
    PATH="$work:$PATH" "$root/scripts/goreleaser-prebuilt.sh" version
)
[[ $output == sanitized ]] || { printf 'unexpected adapter probe output: %s\n' "$output" >&2; exit 1; }
printf 'PASS: GoReleaser prebuilt adapter strips forge tokens before delegated tools\n'
