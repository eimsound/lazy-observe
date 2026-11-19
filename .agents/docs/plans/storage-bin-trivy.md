# 发版前对嵌入的 Victoria 二进制做 Trivy 扫描

## 目标与来源

- 源需求：路线图供应链后续项「对 `.cache/storage-bin`（嵌入的 Victoria 二进制）跑 `trivy rootfs`」。
- 最终状态：发版链路（`release.yml`）在构建归档前，对 amd64、arm64 两个架构里被嵌入的四个 Victoria
  二进制（`victoria-metrics-prod`、`victoria-logs-prod`、`victoria-traces-prod`、`vmagent-prod`）
  各跑一遍 Trivy；有修复版本的 CRITICAL/HIGH 命中按「放行策略」判定（见下），参数与镜像扫描一致，共用 `.trivyignore.yaml`。
- 范围外：不升级 Victoria 版本（V3 负责，已合入 main），不加 `.trivyignore.yaml` 例外；
  不改 `requirements.md`、`.agents/skills/**`、`CHANGELOG.md`（走独立 changelog 切片）。

## 仓库证据

### 镜像扫描覆盖不到嵌入的二进制

- `Dockerfile` 终态镜像只有 `COPY --from=builder /out/lazy-observe /usr/local/bin/lazy-observe`、`/out/data`、
  `LICENSE`、`third_party/`。镜像里没有任何独立的 Victoria 可执行文件。
- 构建链路：`scripts/prepare-embedded-storage.sh` 把四个二进制 `gzip -9 -n` 成
  `internal/embed/bins/assets/<platform>/*.gz` 加 `manifest.tsv`，`internal/embed/bins/bins_linux_*.go`
  用 `//go:embed all:assets/<platform>` 编进 `lazy-observe`，运行时再解压到磁盘。
  GoReleaser 归档里的 `lazy-observe` 是同一个字节，所以 release 归档与镜像是同一个盲区。
- Trivy 扫 Go 可执行文件靠读 `runtime/debug` buildinfo（以 `\xff Go buildinf:` 魔数定位）。
  被嵌入的是 gzip 流，魔数不可见，`lazy-observe` 自己的 buildinfo 也只描述 lazy-observe 的依赖。
- 实测（没有拉镜像）：把 `victoria-logs-prod`、`vmagent-prod` 各 `gzip -9 -n` 之后放进一个目录跑
  `trivy rootfs --scanners vuln ...`，`Results` 为空（0 条）；同样的二进制未压缩时能被识别
  （`Type: gobinary`，四个二进制各一个 Result）。结论：镜像扫描与归档都看不到嵌入物里的 Go 依赖漏洞。

### Trivy 子命令

- `trivy rootfs <dir>` 与 `trivy fs` 都带 gobinary 分析器，对 `.cache/storage-bin/<platform>` 都能识别；
  选 `rootfs`（与路线图原话一致，且 rootfs 面向「已构建出的文件树」）。
- 目录布局：`prepare-embedded-storage.sh`、`mise run dev-storage` 用 `.cache/storage-bin/<platform>/`；
  `mise run storage-bins` 则把二进制放在 `.cache/storage-bin/` 根（宿主平台）。扫描脚本自己用
  `LO_STORAGE_BIN_DIR=.cache/storage-bin/<platform>` 调 `download-storage-bins.sh`（带 `GOOS`/`GOARCH`，
  所以不需要 Go 工具链），下载 + 校验 sha256 + 解包由已有脚本负责，扫描的就是将被嵌入的那批字节。
- 目录里还有 `.archives/`（tar.gz）与 `.manifest`；Trivy 不展开 tar.gz，实测只出现四个 gobinary Result。
- Trivy 读静态 buildinfo，不执行二进制，所以 amd64 runner 可以扫 arm64 二进制，一个 job 覆盖两个架构。

### Trivy 怎么安装、怎么固定

