#!/usr/bin/env bash
set -euo pipefail

# 发版前对嵌入的 Victoria 二进制做漏洞扫描：对 .cache/storage-bin/<platform> 里将被嵌入 lazy-observe 的
# 四个二进制跑 `trivy rootfs`，CRITICAL/HIGH 且已有修复版本的命中只警告，不阻塞发版（ADR 0016）。
#
# 为什么镜像扫描（scripts/check-image-vulns.sh）覆盖不到：这些二进制在 lazy-observe 里是 gzip 后
# go:embed 的 payload，镜像与归档里没有独立的可执行文件，Trivy 读不到它们的 Go buildinfo。
# 这里扫的是下载并校验过 sha256 的原始上游二进制，字节与 scripts/prepare-embedded-storage.sh 嵌入的一致。
#
# 为什么只警告：命中几乎都是上游编译时用的 Go 版本偏旧导致的标准库漏洞，我们不自己编译 Victoria，
# 只能等上游发新版，阻塞只会卡死发版。有命中时逐条输出 ::warning:: 注解并写 $GITHUB_STEP_SUMMARY 表，exit 0；
# 没有命中就只打一行 ok。版本是否落后与本脚本无关：发版前由负责人在 Renovate Dependency Dashboard 上判断。
#
# 「没扫成就失败」不变，仍 exit 非 0：Trivy 安装或运行出错、报告缺失或不可解析、识别出的 gobinary 不是
# 恰好四个、报告条数与 Trivy 退出码对不上（有命中但退出码不是 1，无命中但退出码非 0）、提取出的命中行数与报告条数不等。
# 这类失败说明门禁已经看不见东西，不能悄悄变成永远绿。
#
# 用法：check-storage-bin-vulns.sh [platform...]    默认 linux-amd64 linux-arm64
# 环境：LO_TRIVY_DIR 覆盖 Trivy 安装与缓存目录（默认 .cache/trivy）；
#       LO_STORAGE_BIN_SCAN_DIR 覆盖二进制目录根（默认 .cache/storage-bin，下面按 <platform> 分子目录）。
#
# Trivy 只读静态 buildinfo、不执行二进制，所以一台 amd64 机器可以扫 arm64 二进制。
# 需要 curl、tar、sha256sum（下载与安装 Trivy）和 jq（读报告）。
#
# 参数与 scripts/check-image-vulns.sh 保持一致（severity、--ignore-unfixed、ignorefile、漏洞库、exit code）；
# 例外只写 .trivyignore.yaml，两个扫描共用。Trivy 版本与 sha256 见 scripts/install-trivy.sh。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tool_dir=${LO_TRIVY_DIR:-$root/.cache/trivy}
scan_root=${LO_STORAGE_BIN_SCAN_DIR:-$root/.cache/storage-bin}
# 各平台目录共用 $scan_root/.archives/<platform>，与 prepare-embedded-storage.sh 的布局一致。
archive_root=${LO_STORAGE_ARCHIVE_DIR:-$scan_root/.archives}
binaries=(victoria-metrics-prod victoria-logs-prod victoria-traces-prod vmagent-prod)
downloader=$root/scripts/download-storage-bins.sh

