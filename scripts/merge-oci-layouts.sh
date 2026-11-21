#!/usr/bin/env bash
set -euo pipefail

# 校验并合并由 BuildKit type=oci 导出的单架构 OCI 镜像归档：
#   - inspect: 在 container-check 阶段校验单架构 OCI 归档的 manifest、config、全部 layer blob sha256
#              与平台元数据，并输出不可变 manifest digest 与归档 sha256；
#   - merge:   在无构建的 docker 发布阶段复验归档 sha256 与预期 manifest digest，剥离 preflight 标签注解，
#              组装干净的多架构 OCI image layout（可直接交给 `crane push --index` 推送），
#              并可选导出单架构展开目录供 Trivy 离线生成 SPDX SBOM。

usage() {
  cat >&2 <<'EOF'
usage:
  merge-oci-layouts.sh inspect --archive <oci.tar> --arch <amd64|arm64> --digest-out <file> --sha256-out <file>
  merge-oci-layouts.sh merge --input-dir <dir> --output-dir <dir> [--sbom-dir <dir>]
EOF
}

fail() {
  printf 'merge-oci-layouts: %s\n' "$*" >&2
  exit 1
}

digest_re='^sha256:[0-9a-f]{64}$'

verify_blob_sha256() { # oci_dir digest expected_size
  local oci_dir=$1 digest=$2 expected_size=$3 hex blob actual_size
  [[ $digest =~ $digest_re ]] || fail "invalid blob digest: $digest"
  hex=${digest#sha256:}
  blob=$oci_dir/blobs/sha256/$hex
  [[ -f $blob ]] || fail "missing OCI blob: $digest"
  if [[ -n $expected_size ]]; then
    actual_size=$(wc -c <"$blob")
    [[ $actual_size == "$expected_size" ]] ||
      fail "OCI blob $digest size mismatch: got $actual_size, want $expected_size"
  fi
  printf '%s  %s\n' "$hex" "$blob" | sha256sum --check --strict --status - ||
    fail "OCI blob $digest failed sha256 verification"
}

# 从已解包的单架构 OCI 目录中解析并校验唯一的 image manifest descriptor（处理直出 manifest 与单元素嵌套 index 两种形态），
# 输出经过清理的 descriptor JSON 到 stdout。
resolve_single_manifest_descriptor() { # oci_dir arch
  local oci_dir=$1 arch=$2
  [[ -f $oci_dir/oci-layout ]] || fail "$oci_dir: missing oci-layout"
  [[ -f $oci_dir/index.json ]] || fail "$oci_dir: missing index.json"

  local count
  count=$(jq -er '.manifests | length' "$oci_dir/index.json") || fail "$oci_dir/index.json: invalid manifests array"
  [[ $count == 1 ]] || fail "$oci_dir/index.json: expected exactly 1 manifest entry, got $count"

  local desc media_type digest size nested_hex nested_path nested_count
  desc=$(jq -ec '.manifests[0]' "$oci_dir/index.json")
  media_type=$(jq -er '.mediaType' <<<"$desc")
  digest=$(jq -er '.digest' <<<"$desc")
  size=$(jq -er '.size' <<<"$desc")
  verify_blob_sha256 "$oci_dir" "$digest" "$size"

  case "$media_type" in
  application/vnd.oci.image.index.v1+json | application/vnd.docker.distribution.manifest.list.v2+json)
    nested_hex=${digest#sha256:}
    nested_path=$oci_dir/blobs/sha256/$nested_hex
    nested_count=$(jq -er '.manifests | length' "$nested_path") ||
      fail "$oci_dir: nested index $digest has invalid manifests array"
    [[ $nested_count == 1 ]] ||
      fail "$oci_dir: nested index $digest expected 1 manifest entry, got $nested_count"
    desc=$(jq -ec '.manifests[0]' "$nested_path")
    media_type=$(jq -er '.mediaType' <<<"$desc")
    digest=$(jq -er '.digest' <<<"$desc")
    size=$(jq -er '.size' <<<"$desc")
    verify_blob_sha256 "$oci_dir" "$digest" "$size"
    ;;
  esac

  case "$media_type" in
  application/vnd.oci.image.manifest.v1+json | application/vnd.docker.distribution.manifest.v2+json) ;;
  *) fail "$oci_dir: unsupported image manifest mediaType: $media_type" ;;
  esac

  local manifest_path config_digest config_size config_path cfg_os cfg_arch
  manifest_path=$oci_dir/blobs/sha256/${digest#sha256:}
  config_digest=$(jq -er '.config.digest' "$manifest_path")
  config_size=$(jq -er '.config.size' "$manifest_path")
  verify_blob_sha256 "$oci_dir" "$config_digest" "$config_size"

  config_path=$oci_dir/blobs/sha256/${config_digest#sha256:}
  cfg_os=$(jq -er '.os' "$config_path")
  cfg_arch=$(jq -er '.architecture' "$config_path")
  [[ $cfg_os == linux && $cfg_arch == "$arch" ]] ||
    fail "$oci_dir: config platform mismatch: got ${cfg_os}/${cfg_arch}, want linux/${arch}"

  local layer_count i layer_digest layer_size
  layer_count=$(jq -er '.layers | length' "$manifest_path")
  ((layer_count > 0)) || fail "$oci_dir: manifest $digest has no layers"
  for ((i = 0; i < layer_count; i++)); do
    layer_digest=$(jq -er --argjson i "$i" '.layers[$i].digest' "$manifest_path")
    layer_size=$(jq -er --argjson i "$i" '.layers[$i].size' "$manifest_path")
    verify_blob_sha256 "$oci_dir" "$layer_digest" "$layer_size"
  done

  # 清理 preflight tag 注解，固定标准 platform 字段，避免临时标签名进入最终多架构 index。
  jq -ec --arg arch "$arch" '
    .platform = {"architecture": $arch, "os": "linux"}
    | if has("annotations") then
        .annotations |= del(.["io.containerd.image.name"], .["org.opencontainers.image.ref.name"])
        | if (.annotations | length) == 0 then del(.annotations) else . end
      else . end
  ' <<<"$desc"
}

cmd_inspect() {
  local archive= arch= digest_out= sha256_out=
  while (($# > 0)); do
    case $1 in
    --archive) archive=${2:?}; shift 2 ;;
    --arch) arch=${2:?}; shift 2 ;;
    --digest-out) digest_out=${2:?}; shift 2 ;;
    --sha256-out) sha256_out=${2:?}; shift 2 ;;
    *) usage; exit 2 ;;
    esac
  done
  [[ -n $archive && -n $arch && -n $digest_out && -n $sha256_out ]] || { usage; exit 2; }
  [[ $arch == amd64 || $arch == arm64 ]] || fail "unsupported arch: $arch"
  [[ -s $archive ]] || fail "missing or empty OCI archive: $archive"

  local desc digest archive_dir archive_base
  work=$(mktemp -d)
  tar -xf "$archive" -C "$work"

  desc=$(resolve_single_manifest_descriptor "$work" "$arch")
  digest=$(jq -er '.digest' <<<"$desc")
  [[ $digest =~ $digest_re ]] || fail "unexpected manifest digest: $digest"

  mkdir -p "$(dirname -- "$digest_out")" "$(dirname -- "$sha256_out")"
  printf '%s\n' "$digest" >"$digest_out"
  archive_dir=$(dirname -- "$archive")
  archive_base=$(basename -- "$archive")
  (cd "$archive_dir" && sha256sum "$archive_base") >"$sha256_out"
  printf '%s\n' "$digest"
}

