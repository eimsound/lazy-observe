#!/usr/bin/env bash
set -euo pipefail

# 发布前镜像漏洞扫描：对本机 Docker 里已 load 的镜像跑 Trivy，CRITICAL/HIGH 且已有修复版本即失败。
#
# 供应链约束：
#   - 扫描器是 Trivy（Apache-2.0）。二进制按版本 + sha256 固定，直接从 GitHub 的不可变 release 下载；
#     不用 aquasecurity/trivy-action、setup-trivy（2026-03 这两个 action 的 tag 被劫持、v0.69.4
#     二进制被投毒，见 GHSA-69fq-xp46-6x23）。升级时用 `gh release verify vX -R aquasecurity/trivy`
#     核对 release 证明，再把两个架构的 sha256 抄进下面的 case。
#   - 漏洞库不固定：每次运行从 mirror.gcr.io/aquasec/trivy-db:2 拉 OCI 制品，失败回退
#     ghcr.io/aquasecurity/trivy-db:2（都不经 Docker Hub）。库必须是最新的，所以同一镜像的结果会随新公告变化。
#   - 只扫本机 Docker 里的镜像（--image-src docker），不会回退去远端仓库拉同名镜像。
#
# 误报与例外：写进仓库根目录的 .trivyignore.yaml，每条必须带 statement（理由）和 expired_at（到期日），
# 由 scripts/check-github-workflows.sh 校验；过期条目 Trivy 不再生效，发版会重新被挡住。

image=${1:?usage: check-image-vulns.sh IMAGE}

trivy_version=0.76.2
case "$(uname -m)" in
x86_64 | amd64)
  asset=Linux-64bit
  sha256=bc0fc726e017b8927a62b2d3531d7a84ee01769daf5c99ed866dced54089fdbf
  ;;
aarch64 | arm64)
  asset=Linux-ARM64
  sha256=ab9d51f86e6a5fb936f605faff6b593ac51bc378fff4d3e8b9d58358b9c367c3
  ;;
*)
  printf 'unsupported host architecture for the pinned Trivy: %s\n' "$(uname -m)" >&2
  exit 1
  ;;
esac

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tool_dir=${LO_TRIVY_DIR:-$root/.cache/trivy}
trivy=$tool_dir/$trivy_version/trivy

if [[ ! -x $trivy ]]; then
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  archive=$work/trivy.tar.gz
  curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$archive" \
    "https://github.com/aquasecurity/trivy/releases/download/v${trivy_version}/trivy_${trivy_version}_${asset}.tar.gz"
  printf '%s  %s\n' "$sha256" "$archive" | sha256sum --check --strict -
  tar -xzf "$archive" -C "$work" trivy
  mkdir -p "$(dirname "$trivy")"
  install -m 0755 "$work/trivy" "$trivy"
  rm -rf "$work"
  trap - EXIT
fi

"$trivy" --version

"$trivy" image \
  --image-src docker \
  --scanners vuln \
  --pkg-types os,library \
  --severity CRITICAL,HIGH \
  --ignore-unfixed \
  --ignorefile "$root/.trivyignore.yaml" \
  --db-repository mirror.gcr.io/aquasec/trivy-db:2,ghcr.io/aquasecurity/trivy-db:2 \
  --cache-dir "$tool_dir/cache" \
  --skip-version-check \
  --no-progress \
  --exit-code 1 \
  "$image"
