#!/usr/bin/env bash
set -euo pipefail

# scripts/merge-oci-layouts.sh 的供应链自测：无 Docker/无网络构造最小合成 OCI 归档，
# 验证 inspect/merge 对直出 manifest 与嵌套 index 的正常合并、注解清理，以及对归档篡改、
# digest 漂移、架构错配、缺失架构和 layer blob 损坏的 fail-closed 拦截。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
script="$root/scripts/merge-oci-layouts.sh"
work=$(mktemp -d)
loaded_docker_tags=()
cleanup() {
  local tag
  if ((${#loaded_docker_tags[@]} > 0)) && command -v docker >/dev/null 2>&1; then
    for tag in "${loaded_docker_tags[@]}"; do
      docker rmi -f "$tag" >/dev/null 2>&1 || true
    done
  fi
  rm -rf "$work"
}
trap cleanup EXIT

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

build_synthetic_oci_tar() { # dest_tar arch mode(direct|nested) [config_arch_override] [extra_mode(none|extra)]
  local dest_tar=$1 arch=$2 mode=$3
  local cfg_arch=${4:-$arch}
  local extra_mode=${5:-none}
  local dir="$work/build-${arch}-${mode}-${RANDOM}"
  mkdir -p "$dir/blobs/sha256"

  local -a layer_digests=() layer_sizes=() diff_ids=() extra_members=()
  local idx layer_stage layer_tar layer_gz diff_hex l_digest l_size
  for idx in 0 1; do
    layer_stage="$work/layer-stage-${arch}-${mode}-${idx}-${RANDOM}"
    mkdir -p "$layer_stage/app"
    printf 'layer-%s-%s-%s\n' "$idx" "$arch" "$mode" >"$layer_stage/app/layer-$idx.txt"
    layer_tar="$work/layer-${arch}-${mode}-${idx}-${RANDOM}.tar"
    layer_gz="${layer_tar}.gz"
    tar --sort=name --mtime='UTC 2024-01-01' --owner=0 --group=0 --numeric-owner \
      -cf "$layer_tar" -C "$layer_stage" app
    diff_hex=$(sha256sum "$layer_tar" | awk '{print $1}')
    gzip -n -c "$layer_tar" >"$layer_gz"
    read -r l_digest l_size < <(write_blob "$dir" "$layer_gz")
    diff_ids+=("sha256:$diff_hex")
    layer_digests+=("$l_digest")
    layer_sizes+=("$l_size")
  done

  jq -n \
    --arg arch "$cfg_arch" \
    --arg d0 "${diff_ids[0]}" \
    --arg d1 "${diff_ids[1]}" \
    '{
      architecture: $arch,
      os: "linux",
      config: {},
      rootfs: {type: "layers", diff_ids: [$d0, $d1]}
    }' >"$work/config.json"
  read -r cfg_digest cfg_size < <(write_blob "$dir" "$work/config.json")

  jq -n \
    --arg cfg_digest "$cfg_digest" --argjson cfg_size "$cfg_size" \
    --arg l0_digest "${layer_digests[0]}" --argjson l0_size "${layer_sizes[0]}" \
    --arg l1_digest "${layer_digests[1]}" --argjson l1_size "${layer_sizes[1]}" \
    '{
      schemaVersion: 2,
      mediaType: "application/vnd.oci.image.manifest.v1+json",
      config: {mediaType: "application/vnd.oci.image.config.v1+json", digest: $cfg_digest, size: $cfg_size},
      layers: [
        {mediaType: "application/vnd.oci.image.layer.v1.tar+gzip", digest: $l0_digest, size: $l0_size},
        {mediaType: "application/vnd.oci.image.layer.v1.tar+gzip", digest: $l1_digest, size: $l1_size}
      ]
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

  if [[ $extra_mode == extra ]]; then
    printf 'unreferenced-blob\n' >"$work/extra-blob.bin"
    write_blob "$dir" "$work/extra-blob.bin" >/dev/null
    printf 'extra-root-file\n' >"$dir/extra-file.txt"
    extra_members+=(extra-file.txt)
  fi

  tar -cf "$dest_tar" -C "$dir" oci-layout index.json blobs "${extra_members[@]}"
  printf '%s\n' "$man_digest"
}

verify_docker_tar_output() { # oci_tar arch mode want_digest repo_tag
  local oci_tar=$1 arch=$2 mode=$3 want_digest=$4 repo_tag=$5
  local before_sha after_sha docker_tar digest_out sha_out got_digest
  local orig_dir="$work/verify-orig-${arch}-${mode}"
  local docker_dir="$work/verify-docker-${arch}-${mode}"
  docker_tar="$work/out-${arch}-${mode}.docker.tar"
  digest_out="$work/out-${arch}-${mode}.digest"
  sha_out="$work/out-${arch}-${mode}.sha256"

  before_sha=$(sha256sum "$oci_tar" | awk '{print $1}')
  got_digest=$(bash "$script" inspect \
    --archive "$oci_tar" \
    --arch "$arch" \
    --digest-out "$digest_out" \
    --sha256-out "$sha_out" \
    --docker-tar-out "$docker_tar" \
    --repo-tag "$repo_tag")
  after_sha=$(sha256sum "$oci_tar" | awk '{print $1}')

  [[ $after_sha == "$before_sha" ]] || fail "inspect modified original OCI archive ($mode)"
  [[ $got_digest == "$want_digest" ]] || fail "inspect --docker-tar-out digest mismatch ($mode): $got_digest != $want_digest"
  [[ -s $docker_tar ]] || fail "inspect did not produce non-empty docker tar ($mode)"

  mkdir -p "$orig_dir" "$docker_dir"
  tar -xf "$oci_tar" -C "$orig_dir"
  tar -xf "$docker_tar" -C "$docker_dir"

  local man_hex cfg_hex
  local -a layer_hexes=() raw_entries=() actual_files=() expected_files=()
  man_hex=${want_digest#sha256:}
  cfg_hex=$(jq -er '.config.digest | sub("^sha256:"; "")' "$orig_dir/blobs/sha256/$man_hex")
  mapfile -t layer_hexes < <(jq -er '.layers[].digest | sub("^sha256:"; "")' "$orig_dir/blobs/sha256/$man_hex")

  mapfile -t raw_entries < <(tar -tf "$docker_tar")
  local entry
  for entry in "${raw_entries[@]}"; do
    [[ $entry == */ ]] && continue
    actual_files+=("$entry")
  done
  expected_files=("manifest.json" "blobs/sha256/$cfg_hex")
  local lhex
  for lhex in "${layer_hexes[@]}"; do
    expected_files+=("blobs/sha256/$lhex")
  done

  local actual_sorted expected_sorted
  actual_sorted=$(printf '%s\n' "${actual_files[@]}" | sort)
  expected_sorted=$(printf '%s\n' "${expected_files[@]}" | sort)
  [[ $actual_sorted == "$expected_sorted" ]] ||
    fail "docker tar ($mode) archive members mismatch: got [$actual_sorted], want [$expected_sorted]"

  [[ ! -e $docker_dir/oci-layout && ! -e $docker_dir/index.json && ! -e $docker_dir/extra-file.txt ]] ||
    fail "docker tar ($mode) unexpectedly contained oci-layout, index.json, or extra files"
  [[ ! -e $docker_dir/blobs/sha256/$man_hex ]] ||
    fail "docker tar ($mode) unexpectedly included manifest blob"

  if ! jq -e \
    --arg cfg "blobs/sha256/$cfg_hex" \
    --arg tag "$repo_tag" \
    --arg l0 "blobs/sha256/${layer_hexes[0]}" \
    --arg l1 "blobs/sha256/${layer_hexes[1]}" \
    'type == "array" and length == 1 and .[0].Config == $cfg and .[0].RepoTags == [$tag] and .[0].Layers == [$l0, $l1]' \
    "$docker_dir/manifest.json" >/dev/null; then
    fail "docker tar ($mode) manifest.json content mismatch: $(cat "$docker_dir/manifest.json")"
  fi

  cmp -s "$orig_dir/blobs/sha256/$cfg_hex" "$docker_dir/blobs/sha256/$cfg_hex" ||
    fail "docker tar ($mode) config blob bytes differ from OCI archive"
  for lhex in "${layer_hexes[@]}"; do
    cmp -s "$orig_dir/blobs/sha256/$lhex" "$docker_dir/blobs/sha256/$lhex" ||
      fail "docker tar ($mode) layer blob $lhex bytes differ from OCI archive"
  done

  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    loaded_docker_tags+=("$repo_tag")
    if ! docker load -i "$docker_tar" >/dev/null; then
      fail "docker load -i failed for $mode ($repo_tag)"
    else
      local got_platform got_diff_ids want_diff_ids
      got_platform=$(docker image inspect -f '{{.Os}}/{{.Architecture}}' "$repo_tag" 2>/dev/null || true)
      [[ $got_platform == "linux/${arch}" ]] ||
        fail "docker image platform mismatch for $repo_tag: got '$got_platform', want 'linux/${arch}'"
      got_diff_ids=$(docker image inspect -f '{{json .RootFS.Layers}}' "$repo_tag" 2>/dev/null || true)
      want_diff_ids=$(jq -ec '.rootfs.diff_ids' "$orig_dir/blobs/sha256/$cfg_hex")
      if ! jq -ne --argjson got "${got_diff_ids:-null}" --argjson want "$want_diff_ids" '$got == $want' >/dev/null; then
        fail "docker image diff_ids mismatch for $repo_tag: got '$got_diff_ids', want '$want_diff_ids'"
      fi
      docker rmi -f "$repo_tag" >/dev/null 2>&1 || true
    fi
  fi
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

# 1a. 成对参数校验：仅传 --docker-tar-out 或仅传 --repo-tag 必须以退出码 2 报错。
set +e
bash "$script" inspect \
  --archive "$in_ok/lazy-observe-linux-amd64.oci.tar" \
  --arch amd64 \
  --digest-out "$work/pair-d1" \
  --sha256-out "$work/pair-s1" \
  --docker-tar-out "$work/pair-only.docker.tar" >/dev/null 2>&1
rc_only_out=$?
bash "$script" inspect \
  --archive "$in_ok/lazy-observe-linux-amd64.oci.tar" \
  --arch amd64 \
  --digest-out "$work/pair-d2" \
  --sha256-out "$work/pair-s2" \
  --repo-tag "lazy-observe:only-tag" >/dev/null 2>&1
rc_only_tag=$?
set -e
[[ $rc_only_out -eq 2 ]] || fail "inspect with only --docker-tar-out returned exit code $rc_only_out, want 2"
[[ $rc_only_tag -eq 2 ]] || fail "inspect with only --repo-tag returned exit code $rc_only_tag, want 2"

# 1b. inspect --docker-tar-out --repo-tag 校验（覆盖 direct 与 nested index，以及带冗余文件归档）。
extra_direct_tar="$work/extra-direct.oci.tar"
extra_nested_tar="$work/extra-nested.oci.tar"
want_extra_direct=$(build_synthetic_oci_tar "$extra_direct_tar" amd64 direct amd64 extra)
want_extra_nested=$(build_synthetic_oci_tar "$extra_nested_tar" arm64 nested arm64 extra)
verify_docker_tar_output "$extra_direct_tar" amd64 direct "$want_extra_direct" "lazy-observe:test-direct-amd64-$$-$RANDOM"
verify_docker_tar_output "$extra_nested_tar" arm64 nested "$want_extra_nested" "lazy-observe:test-nested-arm64-$$-$RANDOM"

# 1c. amd64 为嵌套 index（含冗余文件/未引用 blob）时，merge --sbom-dir 输出规范化单 manifest OCI 布局。
in_sbom="$work/in-sbom-nested"
mkdir -p "$in_sbom"
want_sbom_amd64=$(build_synthetic_oci_tar "$in_sbom/lazy-observe-linux-amd64.oci.tar" amd64 nested amd64 extra)
want_sbom_arm64=$(build_synthetic_oci_tar "$in_sbom/lazy-observe-linux-arm64.oci.tar" arm64 direct arm64 extra)
bash "$script" inspect \
  --archive "$in_sbom/lazy-observe-linux-amd64.oci.tar" \
  --arch amd64 \
  --digest-out "$in_sbom/lazy-observe-linux-amd64.digest" \
  --sha256-out "$in_sbom/lazy-observe-linux-amd64.oci.tar.sha256" >/dev/null
bash "$script" inspect \
  --archive "$in_sbom/lazy-observe-linux-arm64.oci.tar" \
  --arch arm64 \
  --digest-out "$in_sbom/lazy-observe-linux-arm64.digest" \
  --sha256-out "$in_sbom/lazy-observe-linux-arm64.oci.tar.sha256" >/dev/null

merged_sbom="$work/merged-sbom"
sbom_nested="$work/sbom-amd64-nested"
bash "$script" merge --input-dir "$in_sbom" --output-dir "$merged_sbom" --sbom-dir "$sbom_nested"

[[ -f $sbom_nested/oci-layout && -f $sbom_nested/index.json ]] || fail "normalized sbom-dir missing oci-layout or index.json"
[[ $(jq -er '.imageLayoutVersion' "$sbom_nested/oci-layout") == "1.0.0" ]] || fail "sbom-dir oci-layout version mismatch"
if ! jq -e \
  --arg want "$want_sbom_amd64" \
  '.schemaVersion == 2 and .mediaType == "application/vnd.oci.image.index.v1+json" and (.manifests | length == 1) and .manifests[0].mediaType == "application/vnd.oci.image.manifest.v1+json" and .manifests[0].digest == $want and .manifests[0].platform == {"os":"linux","architecture":"amd64"}' \
  "$sbom_nested/index.json" >/dev/null; then
  fail "sbom-dir index.json was not normalized to single unwrapped amd64 manifest: $(cat "$sbom_nested/index.json")"
fi
if jq -e '.manifests[0].annotations | has("io.containerd.image.name") or has("org.opencontainers.image.ref.name")' "$sbom_nested/index.json" >/dev/null; then
  fail "sbom-dir index.json retained preflight tag annotations"
fi
[[ $(jq -er '.manifests[0].annotations["org.opencontainers.image.vendor"]' "$sbom_nested/index.json") == lazy-observe ]] ||
  fail "sbom-dir index.json dropped non-preflight annotation"
[[ ! -e $sbom_nested/extra-file.txt ]] || fail "sbom-dir retained extraneous root file"

sbom_orig_dir="$work/sbom-orig-amd64"
mkdir -p "$sbom_orig_dir"
tar -xf "$in_sbom/lazy-observe-linux-amd64.oci.tar" -C "$sbom_orig_dir"
sbom_man_hex=${want_sbom_amd64#sha256:}
sbom_cfg_hex=$(jq -er '.config.digest | sub("^sha256:"; "")' "$sbom_orig_dir/blobs/sha256/$sbom_man_hex")
mapfile -t sbom_layer_hexes < <(jq -er '.layers[].digest | sub("^sha256:"; "")' "$sbom_orig_dir/blobs/sha256/$sbom_man_hex")
sbom_want_blobs=$(printf '%s\n' "$sbom_man_hex" "$sbom_cfg_hex" "${sbom_layer_hexes[@]}" | sort)
sbom_got_blobs=$(
  for blob_path in "$sbom_nested"/blobs/sha256/*; do
    [[ -f $blob_path ]] && basename -- "$blob_path"
  done | sort
)
[[ $sbom_got_blobs == "$sbom_want_blobs" ]] ||
  fail "sbom-dir blobs mismatch (expected only manifest/config/layers): got [$sbom_got_blobs], want [$sbom_want_blobs]"

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