cmd_merge() {
  local input_dir= output_dir= sbom_dir=
  while (($# > 0)); do
    case $1 in
    --input-dir) input_dir=${2:?}; shift 2 ;;
    --output-dir) output_dir=${2:?}; shift 2 ;;
    --sbom-dir) sbom_dir=${2:?}; shift 2 ;;
    *) usage; exit 2 ;;
    esac
  done
  [[ -n $input_dir && -n $output_dir ]] || { usage; exit 2; }
  [[ -d $input_dir ]] || fail "missing input directory: $input_dir"

  work=$(mktemp -d)

  rm -rf "$output_dir"
  mkdir -p "$output_dir/blobs/sha256"

  local -a descriptors=()
  local arch tar_name sha_name digest_name expected_digest extract_dir desc actual_digest blob blob_name dest_blob
  for arch in amd64 arm64; do
    tar_name="lazy-observe-linux-${arch}.oci.tar"
    sha_name="${tar_name}.sha256"
    digest_name="lazy-observe-linux-${arch}.digest"
    [[ -s $input_dir/$tar_name ]] || fail "missing OCI archive: $input_dir/$tar_name"
    [[ -s $input_dir/$sha_name ]] || fail "missing sha256 file: $input_dir/$sha_name"
    [[ -s $input_dir/$digest_name ]] || fail "missing digest file: $input_dir/$digest_name"

    (cd "$input_dir" && sha256sum --check --strict --status "$sha_name") ||
      fail "archive sha256 verification failed for $tar_name"

    expected_digest=$(tr -d '[:space:]' <"$input_dir/$digest_name")
    [[ $expected_digest =~ $digest_re ]] || fail "invalid expected digest in $digest_name: $expected_digest"

    extract_dir="$work/$arch"
    mkdir -p "$extract_dir"
    tar -xf "$input_dir/$tar_name" -C "$extract_dir"

    desc=$(resolve_single_manifest_descriptor "$extract_dir" "$arch")
    actual_digest=$(jq -er '.digest' <<<"$desc")
    [[ $actual_digest == "$expected_digest" ]] ||
      fail "manifest digest mismatch for linux/${arch}: got $actual_digest, want $expected_digest"

    for blob in "$extract_dir"/blobs/sha256/*; do
      [[ -f $blob ]] || continue
      blob_name=$(basename -- "$blob")
      dest_blob="$output_dir/blobs/sha256/$blob_name"
      if [[ ! -e $dest_blob ]]; then
        cp -p "$blob" "$dest_blob"
      fi
    done

    if [[ -n $sbom_dir && $arch == amd64 ]]; then
      rm -rf "$sbom_dir"
      mkdir -p "$sbom_dir"
      cp -a "$extract_dir/." "$sbom_dir/"
    fi

    descriptors+=("$desc")
  done

  printf '{"imageLayoutVersion":"1.0.0"}\n' >"$output_dir/oci-layout"
  jq -n \
    --argjson amd64 "${descriptors[0]}" \
    --argjson arm64 "${descriptors[1]}" \
    '{
      schemaVersion: 2,
      mediaType: "application/vnd.oci.image.index.v1+json",
      manifests: [$amd64, $arm64]
    }' >"$output_dir/index.json"
}

(($# >= 1)) || { usage; exit 2; }
command -v jq >/dev/null 2>&1 || fail 'jq is required'
work=
trap '[[ -n $work ]] && rm -rf -- "$work"' EXIT HUP INT TERM
subcommand=$1
shift
case "$subcommand" in
inspect) cmd_inspect "$@" ;;
merge) cmd_merge "$@" ;;
*) usage; exit 2 ;;
esac
