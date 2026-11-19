#!/usr/bin/env bash
set -euo pipefail

# 按版本 + sha256 固定安装 Trivy 到 .cache/trivy/<version>/trivy，把可执行文件路径打到 stdout。
# 镜像扫描（scripts/check-image-vulns.sh）与嵌入的 Victoria 二进制扫描
# （scripts/check-storage-bin-vulns.sh）共用这一份常量，避免两处 pin 漂移。
#
# 供应链约束：
#   - 不用 aquasecurity/trivy-action、setup-trivy（2026-03 这两个 action 的 tag 被劫持、v0.69.4
#     二进制被投毒，见 GHSA-69fq-xp46-6x23）。升级时用 `gh release verify vX -R aquasecurity/trivy`
#     核对 release 证明，再把两个架构的 sha256 抄进下面的 case。
#   - 只写仓库内的 .cache/trivy（可用 LO_TRIVY_DIR 覆盖），不装到 PATH 或宿主机全局位置。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)

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

tool_dir=${LO_TRIVY_DIR:-$root/.cache/trivy}
trivy=$tool_dir/$trivy_version/trivy

if [[ ! -x $trivy ]]; then
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  archive=$work/trivy.tar.gz
  curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$archive" \
    "https://github.com/aquasecurity/trivy/releases/download/v${trivy_version}/trivy_${trivy_version}_${asset}.tar.gz"
  printf '%s  %s\n' "$sha256" "$archive" | sha256sum --check --strict - >&2
  tar -xzf "$archive" -C "$work" trivy
  mkdir -p "$(dirname "$trivy")"
  install -m 0755 "$work/trivy" "$trivy"
  rm -rf "$work"
  trap - EXIT
fi

printf '%s\n' "$trivy"
