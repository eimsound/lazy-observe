#!/usr/bin/env bash
set -euo pipefail

# GoReleaser needs its forge token in the parent process to publish, but neither
# the prebuilt importer nor delegated Go toolchain subprocesses need it.
unset GITHUB_TOKEN GH_TOKEN GORELEASER_FORCE_TOKEN

root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)

# The GoReleaser OSS schema pinned in mise.toml has no native prebuilt builder. Its Go
# builder still permits a custom tool; for the build command this adapter imports
# the exact binary prepared and gated by the global GoReleaser before hooks. Other
# commands are delegated to Go so GoReleaser's toolchain discovery still works.
if [[ ${1:-} != build ]]; then
  exec go "$@"
fi
shift

output=
while (($#)); do
  case "$1" in
    -o)
      (($# >= 2)) || { printf 'missing value for -o\n' >&2; exit 1; }
      output=$2
      shift 2
      ;;
    -o=*)
      output=${1#-o=}
      shift
      ;;
    *) shift ;;
  esac
done

[[ -n $output ]] || { printf 'GoReleaser did not provide an output path\n' >&2; exit 1; }
[[ -n ${GOOS:-} && -n ${GOARCH:-} ]] || { printf 'GOOS and GOARCH are required\n' >&2; exit 1; }
source_path="$root/bin/lazy-observe-$GOOS-$GOARCH"
[[ -x $source_path ]] || { printf 'missing prebuilt release binary: %s\n' "$source_path" >&2; exit 1; }
mkdir -p "$(dirname -- "$output")"
install -m 0755 "$source_path" "$output"