platforms=("$@")
if ((${#platforms[@]} == 0)); then
  platforms=(linux-amd64 linux-arm64)
fi

# 平台参数先统一校验，再装 Trivy、下载二进制：参数写错时不必等前面的平台扫完才报错。
for platform in "${platforms[@]}"; do
  case "$platform" in
  linux-amd64 | linux-arm64) ;;
  *)
    printf 'unsupported storage binary platform: %s (supported: linux-amd64, linux-arm64)\n' "$platform" >&2
    exit 1
    ;;
  esac
done

for command in jq sort; do
  command -v "$command" >/dev/null 2>&1 || {
    printf 'missing required command: %s\n' "$command" >&2
    exit 1
  }
done

# GitHub workflow command 的转义：data 部分转 % \r \n；属性值（title）另外转 : 和 ,。
escape_data() {
  local s=$1
  s=${s//'%'/'%25'}
  s=${s//$'\r'/'%0D'}
  s=${s//$'\n'/'%0A'}
  printf '%s' "$s"
}
escape_property() {
  local s
  s=$(escape_data "$1")
  s=${s//':'/'%3A'}
  s=${s//','/'%2C'}
  printf '%s' "$s"
}
# step summary 表格单元格：转义 |，不让字段内容拆出新列。
md_cell() {
  printf '%s' "${1//'|'/'\|'}"
}

# ---------------------------------------------------------------------------
# 扫描
# ---------------------------------------------------------------------------

export LO_TRIVY_DIR=$tool_dir
trivy=$(bash "$root/scripts/install-trivy.sh")
"$trivy" --version

report_dir=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-storage-vulns.XXXXXX")
trap 'rm -rf "$report_dir"' EXIT HUP INT TERM

# 命中按 (CVE, 包, installed, fixed, 严重度) 合并，二进制与平台并成一列。
declare -A warn_bins=() warn_plats=()
warn_keys=()
for platform in "${platforms[@]}"; do
  dir=$scan_root/$platform
  report=$report_dir/$platform.json

  printf '== %s: %s\n' "$platform" "$dir"
  # 下载、校验 sha256、解包都交给既有脚本（GOOS/GOARCH 已给定，不需要 Go 工具链）。
  LO_STORAGE_BIN_DIR=$dir LO_STORAGE_ARCHIVE_DIR=$archive_root GOOS=${platform%-*} GOARCH=${platform#*-} \
    bash "$downloader"

  # 与 check-image-vulns.sh 同一套参数；报告写 JSON 以便检查并打印清单。
  # --exit-code 1：有命中时 Trivy 照常写报告再以 1 退出；Trivy 自身出错时报告缺失或不可解析，下面单独判断。
  rc=0
  "$trivy" rootfs \
    --scanners vuln \
    --pkg-types os,library \
    --severity CRITICAL,HIGH \
    --ignore-unfixed \
    --ignorefile "$root/.trivyignore.yaml" \
    --db-repository mirror.gcr.io/aquasec/trivy-db:2,ghcr.io/aquasecurity/trivy-db:2 \
    --cache-dir "$tool_dir/cache" \
    --skip-version-check \
    --no-progress \
    --quiet \
    --format json \
    --output "$report" \
    --exit-code 1 \
    "$dir" || rc=$?
  if [[ ! -s $report ]] || ! jq -e '.Results != null or .SchemaVersion != null' "$report" >/dev/null 2>&1; then
    printf 'trivy rootfs produced no usable report for %s (exit %s)\n' "$platform" "$rc" >&2
    exit 1
  fi

  # 防空转：必须正好识别出四个 gobinary。Trivy 一旦不再识别这些二进制（格式变化、目录写错），
  # 门禁不能悄悄变成永远绿。
  scanned=$(jq -r '[.Results[]? | select(.Type == "gobinary") | .Target] | sort | .[]' "$report")
  expected=$(printf '%s\n' "${binaries[@]}" | sort)
  if [[ $scanned != "$expected" ]]; then
    printf 'trivy did not analyze exactly the expected Go binaries in %s\n' "$dir" >&2
    printf 'expected:\n%s\nanalyzed:\n%s\n' "$expected" "${scanned:-<none>}" >&2
    exit 1
  fi

  count=$(jq '[.Results[]? | (.Vulnerabilities // [])[]] | length' "$report")
  # 报告与退出码必须互相印证：Trivy 的 0/1 与报告里的条数对不上，说明扫描本身出了问题，
  # 不能当作通过（报告为空但退出非 0，往往是 Trivy 中途失败或报告被截断）。
  if ((count == 0)); then
    if ((rc != 0)); then
      printf 'trivy exited %s for %s but its report lists no vulnerabilities; treating the scan as failed\n' \
        "$rc" "$platform" >&2
      exit 1
    fi
    printf '%s: %d binaries analyzed, no fixable CRITICAL/HIGH vulnerabilities\n' "$platform" "${#binaries[@]}"
    continue
  fi
  if ((rc == 0)); then
    printf 'trivy reported %d vulnerabilities for %s but exited 0\n' "$count" "$platform" >&2
    exit 1
  fi
  # 有命中时只认 --exit-code 给的 1；其他非 0 退出码说明 Trivy 自身出错，不能让这份报告走到只警告的分支。
  if ((rc != 1)); then
    printf 'trivy exited %s for %s with %d vulnerabilities in its report (expected 1 when there are findings); treating the scan as failed\n' \
      "$rc" "$platform" "$count" >&2
    exit 1
  fi

  # 先落盘再读：jq 写在进程替换里时失败不会触发 set -e，命中会被读成零行、落到末尾的 ok 分支。
  # 行数必须等于上面的 count，否则同样当作没扫成。空字段补成 "-"：tab 是 IFS 空白字符，连续 tab 会被 read 合并、
  # 字段错位。@tsv 会把字段里的 tab、换行转成字面量 \t、\n，一条命中始终是一行。
  findings=$report_dir/$platform.tsv
  jq -r '.Results[]? | .Target as $t | (.Vulnerabilities // [])[]
    | [$t, .PkgName, .InstalledVersion, .VulnerabilityID, .Severity, .FixedVersion]
    | map(if . == null or . == "" then "-" else . end) | @tsv' "$report" | sort >"$findings"
  rows=$(wc -l <"$findings")
  if ((rows != count)); then
    printf 'extracted %s findings from the %s report but it lists %d vulnerabilities; treating the scan as failed\n' \
      "$rows" "$platform" "$count" >&2
    exit 1
  fi
  while IFS=$'\t' read -r bin pkg installed vuln severity fixed; do
    key=$vuln$'\t'$pkg$'\t'$installed$'\t'$fixed$'\t'$severity
    if [[ -z ${warn_bins[$key]+set} ]]; then
      warn_keys+=("$key")
      warn_bins[$key]=$bin
      warn_plats[$key]=$platform
    else
      [[ ",${warn_bins[$key]}," == *",$bin,"* ]] || warn_bins[$key]+=",$bin"
      [[ ",${warn_plats[$key]}," == *",$platform,"* ]] || warn_plats[$key]+=",$platform"
    fi
  done <"$findings"
done

# ---------------------------------------------------------------------------
# 命中只警告：注解 + step summary，exit 0
# ---------------------------------------------------------------------------

if ((${#warn_keys[@]} > 0)); then
  summary=${GITHUB_STEP_SUMMARY:-}
  if [[ -n $summary ]]; then
    {
      printf '### Embedded storage binary scan: findings (warning only, not blocking the release)\n\n'
      printf 'These come from the upstream Victoria binaries, which we embed as released and never rebuild.\n\n'
      printf '| CVE | Severity | Package | Installed | Fixed in | Binaries | Platforms |\n'
      printf '| --- | --- | --- | --- | --- | --- | --- |\n'
    } >>"$summary"
  fi
  while IFS=$'\t' read -r vuln pkg installed fixed severity; do
    key=$vuln$'\t'$pkg$'\t'$installed$'\t'$fixed$'\t'$severity
    title=$(escape_property "Embedded storage binary: $vuln ($severity)")
    message=$(escape_data "$vuln in $pkg $installed (fixed in $fixed) affects ${warn_bins[$key]} on ${warn_plats[$key]}; warning only, does not block the release")
    printf '::warning title=%s::%s\n' "$title" "$message"
    if [[ -n $summary ]]; then
      printf '| %s | %s | %s | %s | %s | %s | %s |\n' "$(md_cell "$vuln")" "$(md_cell "$severity")" \
        "$(md_cell "$pkg")" "$(md_cell "$installed")" "$(md_cell "$fixed")" \
        "$(md_cell "${warn_bins[$key]//,/, }")" "$(md_cell "${warn_plats[$key]//,/, }")" >>"$summary"
    fi
  done < <(printf '%s\n' "${warn_keys[@]}" | sort)
  printf 'storage binary scan completed for: %s (%d distinct findings, warning only)\n' "${platforms[*]}" "${#warn_keys[@]}"
else
  printf 'storage binary scan ok for: %s (no fixable CRITICAL/HIGH vulnerabilities)\n' "${platforms[*]}"
fi
