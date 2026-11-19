#!/usr/bin/env bash
set -euo pipefail

# 发版前对嵌入的 Victoria 二进制做漏洞扫描：对 .cache/storage-bin/<platform> 里将被嵌入 lazy-observe 的
# 四个二进制跑 `trivy rootfs`，CRITICAL/HIGH 且已有修复版本的命中按下面的「放行策略」判定。
#
# 为什么镜像扫描（scripts/check-image-vulns.sh）覆盖不到：这些二进制在 lazy-observe 里是 gzip 后
# go:embed 的 payload，镜像与归档里没有独立的可执行文件，Trivy 读不到它们的 Go buildinfo。
# 这里扫的是下载并校验过 sha256 的原始上游二进制，字节与 scripts/prepare-embedded-storage.sh 嵌入的一致。
#
# 放行策略（负责人决定：允许发版，忽略上游 7 天内未解决的 CVE）：
#   我们不能自己重编 Victoria，能做的只有升级 pin。所以按组件看「升级是否可能修掉」：
#     victoria-metrics-prod、vmagent-prod → VictoriaMetrics/VictoriaMetrics（pin = metrics_version）
#     victoria-logs-prod                  → VictoriaMetrics/VictoriaLogs（pin = logs_version）
#     victoria-traces-prod                → VictoriaMetrics/VictoriaTraces（pin = traces_version）
#   latest-eligible = `scripts/check-pinned-releases.sh --latest-eligible OWNER/REPO`（发布满
#   MIN_RELEASE_AGE_DAYS 天的最新稳定版，规则只在那个脚本里维护，这里不复制）。
#     - pin >= latest-eligible：该组件的命中只警告（::warning:: 注解 + $GITHUB_STEP_SUMMARY 表），不阻塞，
#       因为上游满 7 天的版本里还没有修复，我们无从升级；
#     - pin <  latest-eligible：照常阻塞，升级 pin 就可能修掉，逼着升级；
#     - 查询 latest-eligible 失败（退出 1：没有合格 release；退出 2：API 失败、限流、参数非法）：阻塞，
#       fail-closed，不能因为 API 抖动放空门禁。
#   不分严重度：CRITICAL 与 HIGH 同样处理。扫描本身失败、识别出的 gobinary 不是四个、报告条数与 Trivy
#   退出码对不上，这些 fail-closed 语义不受放行策略影响。
#   结论会随时间变化：上游新版本满 7 天后，原先只警告的命中会转成阻塞——这不是 flaky，处理方式是升级 pin。
#
# 用法：check-storage-bin-vulns.sh [platform...]    默认 linux-amd64 linux-arm64
# 环境：LO_TRIVY_DIR 覆盖 Trivy 安装与缓存目录（默认 .cache/trivy）；
#       LO_STORAGE_BIN_SCAN_DIR 覆盖二进制目录根（默认 .cache/storage-bin，下面按 <platform> 分子目录）；
#       GH_TOKEN / GITHUB_TOKEN 给 check-pinned-releases.sh 访问 GitHub API 用（没有时匿名，60 次/小时）。
#
# Trivy 只读静态 buildinfo、不执行二进制，所以一台 amd64 机器可以扫 arm64 二进制。
# 需要 curl、tar、sha256sum（下载与安装 Trivy）和 jq（读报告）。
#
# 参数与 scripts/check-image-vulns.sh 保持一致（severity、--ignore-unfixed、ignorefile、漏洞库、exit code）；
# 例外只写 .trivyignore.yaml，两个扫描共用。Trivy 版本与 sha256 见 scripts/install-trivy.sh。
#
# 测试钩子（scripts/check-storage-bin-vulns-test.sh 用，离线）：
#   LO_STORAGE_BIN_DOWNLOADER  替换下载步骤的脚本；pin（*_version=）也从这个文件解析，默认就是
#                              scripts/download-storage-bins.sh，所以测试的桩脚本里写几行 *_version= 即可。
#   latest-eligible 用 check-pinned-releases.sh 自己的 LO_PINNED_RELEASES_FIXTURE / LO_PINNED_RELEASES_NOW
#   钩子，直接透传环境变量，不另设注入点；设了 FIXTURE 时不做「新版本尚未满 7 天」的提示（那个提示要联网）。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
tool_dir=${LO_TRIVY_DIR:-$root/.cache/trivy}
scan_root=${LO_STORAGE_BIN_SCAN_DIR:-$root/.cache/storage-bin}
binaries=(victoria-metrics-prod victoria-logs-prod victoria-traces-prod vmagent-prod)
downloader=${LO_STORAGE_BIN_DOWNLOADER:-$root/scripts/download-storage-bins.sh}
pinned_releases=$root/scripts/check-pinned-releases.sh