- 现状：`scripts/check-image-vulns.sh` 内联了安装逻辑——版本 0.76.2，`github.com/aquasecurity/trivy` release
  tar.gz 直接下载，按架构 pin sha256，装到 `.cache/trivy/<version>/trivy`；不用 `trivy-action`/`setup-trivy`
  （2026-03 tag 被劫持，GHSA-69fq-xp46-6x23）。
- 核实：`https://github.com/aquasecurity/trivy/releases/download/v0.76.2/trivy_0.76.2_checksums.txt`
  里 `Linux-64bit` = `bc0fc726…fdbf`、`Linux-ARM64` = `ab9d51f8…67c3`，与 `check-image-vulns.sh`
  现有常量一致；`api.github.com/.../releases/latest` 显示 v0.76.2 是最新且 `immutable: true`。
- 做法：把安装逻辑抽到 `scripts/install-trivy.sh`（stdout 只打可执行文件路径），
  `check-image-vulns.sh` 与新脚本都调用它，pin 只在一处，不会漂移（和 PR #62 把 mise 的 pin 收进
  `scripts/mise-release.env` 是同一思路）。

### 扫描放哪里

- `release.yml` 现有链路：`container-check`（双架构冒烟 + 镜像扫描）→ `performance-gates` → `archive`
  （`needs: [container-check, performance-gates]`）→ `docker`（`needs: archive`）。
- `ci.yml` 已有 `scripts/download-storage-bins.sh` → `go e2e ha`(+`build arm64`) 的分组，但漏洞库每天变，
  PR 上扫会让与改动无关的 PR 因新公告变红；镜像扫描也刻意只放在发版链路。
- `.trivyignore.yaml` 的到期日机制就是为「发版时重新被挡住」设计的，扫描点放在同一处最一致。

## 备选路线

### 路线 A（采用）：`release.yml` 新增独立 job `storage-bin-scan`，`archive` 依赖它

- 新增 `scripts/install-trivy.sh`、`scripts/check-storage-bin-vulns.sh`，以及 mise 任务
  `storage-bin-vulns`（本地入口）。
- job 在 `ubuntu-24.04` 上跑，不需要 Docker、Go、mise，只用 `bash scripts/check-storage-bin-vulns.sh`
  （需要 `curl`、`tar`、`sha256sum`、`jq`，runner 自带）。一个 job 扫 amd64 + arm64 两份二进制。
- `archive.needs` 加上 `storage-bin-scan`，`docker` 经 `archive` 传递依赖，所以 tag 发布与手动 snapshot
  都先过这一关；与 `container-check` 并行，不拉长发版链路。
- 失败策略与镜像扫描一致：`--severity CRITICAL,HIGH --ignore-unfixed --exit-code 1`，同一份
  `.trivyignore.yaml`。
- 优点：改动面小，不动 `ci.yml` 分组与 `ci-ok`；与镜像扫描同位同规则；不挤占 `container-check` 的 Docker 构建时间。
- 缺点：Victoria 升级 PR 本身不会因此变红，要到发版才发现；缓解办法是本地 `mise run storage-bin-vulns`
  （升级 pin 时自己跑，见 `CONTRIBUTING.md` 的 release 行）。

### 路线 B：把扫描放进 `container-check`，或在 `ci.yml` 里 `download-storage-bins.sh` 变化时也扫

- B1（塞进 `container-check` 矩阵）：每个架构原生 runner 各扫自己的那份，不需要跨架构；但该 job 本来就
  要做 Docker 构建，扫描结果得等构建完才出，且与「镜像」无关的检查混在一个 job 里，失败时更难读。
- B2（`ci.yml` 加 job，`scripts/download-storage-bins.sh` 变化时跑）：升级 PR 立即知道，但要动
  `scripts/ci-changes.sh` 分组、`ci-changes-test.sh`、`ci-ok` 汇总，而且漏洞库每天更新：Victoria
  钉在一个版本上不动时，B2 不会再扫，发版那天的新公告仍然只能靠 release 门禁。B2 不能替代 release 门禁，
  只能叠加在 A 上。

### 选择

