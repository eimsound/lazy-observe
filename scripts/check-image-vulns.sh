#!/usr/bin/env bash
set -euo pipefail

# 发布前镜像漏洞扫描：对本机 Docker 里已 load 的镜像跑 Trivy，CRITICAL/HIGH 且已有修复版本即失败。
#
# 供应链约束：
#   - 扫描器是 Trivy（Apache-2.0）。版本与两个架构的 sha256 只在 scripts/install-trivy.sh 维护
#     （嵌入的 Victoria 二进制扫描 scripts/check-storage-bin-vulns.sh 共用同一份），不用
#     aquasecurity/trivy-action、setup-trivy（原因与升级步骤见该脚本）。
#   - 漏洞库不固定：每次运行从 mirror.gcr.io/aquasec/trivy-db:2 拉 OCI 制品，失败回退
#     ghcr.io/aquasecurity/trivy-db:2（都不经 Docker Hub）。库必须是最新的，所以同一镜像的结果会随新公告变化。
#   - 只扫本机 Docker 里的镜像（--image-src docker），不会回退去远端仓库拉同名镜像。
#
# 误报与例外：写进仓库根目录的 .trivyignore.yaml，每条必须带 statement（理由）和 expired_at（到期日），
# 由 scripts/check-github-workflows.sh 校验；过期条目 Trivy 不再生效，发版会重新被挡住。

image=${1:?usage: check-image-vulns.sh IMAGE}

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tool_dir=${LO_TRIVY_DIR:-$root/.cache/trivy}
export LO_TRIVY_DIR=$tool_dir
trivy=$(bash "$root/scripts/install-trivy.sh")

"$trivy" --version

# 参数（severity、--ignore-unfixed、ignorefile、漏洞库、exit code）与 scripts/check-storage-bin-vulns.sh 保持一致。
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