# shellcheck source=scripts/release-age.env
. "$root/scripts/release-age.env"
min_days=${MIN_RELEASE_AGE_DAYS:?scripts/release-age.env must set MIN_RELEASE_AGE_DAYS}

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

for command in jq date sort; do
  command -v "$command" >/dev/null 2>&1 || {
    printf 'missing required command: %s\n' "$command" >&2
    exit 1
  }
done

# ---------------------------------------------------------------------------
# 组件、pin、latest-eligible
# ---------------------------------------------------------------------------

declare -A comp_label=([metrics]=VictoriaMetrics [logs]=VictoriaLogs [traces]=VictoriaTraces)
declare -A comp_pin=()

# FILE 里恰好一行 NAME=vX.Y.Z 才算数；缺失、重复、格式漂移都直接失败（不能当成「没有 pin」放行）。
read_pin() {
  local file=$1 name=$2 line count=0 value=
  local re="^[[:space:]]*${name}=(v[0-9]+\\.[0-9]+\\.[0-9]+)[[:space:]]*(#.*)?\$"
  while IFS= read -r line || [[ -n $line ]]; do
    if [[ $line =~ $re ]]; then
      value=${BASH_REMATCH[1]}
      count=$((count + 1))
    fi
  done <"$file"
  ((count == 1)) || {
    printf '%s must contain exactly one %s=vX.Y.Z line\n' "$file" "$name" >&2
    exit 1
  }
  printf '%s\n' "$value"
}

[[ -f $downloader ]] || {
  printf 'missing storage binary downloader: %s\n' "$downloader" >&2
  exit 1
}
for comp in metrics logs traces; do
  comp_pin[$comp]=$(read_pin "$downloader" "${comp}_version")
done

component_of() {
  case $1 in
  victoria-metrics-prod | vmagent-prod) printf 'metrics\n' ;;
  victoria-logs-prod) printf 'logs\n' ;;
  victoria-traces-prod) printf 'traces\n' ;;
  *) return 1 ;;
  esac
}

# version_lt A B：A 的 semver 小于 B（两者都是严格 vX.Y.Z）。
version_lt() {
  local lowest
  [[ $1 != "$2" ]] || return 1
  lowest=$(printf '%s\n%s\n' "$1" "$2" | sort -V)
  lowest=${lowest%%$'\n'*}
  [[ $lowest == "$1" ]]
}