选 A。理由：镜像扫描的价值也是「发版前挡住」，A 与它同规则、同位置，改动面最小；B2 可以以后叠加
（建议在 Victoria 升级自动化之后再做），不是这个切片的范围。

## 失败策略与防空转

- 参数与 `check-image-vulns.sh` 一致：`--scanners vuln --pkg-types os,library --severity CRITICAL,HIGH
  --ignore-unfixed --ignorefile .trivyignore.yaml --db-repository mirror.gcr.io/aquasec/trivy-db:2,ghcr.io/aquasecurity/trivy-db:2
  --cache-dir .cache/trivy/cache --skip-version-check --no-progress --exit-code 1`，目标改成目录。
- 防空转：JSON 报告里必须有且仅有四个 `gobinary` Result（目标名等于四个二进制名），缺一个就失败
  ——否则 Trivy 一旦不再识别这些二进制，门禁会悄悄变成永远绿。
- 扫描失败（命中、或 Trivy 自身出错）都以非零退出；命中时在 stderr 打 `二进制 / 包 / 当前版本 / 漏洞 / 严重度 / 修复版本` 清单。

### 放行策略（负责人决定：「允许发版，忽略上游 7 天内未解决的 CVE」）

我们不能自己重编 Victoria，能做的只有升级 pin，所以按组件判断「升级是否可能修掉」。Trivy Result 的 Target
映射到上游仓库与 pin：`victoria-metrics-prod`、`vmagent-prod` → `VictoriaMetrics/VictoriaMetrics`（`metrics_version`）；
`victoria-logs-prod` → `VictoriaMetrics/VictoriaLogs`（`logs_version`）；`victoria-traces-prod` → `VictoriaMetrics/VictoriaTraces`
（`traces_version`）。pin 从 `scripts/download-storage-bins.sh` 读（每个变量恰好一行 `vX.Y.Z`，读不出来即失败）。

latest-eligible = `scripts/check-pinned-releases.sh --latest-eligible OWNER/REPO`（R1 的 7 天年龄规则只在那个脚本里，不复制）：

| 情形 | 结论 |
| --- | --- |
| pin ≥ latest-eligible（semver） | 该组件的命中只警告，不阻塞：上游满 7 天的版本里还没有修复，没得升级 |
| pin < latest-eligible | 照常阻塞，并写明「pin vX 落后于 latest-eligible vY」：升级就可能修掉，逼着升级 |
| 查询退出 1（无合格 release）或 2（API 失败、限流、参数非法） | 阻塞（fail-closed），不能因 API 抖动放空门禁 |

- 不分严重度：CRITICAL 与 HIGH 同样处理（负责人原话未区分；要收紧是后续选项）。
- 现有 fail-closed 语义不变：扫描失败、gobinary 数量不对、`count==0 && rc!=0`、有命中但 `rc==0` 都失败。
- 只在组件真的有命中时才查 latest-eligible，每个组件只查一次；干净的扫描不产生任何 API 请求。
- 不做「下载 latest 再扫一遍」：判断「升级是否可能修掉」只需要版本号，不需要再扫一遍。
- 警告输出：每个放行的 (组件, CVE, 包) 一条 `::warning::` 注解，并写进 `$GITHUB_STEP_SUMMARY`（存在时）一张表
  （组件、pin、latest-eligible、CVE、包、installed、fixed、二进制、平台）。上游已有比 pin 新但不满 7 天的稳定版时，
  额外写一句 `::notice::`「vX 发布于 <日期>，<日期> 起这些命中会转成阻塞」。这句要多一次 `releases/latest` 请求，
  任何失败（网络、解析、非严格 vX.Y.Z、不比 pin 新、已满 7 天）都只是不写这句，不影响门禁结论；`releases/latest`
  是 GitHub 认定的最新 release（LTS 补丁线可能排在前面），所以对它做了「严格 vX.Y.Z、semver 大于 pin、确实不满 7 天」的过滤。
