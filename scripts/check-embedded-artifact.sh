#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
binary_input=${1:?usage: check-embedded-artifact.sh BINARY PLATFORM}
platform_input=${2:?usage: check-embedded-artifact.sh BINARY PLATFORM}

case $binary_input in
  /*) binary=$binary_input ;;
  *) binary=$root/$binary_input ;;
esac
platform=${platform_input//\//-}
case $platform in
  linux-amd64) machine=62; goarch=amd64 ;;
  linux-arm64) machine=183; goarch=arm64 ;;
  *) printf 'unsupported artifact platform: %s\n' "$platform_input" >&2; exit 1 ;;
esac

asset_dir=$root/internal/embed/bins/assets/$platform
manifest=$asset_dir/manifest.tsv
required=(victoria-metrics-prod victoria-logs-prod victoria-traces-prod vmagent-prod)
[[ -x $binary ]] || { printf 'artifact binary is missing: %s\n' "$binary" >&2; exit 1; }
[[ -f $manifest ]] || { printf 'embedded asset manifest is missing: %s\n' "$manifest" >&2; exit 1; }
[[ $(wc -l <"$manifest") -eq 4 ]] || { printf 'embedded asset manifest must contain four rows\n' >&2; exit 1; }

build_info=$(go version -m "$binary")
for setting in 'CGO_ENABLED=0' 'GOOS=linux' "GOARCH=$goarch"; do
  grep -Fq $'\tbuild\t'"$setting" <<<"$build_info" || {
    printf 'artifact build setting is missing or incorrect: %s\n' "$setting" >&2
    exit 1
  }
done

for name in "${required[@]}"; do
  [[ -s $asset_dir/$name.gz ]] || { printf 'embedded asset is missing: %s\n' "$name" >&2; exit 1; }
  [[ $(awk -F '\t' -v wanted="$name" '$1 == wanted { count++ } END { print count + 0 }' "$manifest") -eq 1 ]] || {
    printf 'embedded asset manifest does not contain exactly one %s row\n' "$name" >&2
    exit 1
  }
done

output=${LO_ARTIFACT_EVIDENCE:-$root/.cache/artifact-evidence/$platform.json}
case $output in /*) ;; *) output=$root/$output ;; esac
mkdir -p "${output%/*}"

# 内嵌校验只用 Python 标准库；解释器由 uv 受管（scripts/lib/uv-python.sh），不用系统 python3，uv 缺失即失败。
# shellcheck source=lib/uv-python.sh
. "$root/scripts/lib/uv-python.sh"
python_command=$(lo_uv_python) || exit 1

"$python_command" - "$binary" "$machine" "$platform" "$manifest" "$asset_dir" "$output" "${required[@]}" <<'PY'
import gzip
import hashlib
import json
import pathlib
import struct
import sys

binary_path = pathlib.Path(sys.argv[1])
expected_machine = int(sys.argv[2])
platform = sys.argv[3]
manifest_path = pathlib.Path(sys.argv[4])
asset_dir = pathlib.Path(sys.argv[5])
output_path = pathlib.Path(sys.argv[6])
required = sys.argv[7:]
binary = binary_path.read_bytes()
if binary[:4] != b"\x7fELF" or binary[5] != 1:
    raise SystemExit("artifact is not a little-endian ELF binary")
machine = struct.unpack_from("<H", binary, 18)[0]
if machine != expected_machine:
    raise SystemExit(f"ELF machine {machine} does not match expected {expected_machine}")
for name in required:
    if name.encode() not in binary:
        raise SystemExit(f"binary does not contain embedded manifest identity {name}")

manifest = []
for line in manifest_path.read_text(encoding="utf-8").splitlines():
    name, size, digest = line.split("\t")
    if name not in required or int(size) <= 0 or len(digest) != 64:
        raise SystemExit(f"invalid embedded manifest row: {line}")
    payload_size = 0
    payload_hash = hashlib.sha256()
    try:
        with gzip.open(asset_dir / f"{name}.gz", "rb") as payload:
            while chunk := payload.read(1024 * 1024):
                payload_size += len(chunk)
                payload_hash.update(chunk)
    except (OSError, EOFError) as error:
        raise SystemExit(f"invalid embedded gzip payload for {name}: {error}") from error
    if payload_size != int(size) or payload_hash.hexdigest() != digest:
        raise SystemExit(f"embedded payload does not match manifest for {name}")
    if digest.encode() not in binary:
        raise SystemExit(f"binary does not contain embedded manifest digest for {name}")
    manifest.append({"name": name, "uncompressed_bytes": int(size), "sha256": digest})

evidence = {
    "schema": 1,
    "platform": platform,
    "binary": binary_path.name,
    "binary_bytes": len(binary),
    "binary_sha256": hashlib.sha256(binary).hexdigest(),
    "cgo_enabled": False,
    "embedded_assets": manifest,
}
output_path.write_text(json.dumps(evidence, indent=2, sort_keys=True) + "\n", encoding="utf-8")
PY

printf 'PASS artifact-build: %s contains four verified managed assets (%s)\n' "$platform_input" "$output"