# 每个组件只解析一次，并且只在它真的有命中时才查（干净的扫描不产生任何 API 请求）。
declare -A comp_state=()  # current | behind | unknown
declare -A comp_latest=() # latest-eligible tag（unknown 时为空）
declare -A comp_reason=() # behind / unknown 的阻塞原因
resolve_component() {
  local comp=$1 repo latest rc=0
  [[ -z ${comp_state[$comp]:-} ]] || return 0
  repo=VictoriaMetrics/${comp_label[$comp]}
  latest=$(bash "$pinned_releases" --latest-eligible "$repo") || rc=$?
  if ((rc != 0)); then
    comp_state[$comp]=unknown
    comp_latest[$comp]=
    comp_reason[$comp]="${comp_label[$comp]}: could not determine the latest eligible release of $repo (check-pinned-releases.sh exit $rc); failing closed"
  elif ! [[ $latest =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    comp_state[$comp]=unknown
    comp_latest[$comp]=
    comp_reason[$comp]="${comp_label[$comp]}: unexpected latest-eligible output for $repo: '$latest'; failing closed"
  elif version_lt "${comp_pin[$comp]}" "$latest"; then
    comp_state[$comp]=behind
    comp_latest[$comp]=$latest
    comp_reason[$comp]="${comp_label[$comp]}: pin ${comp_pin[$comp]} is behind latest-eligible $latest; upgrade the pin in scripts/download-storage-bins.sh"
  else
    comp_state[$comp]=current
    comp_latest[$comp]=$latest
    comp_reason[$comp]=
  fi
}

# 可选提示：上游已有比 pin 新、但还没满 N 天的稳定版时，说一句它哪天满 N 天（那天起这些命中会转成阻塞）。
# 需要额外一次 API 请求（releases/latest）；任何失败都只是不写这句话，不影响门禁结论。离线测试（设了
# LO_PINNED_RELEASES_FIXTURE）下不做。注意 releases/latest 是 GitHub 认定的最新 release，LTS 补丁线可能
# 排在前面，所以只在它是严格 vX.Y.Z、semver 大于 pin、并且确实还不满 N 天时才提示。
declare -A comp_hint=()
newer_hint() {
  local comp=$1 repo json tag published epoch token args
  [[ -z ${LO_PINNED_RELEASES_FIXTURE:-} ]] || return 0
  [[ -z ${comp_hint[$comp]+set} ]] || return 0
  comp_hint[$comp]=
  repo=VictoriaMetrics/${comp_label[$comp]}
  command -v curl >/dev/null 2>&1 || return 0
  token=${GH_TOKEN:-${GITHUB_TOKEN:-}}
  args=(--proto '=https' --tlsv1.2 -fsS --max-time 20 -H 'Accept: application/vnd.github+json')
  [[ -z $token ]] || args+=(-H "Authorization: Bearer $token")
  json=$(curl "${args[@]}" "https://api.github.com/repos/$repo/releases/latest" 2>/dev/null) || return 0
  tag=$(jq -r '.tag_name // empty' <<<"$json" 2>/dev/null) || return 0
  published=$(jq -r '.published_at // empty' <<<"$json" 2>/dev/null) || return 0
  [[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ && -n $published ]] || return 0
  version_lt "${comp_pin[$comp]}" "$tag" || return 0
  epoch=$(date -u -d "$published" +%s 2>/dev/null) || return 0
  (($(date +%s) - epoch < min_days * 86400)) || return 0
  comp_hint[$comp]="$repo $tag was published $published; it is eligible on $(date -u -d "@$((epoch + min_days * 86400))" +%Y-%m-%d), after which these findings block the release unless the pin is upgraded"
}

# ---------------------------------------------------------------------------
# 扫描
# ---------------------------------------------------------------------------

export LO_TRIVY_DIR=$tool_dir
trivy=$(bash "$root/scripts/install-trivy.sh")
"$trivy" --version

report_dir=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-storage-vulns.XXXXXX")
trap 'rm -rf "$report_dir"' EXIT HUP INT TERM

failed=()
blockers=()
# 放行（只警告）的命中，按 (组件, CVE, 包, installed, fixed) 合并，二进制与平台并成一列。
declare -A warn_bins=() warn_plats=()
warn_keys=()
for platform in "${platforms[@]}"; do
  dir=$scan_root/$platform
  report=$report_dir/$platform.json

  printf '== %s: %s\n' "$platform" "$dir"
  # 下载、校验 sha256、解包都交给既有脚本（GOOS/GOARCH 已给定，不需要 Go 工具链）。
  LO_STORAGE_BIN_DIR=$dir GOOS=${platform%-*} GOARCH=${platform#*-} \
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
  # 有命中时只认 --exit-code 给的 1；其他非 0 退出码说明 Trivy 自身出错，不能让这份报告走到只警告的放行分支。
  if ((rc != 1)); then
    printf 'trivy exited %s for %s with %d vulnerabilities in its report (expected 1 when there are findings); treating the scan as failed\n' \
      "$rc" "$platform" "$count" >&2
    exit 1
  fi

  # 把每条命中归到组件：pin 不落后于 latest-eligible 的放行（警告），其余阻塞。
  rows=$(jq -r '.Results[]? | .Target as $t | (.Vulnerabilities // [])[]
    | [$t, .PkgName, .InstalledVersion, .VulnerabilityID, .Severity, .FixedVersion] | @tsv' "$report" | sort)
  block_rows=
  blocked_comps=()
  warned=0
  while IFS=$'\t' read -r bin pkg installed vuln severity fixed; do
    [[ -n $bin ]] || continue
    comp=$(component_of "$bin") || {
      printf 'unexpected binary in the trivy report: %s\n' "$bin" >&2
      exit 1
    }
    resolve_component "$comp"
    if [[ ${comp_state[$comp]} == current ]]; then
      key=$comp$'\t'$vuln$'\t'$pkg$'\t'$installed$'\t'$fixed$'\t'$severity
      if [[ -z ${warn_bins[$key]+set} ]]; then
        warn_keys+=("$key")
        warn_bins[$key]=$bin
        warn_plats[$key]=$platform
      else
        [[ ",${warn_bins[$key]}," == *",$bin,"* ]] || warn_bins[$key]+=",$bin"
        [[ ",${warn_plats[$key]}," == *",$platform,"* ]] || warn_plats[$key]+=",$platform"
      fi
      warned=$((warned + 1))
    else
      block_rows+="$bin"$'\t'"$pkg"$'\t'"$installed"$'\t'"$vuln"$'\t'"$severity"$'\t'"$fixed"$'\n'
      [[ " ${blocked_comps[*]:-} " == *" $comp "* ]] || blocked_comps+=("$comp")
    fi
  done <<<"$rows"

  if [[ -z $block_rows ]]; then
    printf '%s: %d findings in %d binaries, all in components whose pinned version is not behind the latest eligible upstream release (warning only)\n' \
      "$platform" "$warned" "${#binaries[@]}"
    continue
  fi

  failed+=("$platform")
  block_count=$(grep -c . <<<"$block_rows" || true)
  printf '%s: %d fixable CRITICAL/HIGH findings block the release (%d more only warn)\n' "$platform" "$block_count" "$warned" >&2
  for comp in "${blocked_comps[@]}"; do
    resolve_component "$comp"
    printf 'blocked by: %s\n' "${comp_reason[$comp]}" >&2
    blockers+=("${comp_reason[$comp]}")
  done
  # 先给按 (CVE, 包) 去重的摘要：同一个 CVE 往往同时命中好几个二进制，逐行清单没人看得完。
  printf 'summary by vulnerability (CVE, package, severity, fixed in, affected binaries):\n' >&2
  jq -Rrn '[inputs | select(length > 0) | split("\t")
      | {id: .[3], pkg: .[1], sev: .[4], fixed: .[5], bin: .[0]}]
    | group_by([.id, .pkg])[]
    | [.[0].id, .[0].pkg, .[0].sev, .[0].fixed, ([.[].bin] | unique | join(","))] | @tsv' <<<"$block_rows" | sort >&2
  printf 'details per binary (binary, package, installed, vulnerability, severity, fixed in):\n' >&2
  printf '%s' "$block_rows" | sort >&2
done

# ---------------------------------------------------------------------------
# 放行项的警告：注解 + step summary
# ---------------------------------------------------------------------------

if ((${#warn_keys[@]} > 0)); then
  summary=${GITHUB_STEP_SUMMARY:-}
  if [[ -n $summary ]]; then
    {
      printf '### Embedded storage binary scan: findings not blocking the release\n\n'
      printf 'Upstream has no release older than %s days that fixes these yet, so they only warn. ' "$min_days"
      printf 'Once a newer upstream release is %s days old, they block the release until the pin is upgraded.\n\n' "$min_days"
      printf '| Component | Pin | Latest eligible | CVE | Package | Installed | Fixed in | Binaries | Platforms |\n'
      printf '| --- | --- | --- | --- | --- | --- | --- | --- | --- |\n'
    } >>"$summary"
  fi
  declare -A hinted=()
  while IFS=$'\t' read -r comp vuln pkg installed fixed severity; do
    key=$comp$'\t'$vuln$'\t'$pkg$'\t'$installed$'\t'$fixed$'\t'$severity
    printf '::warning title=Embedded %s %s: %s (%s)::%s in %s %s (fixed in %s) affects %s on %s; pin %s is not behind the latest eligible upstream release %s, so this does not block the release\n' \
      "${comp_label[$comp]}" "${comp_pin[$comp]}" "$vuln" "$severity" "$vuln" "$pkg" "$installed" "$fixed" \
      "${warn_bins[$key]}" "${warn_plats[$key]}" "${comp_pin[$comp]}" "${comp_latest[$comp]}"
    if [[ -n $summary ]]; then
      printf '| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n' "${comp_label[$comp]}" "${comp_pin[$comp]}" \
        "${comp_latest[$comp]}" "$vuln" "$pkg" "$installed" "$fixed" "${warn_bins[$key]//,/, }" \
        "${warn_plats[$key]//,/, }" >>"$summary"
    fi
  done < <(printf '%s\n' "${warn_keys[@]}" | sort)
  for comp in "${!comp_state[@]}"; do
    [[ ${comp_state[$comp]} == current && -z ${hinted[$comp]:-} ]] || continue
    hinted[$comp]=1
    newer_hint "$comp"
    if [[ -n ${comp_hint[$comp]:-} ]]; then
      printf '::notice title=Upcoming block for %s::%s\n' "${comp_label[$comp]}" "${comp_hint[$comp]}"
      [[ -z $summary ]] || printf '\n%s: %s.\n' "${comp_label[$comp]}" "${comp_hint[$comp]}" >>"$summary"
    fi
  done
fi

if ((${#failed[@]} > 0)); then
  if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
    {
      printf '\n### Embedded storage binary scan: blocking\n\n'
      printf '%s\n' "${blockers[@]}" | sort -u | sed 's/^/- /'
    } >>"$GITHUB_STEP_SUMMARY"
  fi
  printf 'storage binary scan failed for: %s\n' "${failed[*]}" >&2
  printf 'upgrade the pin of the component named above in scripts/download-storage-bins.sh, or add a justified,\n' >&2
  printf 'expiring exception to .trivyignore.yaml (see the header there).\n' >&2
  exit 1
fi
printf 'storage binary scan passed for: %s (%d findings only warn)\n' "${platforms[*]}" "${#warn_keys[@]}"