- 结论随时间变化：上游新版本满 7 天后，原先只警告的命中会转成阻塞——这不是 flaky，处理方式是升级 pin（CONTRIBUTING 写了）。
- `release.yml` 的扫描 step 带 `GH_TOKEN: ${{ github.token }}`（job 权限仍只有 `contents: read`）；R1 已处理
  aquasecurity 的 IP allow list 403 → 匿名重试，Victoria 仓库没有这个问题。`check-github-workflows.sh` 断言它存在。
- 测试注入：用 R1 的 `LO_PINNED_RELEASES_FIXTURE` / `LO_PINNED_RELEASES_NOW` 钩子（环境变量原样透传给
  `check-pinned-releases.sh`），不给扫描脚本另加 latest-eligible 注入点；pin 则从 `LO_STORAGE_BIN_DOWNLOADER`
  指向的文件读（默认就是 `download-storage-bins.sh`），所以桩下载器里写几行 `*_version=` 即可。
  `check-github-workflows.sh` 断言 `release.yml` 不出现这些测试钩子。

## 扫描结果（实测，Trivy 0.76.2，2026-11-19）

### 切片首版时（旧 pin：VM v1.147.0 / Logs v1.52.0 / Traces v0.9.4）

两个架构的结果逐行一致，共 63 条，全部 HIGH、全部已有修复版本：stdlib 每个二进制 13～14 条（上游用 Go 1.26.4/1.26.5 构建）、
`golang.org/x/net` v0.56.0（metrics、vmagent 各 2 条）、`github.com/klauspost/compress` v1.18.6（四个二进制各 1 条）。
把三个上游换成当时的最新 release 再扫（临时目录，未改 pin）：victoria-logs-prod 为 0 条，其余三个各剩 1 条
（stdlib v1.27.3，CVE-2026-97054，修复版本 1.26.11 / 1.27.4）。负责人的结论是「追较新的版本 + 加 min release age」，
由 V3（#72）升级到满 7 天的最新稳定版、R1（#70）加最小发布年龄门禁。

### 现在（rebase 到 V3 之后：VM v1.156.0 / Logs v1.53.0 / Traces v0.13.0，三者都是 latest-eligible）

`GH_TOKEN=$(gh auth token) bash scripts/check-storage-bin-vulns.sh` → exit 0，两个架构各 8 条命中、全部只警告：
VictoriaLogs v1.53.0（Go 1.27.1 的 stdlib）CVE-2026-111943、78667、78669、97031、97054；
VictoriaMetrics v1.156.0（victoria-metrics-prod、vmagent-prod）与 VictoriaTraces v0.13.0 各 1 条 CVE-2026-97054（stdlib v1.27.3，修复版本 1.26.11 / 1.27.4）。
VictoriaLogs v1.54.0 发布于 2026-11-13，2026-11-20 起满 7 天：那之后 Logs 的这 5 条会转成阻塞，需要把 `logs_version` 升到 v1.54.0（预期这是 `::notice::` 提示的内容）。

## 切片

单切片（一个 commit）：

1. `scripts/install-trivy.sh`：抽出 pin 与安装；`check-image-vulns.sh` 改为调用它。
2. `scripts/check-storage-bin-vulns.sh`：下载/校验（复用 `download-storage-bins.sh`）+ 双架构 `trivy rootfs` + 防空转检查 + 放行策略（见上）+ 命中清单（先按 CVE 去重的摘要，再逐二进制明细）；平台参数在安装 Trivy 之前统一校验；报告条数与退出码对不上一律判失败。离线测试 `scripts/check-storage-bin-vulns-test.sh`（桩 trivy、桩下载器、R1 的 fixture 钩子）接进 `mise run workflow-check`。
3. `mise.toml`：新增 `storage-bin-vulns` 任务。
4. `release.yml`：新增 `storage-bin-scan` job（扫描 step 带 `GH_TOKEN`），`archive.needs` 加上它。
5. `scripts/check-github-workflows.sh`：断言 `storage-bin-scan` 的 run 行精确等于该脚本（不能带 `|| true`）、job 无 `continue-on-error`、job 级与 step 级都没有 `if:`、step 带 `GH_TOKEN`、`release.yml` 不出现 `LO_STORAGE_BIN_DOWNLOADER` / `LO_PINNED_RELEASES_*` 测试钩子、`archive` 的单行 `needs: [...]` 列表含 `storage-bin-scan`（按元素匹配，不是子串）；
   `scripts/ci-changes-test.sh` 补两个新脚本的分组期望；`.trivyignore.yaml` 注释、`CONTRIBUTING.md` 的 release 行同步。

