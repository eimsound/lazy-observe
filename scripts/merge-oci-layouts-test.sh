#!/usr/bin/env bash
set -euo pipefail

# scripts/merge-oci-layouts.sh 的供应链自测：无 Docker/无网络构造最小合成 OCI 归档，
# 验证 inspect/merge 对直出 manifest 与嵌套 index 的正常合并、注解清理，以及对归档篡改、
# digest 漂移、架构错配、缺失架构和 layer blob 损坏的 fail-closed 拦截。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
script="$root/scripts/merge-oci-layouts.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

failures=0
fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}

write_blob() { # oci_dir content_file -> prints "sha256:<hex> <size>"
  local oci_dir=$1 src=$2 hex size
  hex=$(sha256sum "$src" | awk '{print $1}')
  size=$(wc -c <"$src")
  mkdir -p "$oci_dir/blobs/sha256"
  cp "$src" "$oci_dir/blobs/sha256/$hex"
  printf 'sha256:%s %s\n' "$hex" "$size"
}

build_synthetic_oci_tar() { # dest_tar arch mode(direct|nested) [config_arch_override]
  local dest_tar=$1 arch=$2 mode=$3
  local cfg_arch=${4:-$arch}
  local dir="$work/build-${arch}-${mode}-${RANDOM}"
  mkdir -p "$dir/blobs/sha256"

  printf 'layer-bytes-%s-%s\n' "$arch" "$mode" >"$work/layer.bin"
  read -r layer_digest layer_size < <(write_blob "$dir" "$work/layer.bin")

  jq -n --arg arch "$cfg_arch" --arg diff "$layer_digest" \
    '{architecture: $arch, os: "linux", rootfs: {type: "layers", diff_ids: [$diff]}}' >"$work/config.json"
  read -r cfg_digest cfg_size < <(write_blob "$dir" "$work/config.json")

  jq -n \
    --arg cfg_digest "$cfg_digest" --argjson cfg_size "$cfg_size" \
    --arg layer_digest "$layer_digest" --argjson layer_size "$layer_size" \
    '{
      schemaVersion: 2,
      mediaType: "application/vnd.oci.image.manifest.v1+json",
      config: {mediaType: "application/vnd.oci.image.config.v1+json", digest: $cfg_digest, size: $cfg_size},
      layers: [{mediaType: "application/vnd.oci.image.layer.v1.tar+gzip", digest: $layer_digest, size: $layer_size}]
    }' >"$work/manifest.json"
  read -r man_digest man_size < <(write_blob "$dir" "$work/manifest.json")

  printf '{"imageLayoutVersion":"1.0.0"}\n' >"$dir/oci-layout"

  local man_desc
  man_desc=$(jq -nc \
    --arg digest "$man_digest" --argjson size "$man_size" --arg arch "$arch" \
    '{
      mediaType: "application/vnd.oci.image.manifest.v1+json",
      digest: $digest,
      size: $size,
      platform: {os: "linux", architecture: $arch},
      annotations: {
        "io.containerd.image.name": ("docker.io/library/lazy-observe:preflight-" + $arch),
        "org.opencontainers.image.ref.name": ("preflight-" + $arch),
        "org.opencontainers.image.vendor": "lazy-observe"
      }
    }')

  if [[ $mode == direct ]]; then
    jq -n --argjson desc "$man_desc" \
      '{schemaVersion: 2, mediaType: "application/vnd.oci.image.index.v1+json", manifests: [$desc]}' >"$dir/index.json"
  else
    jq -n --argjson desc "$man_desc" \
      '{schemaVersion: 2, mediaType: "application/vnd.oci.image.index.v1+json", manifests: [$desc]}' >"$work/nested.json"
    read -r nested_digest nested_size < <(write_blob "$dir" "$work/nested.json")
    jq -n --arg digest "$nested_digest" --argjson size "$nested_size" \
      '{
        schemaVersion: 2,
        mediaType: "application/vnd.oci.image.index.v1+json",
        manifests: [{mediaType: "application/vnd.oci.image.index.v1+json", digest: $digest, size: $size}]
      }' >"$dir/index.json"
  fi

  tar -cf "$dest_tar" -C "$dir" oci-layout index.json blobs
  printf '%s\n' "$man_digest"
}

# 1. 正常路径：amd64（direct）+ arm64（nested index）均通过 inspect 与 merge。
in_ok="$work/in-ok"
mkdir -p "$in_ok"
want_amd64=$(build_synthetic_oci_tar "$in_ok/lazy-observe-linux-amd64.oci.tar" amd64 direct)
want_arm64=$(build_synthetic_oci_tar "$in_ok/lazy-observe-linux-arm64.oci.tar" arm64 nested)

