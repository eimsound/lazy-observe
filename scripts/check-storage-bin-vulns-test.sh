#!/usr/bin/env bash
set -euo pipefail

# scripts/check-storage-bin-vulns.sh 的离线测试：桩 trivy、桩下载器（pin 也写在它里面）、假 GitHub API
# （check-pinned-releases.sh 自己的 LO_PINNED_RELEASES_FIXTURE / LO_PINNED_RELEASES_NOW 钩子），不联网。
# 钉住两类规则：
#   1. 放行策略：pin >= latest-eligible 的命中只警告，pin 落后或查询 latest-eligible 失败（退出 1/2）一律阻塞；
#   2. 「不能悄悄判通过」：报告与退出码互相印证、必须识别出四个 gobinary、平台参数先于安装校验。

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
script=$root/scripts/check-storage-bin-vulns.sh
work=$(mktemp -d "${TMPDIR:-/tmp}/lazy-observe-storage-vulns-test.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# 当前时间固定为 2026-11-19T12:00:00Z；没有 token，也不让环境里的真实 token 漏进测试。
now=$(date -u -d 2026-11-19T12:00:00Z +%s)
export LO_PINNED_RELEASES_NOW=$now
unset GH_TOKEN GITHUB_TOKEN GITHUB_STEP_SUMMARY

mkdir -p "$work/trivy/0.76.2" "$work/scan/linux-amd64" "$work/scan/linux-arm64"
# trivy 桩：STUB_VULN_BINS（逗号分隔）里的二进制各带一条 HIGH 命中；STUB_MISSING 指定的二进制不出现在报告里；STUB_RC 是退出码。
cat >"$work/trivy/0.76.2/trivy" <<'STUB'
#!/usr/bin/env bash
[[ ${1:-} == --version ]] && { echo 'Version: stub'; exit 0; }
out=
while (($#)); do
  [[ $1 == --output ]] && out=$2
  shift
done
vuln='"Vulnerabilities":[{"VulnerabilityID":"CVE-2099-0001","PkgName":"stdlib","InstalledVersion":"v1.0.0","Severity":"HIGH","FixedVersion":"1.0.1"}]'
results=
for bin in victoria-metrics-prod victoria-logs-prod victoria-traces-prod vmagent-prod; do
  [[ ${STUB_MISSING:-} != "$bin" ]] || continue
  extra=
  [[ ",${STUB_VULN_BINS:-}," != *",$bin,"* ]] || extra=",$vuln"
  results+="${results:+,}{\"Target\":\"$bin\",\"Type\":\"gobinary\"$extra}"
done
printf '{"SchemaVersion":2,"Results":[%s]}' "$results" >"$out"
exit "${STUB_RC:-0}"
STUB
chmod +x "$work/trivy/0.76.2/trivy"

# 下载器桩：不下载，只承载 pin（真实脚本负责下载与 sha256 校验，不在这里测）。
set_pins() {
  printf '#!/usr/bin/env bash\nexit 0\nmetrics_version=%s\nlogs_version=%s\ntraces_version=%s\n' "$1" "$2" "$3" \
    >"$work/downloader.sh"
}

# 假 GitHub API：release REPO TAG DAYS_AGO，发布于「当前时间 - DAYS_AGO 天」。
fx=$work/api
reset_api() {
  rm -rf "$fx"
  mkdir -p "$fx"
}
release() {
  local repo=$1 tag=$2 days=$3 dir published
  dir=$fx/${repo//\//__}
  mkdir -p "$dir"
  published=$(date -u -d "@$((now - days * 86400))" +%Y-%m-%dT%H:%M:%SZ)
  [[ -f $dir/releases.json ]] || printf '[]\n' >"$dir/releases.json"
  jq --arg tag "$tag" --arg published "$published" \
    '. + [{tag_name: $tag, published_at: $published, prerelease: false, draft: false}]' "$dir/releases.json" >"$dir/releases.tmp"
  mv "$dir/releases.tmp" "$dir/releases.json"
}
# 基线：每个仓库 latest-eligible 分别是 v1.12.0 / v1.20.0 / v0.30.0；VictoriaMetrics 另有 3 天前的 v1.13.0（不满 7 天）。
baseline_api() {
  reset_api
  release VictoriaMetrics/VictoriaMetrics v1.10.0 100
  release VictoriaMetrics/VictoriaMetrics v1.12.0 30
  release VictoriaMetrics/VictoriaMetrics v1.13.0 3
  release VictoriaMetrics/VictoriaLogs v1.19.0 100
  release VictoriaMetrics/VictoriaLogs v1.20.0 30
  release VictoriaMetrics/VictoriaTraces v0.30.0 100
}

# run VULN_BINS RC [args...]：stdout 存 $work/out，stderr 存 $work/err，返回脚本退出码。
run() {
  local vulns=$1 rc=$2 status=0
  shift 2
  STUB_VULN_BINS=$vulns STUB_RC=$rc LO_TRIVY_DIR=$work/trivy LO_STORAGE_BIN_SCAN_DIR=$work/scan \
    LO_STORAGE_BIN_DOWNLOADER=$work/downloader.sh LO_PINNED_RELEASES_FIXTURE=$fx \
    LO_PINNED_RELEASES_FIXTURE_LOG=$work/api.log bash "$script" "$@" >"$work/out" 2>"$work/err" || status=$?
  return "$status"
}
expect_pass() { # 描述 VULN_BINS RC [args...]
  local what=$1 status=0
  shift
  run "$@" || status=$?
  [[ $status -eq 0 ]] || fail "$what: must pass (got $status): $(cat "$work/err")"
}
expect_fail() { # 描述 VULN_BINS RC [args...]
  local what=$1 status=0
  shift
  run "$@" || status=$?
  [[ $status -ne 0 ]] || fail "$what: must fail but exited 0: $(cat "$work/out")"
}
has() { # 文件 片段 描述
  grep -Fq -- "$2" "$1" || fail "$3: expected '$2' in $(basename "$1"): $(cat "$1")"
}
hasnt() {
  ! grep -Fq -- "$2" "$1" || fail "$3: did not expect '$2' in $(basename "$1"): $(cat "$1")"
}

baseline_api

# ---------------------------------------------------------------------------
# 放行策略
# ---------------------------------------------------------------------------

# pin == latest-eligible 且有命中 → 通过，输出 warning；step summary 里有表。
set_pins v1.12.0 v1.20.0 v0.30.0
summary=$work/step-summary.md
: >"$summary"
status=0
GITHUB_STEP_SUMMARY=$summary run victoria-metrics-prod,vmagent-prod,victoria-logs-prod 1 linux-amd64 linux-arm64 || status=$?
[[ $status -eq 0 ]] || fail "pin == latest-eligible must pass with warnings (got $status): $(cat "$work/err")"
has "$work/out" '::warning title=Embedded VictoriaMetrics v1.12.0: CVE-2099-0001 (HIGH)::' 'metrics warning'
has "$work/out" '::warning title=Embedded VictoriaLogs v1.20.0: CVE-2099-0001 (HIGH)::' 'logs warning'
has "$work/out" 'victoria-metrics-prod,vmagent-prod on linux-amd64,linux-arm64' 'warning merges binaries and platforms'
[[ $(grep -c '^::warning' "$work/out") -eq 2 ]] || fail "warnings must be deduplicated per component/CVE/package: $(cat "$work/out")"
hasnt "$work/out" 'VictoriaTraces' 'traces has no findings'
has "$summary" '| Component | Pin | Latest eligible | CVE | Package | Installed | Fixed in |' 'step summary header'
has "$summary" '| VictoriaMetrics | v1.12.0 | v1.12.0 | CVE-2099-0001 | stdlib | v1.0.0 | 1.0.1 |' 'step summary metrics row'
has "$summary" '| VictoriaLogs | v1.20.0 | v1.20.0 | CVE-2099-0001 | stdlib | v1.0.0 | 1.0.1 |' 'step summary logs row'
hasnt "$work/err" 'blocked by' 'warn-only run must not report blockers'

# pin > latest-eligible（上游有不满 7 天的新版本，pin 已经在它上面）→ 警告，不阻塞。
set_pins v1.13.0 v1.20.0 v0.30.0
expect_pass 'pin newer than latest-eligible' victoria-metrics-prod 1 linux-amd64
has "$work/out" '::warning title=Embedded VictoriaMetrics v1.13.0' 'newer-than-eligible pin only warns'

# pin < latest-eligible 且有命中 → 阻塞，写明落后；没有任何放行的警告。
set_pins v1.10.0 v1.20.0 v0.30.0
expect_fail 'pin behind latest-eligible' victoria-metrics-prod 1 linux-amd64
has "$work/err" 'blocked by: VictoriaMetrics: pin v1.10.0 is behind latest-eligible v1.12.0' 'behind reason'
hasnt "$work/out" '::warning' 'a blocked-only run has nothing to warn about'
has "$work/err" 'summary by vulnerability' 'blocking findings come with the per-CVE summary'

# 混合：metrics 落后有命中、logs 最新有命中 → 阻塞，且只有落后的组件被列为阻塞原因，logs 的命中仍以警告形式出现。
set_pins v1.10.0 v1.20.0 v0.30.0
expect_fail 'mixed behind and current' victoria-metrics-prod,victoria-logs-prod 1 linux-amd64
has "$work/err" 'blocked by: VictoriaMetrics: pin v1.10.0 is behind' 'mixed: metrics blocks'
hasnt "$work/err" 'blocked by: VictoriaLogs' 'mixed: logs is not a blocker'
has "$work/out" '::warning title=Embedded VictoriaLogs v1.20.0' 'mixed: logs finding still warns'
hasnt "$work/err" $'victoria-logs-prod\tstdlib' 'mixed: logs rows are not in the blocking details'

# latest-eligible 查询失败 → 阻塞（fail-closed）。
set_pins v1.12.0 v1.20.0 v0.30.0
printf '[]\n' >"$fx/VictoriaMetrics__VictoriaTraces/releases.json" # 没有任何合格 release → 退出 1
expect_fail 'latest-eligible exit 1' victoria-traces-prod 1 linux-amd64
has "$work/err" 'VictoriaTraces: could not determine the latest eligible release of VictoriaMetrics/VictoriaTraces (check-pinned-releases.sh exit 1)' 'exit 1 blocks'
baseline_api
printf '500\n\n{"message":"boom"}' >"$fx/VictoriaMetrics__VictoriaLogs/releases.json.anon-error"
printf '500\n\n{"message":"boom"}' >"$fx/VictoriaMetrics__VictoriaLogs/releases.json.auth-error"
expect_fail 'latest-eligible exit 2' victoria-logs-prod 1 linux-amd64
has "$work/err" 'VictoriaLogs: could not determine the latest eligible release of VictoriaMetrics/VictoriaLogs (check-pinned-releases.sh exit 2)' 'exit 2 blocks'
baseline_api

# 没有命中就不查 API：干净的扫描即使 API 全坏也通过，并且一次请求都没发。
rm -f "$work/api.log"
printf '500\n\n{"message":"boom"}' >"$fx/VictoriaMetrics__VictoriaLogs/releases.json.anon-error"
expect_pass 'clean scan needs no API' '' 0 linux-amd64 linux-arm64
[[ ! -s $work/api.log ]] || fail "a clean scan must not query the GitHub API: $(cat "$work/api.log")"
baseline_api

# 每个组件只查一次：metrics 与 vmagent 都有命中，两个平台各扫一遍，仍然只有一次 releases 请求。
rm -f "$work/api.log"
set_pins v1.12.0 v1.20.0 v0.30.0
expect_pass 'one lookup per component' victoria-metrics-prod,vmagent-prod 1 linux-amd64 linux-arm64
[[ $(grep -c 'VictoriaMetrics/VictoriaMetrics/releases' "$work/api.log") -eq 1 ]] ||
  fail "latest-eligible must be looked up once per component: $(cat "$work/api.log")"

# pin 读不出来（缺行）→ 失败，不能当作没有 pin。
printf '#!/usr/bin/env bash\nexit 0\nmetrics_version=v1.12.0\nlogs_version=v1.20.0\n' >"$work/downloader.sh"
expect_fail 'missing traces pin' victoria-traces-prod 1 linux-amd64
has "$work/err" 'must contain exactly one traces_version=vX.Y.Z line' 'unreadable pin'

# ---------------------------------------------------------------------------
# 不能悄悄判通过（放行策略之外的 fail-closed 语义）
# ---------------------------------------------------------------------------

set_pins v1.12.0 v1.20.0 v0.30.0
expect_pass 'clean report with exit 0' '' 0 linux-amd64 linux-arm64

expect_fail 'exit 1 with an empty report' '' 1 linux-amd64
has "$work/err" 'treating the scan as failed' 'empty report with exit 1 must say why'

status=0
STUB_MISSING=vmagent-prod run '' 0 linux-amd64 || status=$?
[[ $status -ne 0 ]] || fail 'a report that analyzed only three of the four binaries must fail'
has "$work/err" 'did not analyze exactly the expected Go binaries' 'missing binary must say why'

# 报告里有命中但 Trivy 退出 0：即使 pin 都是最新也必须失败（报告与退出码对不上）。
expect_fail 'findings with exit 0' victoria-metrics-prod 0 linux-amd64
has "$work/err" 'but exited 0' 'findings with exit 0'

# 报告里有命中但 Trivy 退出码不是 --exit-code 给的 1：同样是报告与退出码对不上，pin 都是最新也必须失败，不能落到只警告。
expect_fail 'findings with exit 2' victoria-metrics-prod 2 linux-amd64
has "$work/err" 'expected 1 when there are findings' 'findings with an unexpected exit code'

# 阻塞时的摘要按 (CVE, 包) 去重：同一个 CVE 命中两个二进制只占一行，且二进制名在同一行里。
set_pins v1.10.0 v1.19.0 v0.30.0
expect_fail 'dedup summary' victoria-metrics-prod,vmagent-prod 1 linux-amd64
lines=$(grep -c $'^CVE-2099-0001\tstdlib\tHIGH\t1.0.1\t' "$work/err" || true)
[[ $lines -eq 1 ]] || fail "summary must hold one deduplicated line per CVE and package, got $lines: $(cat "$work/err")"
has "$work/err" $'\tvictoria-metrics-prod,vmagent-prod' 'summary must list the affected binaries'

# 非法平台要在安装 Trivy / 扫描任何平台之前报错：用一个空的 Trivy 目录证明没有安装动作发生。
mkdir "$work/empty-trivy"
status=0
LO_TRIVY_DIR=$work/empty-trivy LO_STORAGE_BIN_SCAN_DIR=$work/scan LO_STORAGE_BIN_DOWNLOADER=$work/downloader.sh \
  bash "$script" linux-amd64 linux-foo >/dev/null 2>"$work/err" || status=$?
[[ $status -ne 0 ]] || fail 'an unsupported platform must fail'
has "$work/err" 'unsupported storage binary platform: linux-foo' 'bad platform must say why'
[[ -z $(ls -A "$work/empty-trivy") ]] || fail 'a bad platform must be rejected before Trivy is installed'

printf 'PASS: check-storage-bin-vulns warns only for components at the latest eligible upstream release and fails closed otherwise\n'