验证（green 命令）：`bash scripts/check-storage-bin-vulns-test.sh`（放行策略与 fail-closed 的离线用例）、`GH_TOKEN=$(gh auth token) bash scripts/check-storage-bin-vulns.sh`（按当前 main 的 pin 预期 exit 0、两个架构各 8 条只警告）、`mise run workflow-check`、`mise run ci-changes-test`、
`mise run release-prerequisites-test`、`mise run release-snapshot-check`、`bash -n`、`scripts/check-pipefail-pipelines.sh`。

## 执行日志

- 2026-11-19 `trivy rootfs`（0.76.2）对 amd64、arm64 各四个二进制 → 各识别 4 个 gobinary，各 63 条 HIGH（有修复），两个架构逐行一致。
- 2026-11-19 把四个二进制 `gzip -9 -n` 后扫 → `Results` 为 0 条（镜像扫描看不到嵌入物的依据）。
- 2026-11-19 `bash scripts/check-image-vulns.sh lazy-observe:s1-amd64`（本机已有镜像，重构后）→ exit 0，只识别 1 个 gobinary（lazy-observe 自身）。
- 2026-11-19 `bash scripts/check-storage-bin-vulns.sh` → exit 1，两个架构各 63 条，清单打在 stderr。
- 2026-11-19 临时给全部 17 个 CVE 写例外后重跑 → exit 0（验证例外生效与通过路径），随后 `git checkout .trivyignore.yaml` 还原，例外不在提交里。
- 2026-11-19 把 `vmagent-prod` 换成空壳再扫 → exit 1，「did not analyze exactly the expected Go binaries」（防空转生效）。
- 2026-11-19 `mise run workflow-check`、`mise run ci-changes-test`、`mise run release-prerequisites-test`、
  `mise run release-snapshot-check` → 均 exit 0；临时拿掉 `archive.needs` 里的 `storage-bin-scan`、或把 job 的 run 改成 `echo skip` → `check-github-workflows.sh` 均 exit 1。
- 2026-11-19（审查后修订）`check-storage-bin-vulns.sh`：`count==0` 且 `rc!=0` 改为失败；平台校验前移到装 Trivy 之前；
  命中清单前加按 (CVE, 包) 去重的摘要（两个架构各 17 行，原逐行明细各 63 行）。`bash scripts/check-storage-bin-vulns-test.sh` → PASS；
  临时把「rc!=0 即失败」改成 `if false` 后该测试 FAIL（`exit 1 with an empty report must fail`），已还原。
- 2026-11-19 `check-github-workflows.sh` 变异测试（每次测完还原 release.yml）：job 级 `continue-on-error: true`、step 级 `continue-on-error: true`、
  run 行加 `|| true`、job 加 `if: false`、`needs` 去掉 `storage-bin-scan`、`needs` 里写成 `storage-bin-scanx` → 均报错。
- 2026-11-19 跳过：给 `storage-bin-scan` 加只恢复不保存的 `actions/cache` 复用 `storage-all-` key。只恢复要用 `actions/cache/restore`，
  不在 workflow-check 的 action 白名单里（只允许 `actions/cache@<sha>`），还要加 `ci-cache-keys` 步骤和路径对齐，断言会变复杂；
  8 个 tar.gz 合计约 300MB，每次发版只下载一次，先不优化。