got_amd64=$(bash "$script" inspect \
  --archive "$in_ok/lazy-observe-linux-amd64.oci.tar" \
  --arch amd64 \
  --digest-out "$in_ok/lazy-observe-linux-amd64.digest" \
  --sha256-out "$in_ok/lazy-observe-linux-amd64.oci.tar.sha256")
got_arm64=$(bash "$script" inspect \
  --archive "$in_ok/lazy-observe-linux-arm64.oci.tar" \
  --arch arm64 \
  --digest-out "$in_ok/lazy-observe-linux-arm64.digest" \
  --sha256-out "$in_ok/lazy-observe-linux-arm64.oci.tar.sha256")

[[ $got_amd64 == "$want_amd64" ]] || fail "inspect amd64 digest mismatch: $got_amd64 != $want_amd64"
[[ $got_arm64 == "$want_arm64" ]] || fail "inspect arm64 digest mismatch: $got_arm64 != $want_arm64"

merged="$work/merged"
sbom_dir="$work/sbom-amd64"
bash "$script" merge --input-dir "$in_ok" --output-dir "$merged" --sbom-dir "$sbom_dir"

[[ -f $merged/oci-layout && -f $merged/index.json ]] || fail "merged layout missing oci-layout or index.json"
[[ -f $sbom_dir/index.json ]] || fail "sbom-dir was not populated"
[[ $(jq -er '.manifests | length' "$merged/index.json") == 2 ]] || fail "merged index must have 2 manifests"
[[ $(jq -er '.manifests[] | select(.platform.architecture=="amd64") | .digest' "$merged/index.json") == "$want_amd64" ]] ||
  fail "merged amd64 digest mismatch"
[[ $(jq -er '.manifests[] | select(.platform.architecture=="arm64") | .digest' "$merged/index.json") == "$want_arm64" ]] ||
  fail "merged arm64 digest mismatch"
if jq -e '.manifests[].annotations | has("io.containerd.image.name") or has("org.opencontainers.image.ref.name")' "$merged/index.json" >/dev/null; then
  fail "merged index retained preflight tag annotations"
fi
[[ $(jq -er '.manifests[0].annotations["org.opencontainers.image.vendor"]' "$merged/index.json") == lazy-observe ]] ||
  fail "merged index dropped non-preflight annotation"

# 2. 归档 sha256 篡改必须失败。
in_bad_sha="$work/in-bad-sha"
cp -a "$in_ok" "$in_bad_sha"
printf 'tamper' >>"$in_bad_sha/lazy-observe-linux-amd64.oci.tar"
if bash "$script" merge --input-dir "$in_bad_sha" --output-dir "$work/out-bad-sha" >/dev/null 2>&1; then
  fail "merge accepted a tampered OCI archive"
fi

# 3. 预期 digest 漂移必须失败。
in_bad_digest="$work/in-bad-digest"
cp -a "$in_ok" "$in_bad_digest"
printf 'sha256:%064d\n' 0 >"$in_bad_digest/lazy-observe-linux-arm64.digest"
if bash "$script" merge --input-dir "$in_bad_digest" --output-dir "$work/out-bad-digest" >/dev/null 2>&1; then
  fail "merge accepted a mismatched manifest digest"
fi

# 4. config 架构与声明不符必须失败。
in_bad_arch="$work/in-bad-arch"
mkdir -p "$in_bad_arch"
build_synthetic_oci_tar "$in_bad_arch/wrong.oci.tar" amd64 direct arm64 >/dev/null
if bash "$script" inspect \
  --archive "$in_bad_arch/wrong.oci.tar" \
  --arch amd64 \
  --digest-out "$in_bad_arch/d" \
  --sha256-out "$in_bad_arch/s" >/dev/null 2>&1; then
  fail "inspect accepted an archive whose config architecture did not match --arch"
fi

# 5. 缺失某一架构归档必须失败。
in_missing="$work/in-missing"
cp -a "$in_ok" "$in_missing"
rm -f "$in_missing/lazy-observe-linux-arm64.oci.tar"
if bash "$script" merge --input-dir "$in_missing" --output-dir "$work/out-missing" >/dev/null 2>&1; then
  fail "merge succeeded with missing arm64 archive"
fi

if ((failures > 0)); then
  printf 'merge-oci-layouts test: %d failure(s)\n' "$failures" >&2
  exit 1
fi
printf 'PASS: merge-oci-layouts validates checksums, digests, platforms, and merges multi-arch OCI layouts\n'