- 2026-11-19（V3/R1 合入后，负责人新策略）rebase 到 origin/main `84c88a5`：`mise.toml` 的 workflow-check 列表与 `ci-changes-test.sh` 的 expect 行两边各保留；
  `check-image-vulns.sh`/`install-trivy.sh` 带 `pins`，`check-storage-bin-vulns*.sh` 按 `scripts/*` 归 `go`；`mise run ci-changes-test` → PASS。
- 2026-11-19 实现放行策略：`check-storage-bin-vulns.sh` 按组件调 `check-pinned-releases.sh --latest-eligible`；
  `check-storage-bin-vulns-test.sh` 扩展为放行策略 + fail-closed 两组用例 → PASS。变异验证（每次测完还原脚本，`cmp` 确认一致）：
  把 `version_lt` 判落后改成 `!=`、落后永不阻塞、API 失败当作 current、未知状态不当阻塞、关掉「每个组件只查一次」缓存、关掉警告按二进制/平台合并 → 测试均以对应的 `FAIL:` 退出 1。
- 2026-11-19 `check-github-workflows.sh` 变异测试（非常量条件）：step 级 `if: github.event_name == 'schedule'`、`- if:` 写法、job 级 `if:`、
  删 `GH_TOKEN`、env 里加 `LO_STORAGE_BIN_DOWNLOADER` / `LO_PINNED_RELEASES_FIXTURE`、step 级 `continue-on-error` → 均报错，release.yml 已还原。
- 2026-11-19 真实扫描 `GH_TOKEN=$(gh auth token) bash scripts/check-storage-bin-vulns.sh` → exit 0，两个架构各 8 条只警告（见「扫描结果」）；
  用 /tmp 里的桩下载器把 metrics pin 临时写成 v1.147.0 再扫 → exit 1，`blocked by: VictoriaMetrics: pin v1.147.0 is behind latest-eligible v1.156.0`，
  Logs/Traces 的命中仍只警告。临时文件已删，没有改任何 pin。
- 2026-11-19（复审）报告里有命中但 Trivy 退出码不是 1（例如 2）时原先会落到只警告分支并通过；改为有命中时只认退出码 1，其他一律失败。
  离线测试加 `findings with exit 2`：修复前该用例 `FAIL: findings with exit 2: must fail but exited 0`，修复后 PASS。
- 2026-11-20（ADR 0016 改为只警告，取代上面「放行策略」一节）：负责人决定嵌入的 Victoria 只用上游官方二进制、绝不自行编译，
  扫描有命中只警告、不再随 pin 是否落后于 latest-eligible 阻塞；版本落后与否改由发版前的 Renovate Dependency Dashboard 判断。
  `check-storage-bin-vulns.sh` 删掉 latest-eligible 联动、组件到上游仓库的映射、`newer_hint`、`LO_STORAGE_BIN_DOWNLOADER` 与
  `LO_PINNED_RELEASES_*` 透传，`GH_TOKEN` 也不再需要；`check-storage-bin-vulns-test.sh` 删除。
  保留「没扫成就失败」：Trivy 出错、gobinary 不是恰好四个、有命中但退出码不是 1、无命中但退出码非 0、报告不可解析。
  `storage-bin-scan` 仍是 `archive` 的前置 job，`check-github-workflows.sh` 保留它不能被 `if:`/`continue-on-error` 绕过的断言，
  删除「要求 GH_TOKEN」「禁止测试钩子出现在 release.yml」两条。
- 2026-11-20（复审）命中清单原先经进程替换 `< <(jq …)` 读取，jq 在这里失败（例如字段是对象、`@tsv` 报错）不触发 `set -e`，
  命中被读成零行、落到 ok 分支 exit 0。改为先写进临时 TSV（pipefail 下 jq 失败即退出），并要求行数等于报告条数；
  空字段补 `-`，避免 tab 被 `read` 合并后字段错位、`warn_bins[$key]` 未绑定；step summary 单元格转义 `|`。
  一次性桩复核：修复前 `PkgName` 为对象时 exit 0，修复后 exit 5；`InstalledVersion` 为空时修复前 unbound variable 失败，修复后正常警告。
