# Renovate 替代 Dependabot 与 pinned-releases

决策见 ADR 0015；本文是方案、各 manager 的要点、负责人操作和 PR 审查清单。配置在 `.github/renovate.json`。
依据是 2026-11-20 的调研（Renovate 44.253.4 本地 `--platform=local` 的 extract / lookup / full dry-run、`renovatebot/renovate` 文档与源码）。
Mend 托管 App 上的真实行为本地验证不了，见末尾「没验证的」。

## 目标

- 一个 bot 管全部依赖：清单类走原生 manager，写在脚本里的手工固定点走 `customManagers`；
- 统一最小发布年龄 7 天（`scripts/release-age.env` 是单一来源）；
- 删除 `dependabot.yml`、`pinned-releases.yml`、`ci.yml` 的 `pinned-releases` job、`scripts/check-pinned-releases*.sh` 及 mise 任务；
- 范围外：不改任何依赖的版本；不改 `requirements.md`；不在仓库 CI 里运行 Renovate（它是 AGPL-3.0，只用 Mend 托管 App，本地一次性校验不进仓库）。

## 方案

### sha256 随版本一起升

Renovate 的正则 manager 一个依赖 = 一个 `(currentValue, currentDigest)` 对，两者必须出现在同一个匹配里。datasource 选 `github-release-attachments`：
它按 `currentDigest` 在旧 release 里找出对应资产，解析 `SHASUMS256.txt` / `*_checksums.txt`（小于 5 KiB 的资产才当清单解析），再给出新 release 里同名资产的 digest，
托管 App 直接可用，不需要 `postUpgradeTasks`。实测 mise v2026.11.7→v2026.11.10、VictoriaLogs v1.53.0→v1.54.0 给出的 `newDigest` 与上游清单一致。

因此固定点布局做了最小调整：**每个 sha 行行尾加 `# <release tag>`**（`scripts/mise-release.env`、`scripts/download-storage-bins.sh`、`scripts/install-trivy.sh`），
版本变量单独做一个无 digest 的依赖。所有匹配互不重叠，每个依赖的替换目标是一整行；行尾注释对 bash 无影响，消费脚本零改动。
`*_version` 变量行不加行尾注释（`scripts/third-party-notices.sh` 按整行读取）。Trivy 的版本变量没有 `v` 前缀而 tag 有，版本依赖用 `extractVersionTemplate: ^v(?<version>.+)$`。
同一个 release 的多个依赖用 `groupName` 合成一个 PR（mise bootstrap、trivy、victoria-metrics、victoria-logs、victoria-traces）。

已知坑：

- `getDigest` 返回 `null` 时 Renovate 只打一条警告，**更新仍保留**，PR 里会出现「版本变了、sha 没变」。兜底是 CI 自己变红：mise 引导（每个 job 的 `ci-setup-mise.sh`、Dockerfile）和 Victoria 二进制（e2e 等 job）的 sha 在现有 PR CI 里本来就会下载并校验；Trivy 靠新增的 `trivy-pin` job。
- mise 的 `SHASUMS256.txt` 约 3.7 KB，距 5 KiB 阈值不远；超过后实现退化成「下载全部资产自己算」，等于信任首次下载，托管版可能很慢。看到 mise 固定点 PR 超时或 digest 来源可疑时先查这里。
- 清单命中时 digest 来自上游同一个 release 的清单，信任模型与过去人工抄 `SHASUMS256.txt` 相同；清单不命中时退化为 Renovate 自己下载算，PR 里看不出走了哪条路。

### Trivy 与 `trivy-pin` job

Trivy 继续用 `scripts/install-trivy.sh` 下载，**不用** trivy-action / setup-trivy（2026-03 tag 被劫持，头注释保留原因）。
过去升级时人工执行 `gh release verify`，现在改由 Renovate 升级，靠 7 天最小发布年龄和 sha256 校验；`gh release verify` 要调 API，会被 aquasecurity 的 IP allow list 拦住，不进 CI。

Trivy 的 sha 只在 `release.yml` 的扫描里用，PR CI 其它 job 不会校验，所以 `ci-changes.sh` 在 `scripts/install-trivy.sh` 改动时打开 `trivy_pin` 标志，
`ci.yml` 的 `trivy-pin` job 在 amd64 与 arm64 runner 上各跑一次 `bash scripts/install-trivy.sh`（下载加 `sha256sum --check`；脚本按 `uname -m` 选 sha，两个 sha 行在 Renovate 里是两个独立依赖，可能只错一个），不查 API、不算年龄、不要 token，计入 `ci-ok`。

三处 sha 的 PR 兜底（`ci-changes.sh` 分组已核对）：`scripts/mise-release.env` 走全量，每个 job 的 `ci-setup-mise.sh` 都重新下载校验（`~/.local/bin` 不在缓存里），arm64 由 `go-cross`/`artifact-smoke` 的 arm64 runner 和 Dockerfile 覆盖；`scripts/download-storage-bins.sh` 触发 e2e、HA E2E、sdk-journeys 与打包链路，`go-cross` 的 amd64/arm64 都会下载并校验（缓存命中也重算 sha）；`scripts/install-trivy.sh` 由 `trivy-pin` 两个架构覆盖。

### 各 manager 要点

| manager | 文件 | 要点 |
| --- | --- | --- |
| gomod | `go.mod`、`sdk/go/go.mod`、`entc.mod` | `managerFilePatterns` 写成 `["/(^|/)go\\.mod$/","/(^|/)entc\\.mod$/"]` 才会识别 `entc.mod`。`golang` depType（`go` 指令）关掉：`sdk/go` 是库，抬 `go` 指令等于抬使用方下限。`github.com/eimsound/lazy-observe/sdk/go`（`replace => ./sdk/go`）关掉。`entgo.io/**` 在 go.mod 与 entc.mod 同一 PR（组 `ent`，实测分支 `renovate/ent` 同时改两个文件；该规则必须排在所有 `gomod minor-patch` 规则之后，否则后者覆盖 groupName，ent 会被并进大组）；根模块与 `sdk/go` 的 `go.opentelemetry.io/**` 各自同批（组 `opentelemetry`、`opentelemetry (sdk/go)`，同样排在 minor-patch 之后）；其余 minor/patch 一组、major 单独。`postUpdateOptions: ["gomodTidy"]`。周更，不设 Dashboard 批准。 |
| npm | `web`、`sdk/node`、`sdk/browser`（含各 examples，同一 lockfile） | 每个工程 minor+patch 一组、major 单独（`matchFileNames`）。`:ignoreModulesAndTests` 预设去掉，否则 examples 被忽略。`@types/node` 上界 `<27.0.0` 跟随 `mise.toml` 的 node 主版本，升 node 主版本时同步改；`sdk/browser` 的 `unplugin` 上界 `<3.0.0`。`pnpm` 在 `mise.toml` 与三个 `packageManager` 里同一 PR（组 `pnpm`，排在 npm 与 mise 的 minor-patch 规则之后，否则 `mise.toml` 里的 pnpm 被并进 mise tools 组）。 |
| pep621 | `sdk/python/pyproject.toml` | `rangeStrategy: widen`：库里的 `>=` 下限是对使用方的兼容承诺，现有范围仍满足时不开 PR。代价是 Renovate 对 Python 基本空转（只有新版本让范围不再满足时才动），Dependabot 之前也是这样。minor/patch 一组、major 单独。运行时 7 天由 `[tool.uv] exclude-newer = "7 days"` 保证（#119）。 |
| github-actions | 5 个 workflow | `helpers:pinGitHubActionDigestsToSemver`：保持 SHA 固定，行尾 `# vX.Y.Z` 注释与 SHA 一起改。`github-runner` depType 关掉（`ubuntu-24.04` 会被提 26.04）。minor/patch/digest 一组、major 单独。 |
| dockerfile | `Dockerfile` | `FROM` 的 tag 与 digest 原生更新；minor/patch/digest 一组。浮动 tag（`debian:trixie-slim`）会提 digest 更新。gcr distroless 与 Docker Hub 的 debian 拿不到时间戳，见下。 |
| mise | `mise.toml` 的 `[tools]` | 原生识别，minor/patch 一组、major 单独；go 后端（`go:…/golines`、`govulncheck`）与 `{ version = "…", lazy = true }` 的内联表写法也识别。工具版本只在 `[tools]` 写一次，没有别的重复固定点，合并 mise tools PR 不用再手工同步，见下节「mise 工具版本的单一来源」。 |
| custom.regex | mise 引导、Victoria 三件套、Trivy、e2e 镜像、BuildKit | 见上。e2e 镜像用一条正则 + `docker` datasource，原生更新 tag 与 digest；BuildKit 的 `driver-opts: image=moby/buildkit:tag@digest` 同理。手工固定点不设周排期（`schedule: at any time`），满 7 天就开 PR。 |

### mise 工具版本的单一来源

Renovate 的 mise manager 只改 `mise.toml`，所以工具版本只在 `mise.toml` 的 `[tools]` 写一次，其余地方只写工具名：

- `scripts/ci-setup-mise.sh` 的 profile 是工具名清单，用 `mise install <名字>` 装；mise 对「已在配置里的名字」装配置的那一版，不是 latest。
  `MISE_CI_TOOLS` 仍可写 `node@22.23.5` 这样的显式版本（SDK 的 Node 矩阵用它）。
- `ci.yml` 的 `mise x go uv -- …` 同理，取 `[tools]` 的 uv。
- 任务不再写 `tools.*`：任务级 `tools` 只在 `mise run` 时才被解析，`mise install uv` 看不到，会装成 latest。
- 原来任务级才有的 uv、golangci-lint、helm、goreleaser、go-licenses、govulncheck 搬进 `[tools]` 并标 `lazy = true`。
  lazy 工具不会被裸 `mise install` 和 `mise run` 预装，只在 profile 按名字装、或任务/命令真正用到时才装，
  所以每个 CI job 仍然只装自己那份；不标 lazy 的话，每个 `mise run` 都会装全量，`go install` 编译的 go-licenses、govulncheck 会拖慢所有 job。
- `lazy = true` 需要 mise 2026.9 起，`min_version` 因此设为 2026.10.13（修了「任务装别的工具时丢 lazy shim」）。CI 与 Dockerfile 用的引导版本更新，不受影响；
  本地 mise 低于该版本时 mise 直接拒绝加载配置，`README.md`、`README.zh-CN.md`、`CONTRIBUTING.md` 写的最低版本同步改了。
- `mise install <名字>` 对不在配置里的名字会**静默装 latest**，mise 没有「拒装未配置工具」的选项（`--locked` 只要求 lockfile 里有下载 URL，本仓库没有 lockfile）。
  所以 `ci-setup-mise.sh` 在安装前后各做一次 fail-closed 校验（`mise tool <名字> --config-source/--requested/--installed`）：
  裸名字的版本必须来自本仓库 `mise.toml`（不认 `MISE_<TOOL>_VERSION`、`mise.local.toml`、全局配置）且是精确版本，profile 里不许写 `名字@版本`，
  `MISE_CI_TOOLS` 的 `名字@版本` 必须是精确版本，同一工具不许请求两次（`mise install node node@22.23.5` 只装 22.23.5，裸 `node` 被悄悄丢掉，SDK Node 矩阵因此改成 `pnpm node@<矩阵>`）；装完再确认装上的就是那一版。任一不满足即退出 1，安装前那次失败时不会调用 `mise install`。
  `scripts/ci-setup-mise-test.sh`（`workflow-check` 的一部分）守「未固定的工具必须失败且不安装」。新增 profile 工具时先写进 `[tools]`。
- Renovate 对内联表的支持已实测（44.253.4，2026-11-20）：`--dry-run=extract` 识别全部 6 个 `{ version = …, lazy = true }` 工具且 `currentValue` 正确；
  `--dry-run=lookup` 给出 uv、golangci-lint、helm、goreleaser 的 minor/patch 更新并归入 `renovate/mise-tools-minor-patch`，helm 4 单独成 `renovate/helm-4.x`，
  govulncheck 按 `timestamp-optional` 规则单独成 PR；mise manager 没有自定义 `updateDependency`，用 Renovate 自身的 `doAutoReplace` 对内联表改写，6 个工具都只改了 `version` 字段、重新解析版本正确。

文档（`CONTRIBUTING.md`、脚本注释）里也不再抄版本号，写「版本见 `mise.toml`」。

剩余的、不在这套机制里的版本抄写：`ci.yml` 的 `sdk-node`、`sdk-browser` 矩阵里的 `26.13.1`（刻意写成 patch；26 那一格与 `[tools]` 的 node 同值，升 node 时要一起改，这是 SDK 兼容矩阵，不是工具安装）。

### 最小发布年龄

- 顶层 `minimumReleaseAge: "7 days"`，`minimumReleaseAgeBehaviour: timestamp-required`（默认值，显式写出），`internalChecksFilter: strict`，`prCreation: not-pending`，`internalChecksAsSuccess: true`。
  `internalChecksAsSuccess` 必须写：`ci.yml` 只在 `pull_request` 上跑，Renovate 分支没有检查，缺了它 PR 创建会被无限期推迟。
- pnpm 自己的 `minimumReleaseAge`（分钟，`pnpm-workspace.yaml`）与 Renovate 的值各自生效，Renovate 不读 pnpm 配置。伴生包晚发布由默认 30 分钟 `minimumReleaseAgeBuffer` 兜住。
  没有开实验性的 `minimumReleaseAgePackageManagerSync`。
- `vulnerabilityAlerts` 开启并显式写 `minimumReleaseAge: "7 days"`（默认 `null`，安全更新不等，会开出 pnpm 装不上的 PR）；`osvVulnerabilityAlerts: false`。
  安全更新「插队」：无视 `prConcurrentLimit` 和 `schedule`。因为 `vulnerabilityAlerts.prCreation` 默认 `immediate`，年龄闸靠 `internalChecksFilter: strict` 在选版本阶段起作用。
- `scripts/check-github-workflows.sh` 校验 `release-age.env`、`renovate.json`（顶层与 `vulnerabilityAlerts` 两处，且每个 `minimumReleaseAge` 都等于它）、各 pnpm 工程（分钟）、各 `pyproject.toml` 的 `exclude-newer` 一致；另校验 `renovate.json` 里的 `internalChecksFilter` 全是 `strict`（`flexible`/`none` 会在所有新版本都不满 7 天时照样开 PR）。
- **没有时间戳的依赖**：GHCR、Quay、GCR 不提供发布时间；Docker Hub 上标签超过 1000 个的仓库在 Renovate 44.253.4 下也拿不到
  （Renovate 请求 `page_size=1000`，Hub 每页只给 100 条，第 11 页起返回 403，整次查询回退成无时间戳。2026-11-20 的标签数：debian 3090、seaweedfs 4345、postgres 1469、
  Victoria 集群镜像约 1900；`valkey/valkey` 353、`moby/buildkit` 381、`docker/dockerfile` 172 正常）。
  这些依赖（Quay 的 vmstorage/vminsert/vmselect、seaweedfs、postgres、`govulncheck`、gcr distroless、Dockerfile 的 debian）统一 `minimumReleaseAgeBehaviour: timestamp-optional`，
  有新版就自动开 PR，不设 `dependencyDashboardApproval`，每个依赖单独成 PR；它们不受 7 天限制，与主流做法一致（Renovate 的 `config:best-practices` 也只对 npm 做年龄限制）。
  Victoria 集群镜像在 Docker Hub 有同 tag、同 index digest 的发布（逐个比对一致），所以将来 Renovate 修好 Hub 的分页后，可以把 e2e 改到 Docker Hub 恢复 7 天检查。
- postgres、valkey 是 e2e 夹具，规则里关掉 major，只升 minor/patch。

### 其它写法

- Renovate 44 用 `managerFilePatterns`（旧名 `fileMatch`），值是 `/regex/` 字符串；`dependencyDashboard` 必须写成对象 `{"enabled": true}`，布尔写法 `--strict` 会报需要迁移。
- 提交信息 `chore: update dependency x to v1.2.3`：`semanticCommits: enabled`、`semanticCommitType: chore`、`semanticCommitScope: null`，并去掉 `:semanticPrefixFixDepsChoreOthers`。
- 排期 `timezone: Asia/Shanghai`、`schedule: ["after 3am on monday"]`；Mend 免费托管每 4 小时跑一轮。`prConcurrentLimit: 10`，`prHourlyLimit: 0`。
- `enabledManagers` 显式列出（github-actions、gomod、npm、pep621、dockerfile、mise、custom.regex），避免托管 App 的默认集合打开 `helm-values` 等不需要的 manager。
- `packageRules` 按顺序合并，后面命中的规则覆盖前面的同名字段：专用分组（ent、OpenTelemetry、pnpm）必须排在通用的 minor-patch 规则之后，`timestamp-optional` 那条用 `groupName: null` 把无时间戳依赖从各组里拿出来。改规则后用 `--dry-run=lookup` 看每个更新的 `branchName`。
- 本地验证时 `--platform=local` 用 `git ls-files` 取文件，新文件要先 `git add` 才会被读到。

## 风险与约束

1. **ruleset**：进 `main` 必须 PR 且 `ci-ok` 绿。`ci.yml` 的 `pull_request` 触发不得加 `paths` 过滤（现在是裸 `pull_request:`，增量靠 `ci-changes.sh` 的 job 级 `if`），否则 Renovate 的 PR 缺 `ci-ok` 永远合不进。
2. **Renovate 的 PR 是同仓库分支**，`pull_request` workflow 对它有仓库 secrets 访问权。现在 `ci.yml`/`bench.yml` 没有 `secrets.` 引用且 `permissions: contents: read`；以后给 `ci.yml` 加 secret 时要想到「依赖升级 PR 的安装脚本会在带 secret 的环境里跑」。
3. **PR 年龄闸只管 Renovate 自己开的 PR**：人手改的固定点不再有年龄检查，也没有 `gh release verify`。
4. **「是否落后上游」不再有脚本判断**（ADR 0016）：发版前看 Dependency Dashboard，没有待合并的固定点升级。
5. **go.sum 与 entc.sum**（结论来自 `.agents/docs/research/ent-renovate.md`）：`internal/db/ent` 入库后，根模块的 `go get -t ./...` 加 `go mod tidy` 可以全自动完成（pgx、miniredis 升级实测过），gomod 照常周更、不设 Dashboard 批准。`entc.mod` 不行：Renovate 的 `go get` 只改根 `go.mod`/`go.sum`，去掉 `tools/entgen` 的 build ignore 也无法让 `entc.sum` 自动更新，所以 ent 的升级 PR（`renovate/ent`）很可能带 artifact 失败（缺 sum），需要 agent 按下面审查清单里的命令补 sum。`sdk/go` 没有生成物依赖，`gomodTidy` 正常。
   「全自动」只指 `go.mod`/`go.sum`：改了进二进制的模块版本时，`license-check` 会因 `third_party/go/THIRD_PARTY_NOTICES.txt`（带模块版本）过期变红，合并前要补一次生成（见审查清单）。
6. **AGPL**：不要在仓库 CI 里运行 `renovate` 或 `renovate-config-validator`；配置校验靠 Mend App 对 `renovate/reconfigure` 分支的检查和本地一次性运行。
7. 改 `renovate.json` 后要等下一轮（最长 4 小时）或在 Dashboard 勾「trigger a request for Renovate to run again」。

## 负责人操作

1. 本切片合并**之后**再安装 Mend Renovate GitHub App（`github.com/apps/renovate`），避免 Onboarding PR 与已有配置撞车，也避免两套机制并行。
2. **只选 `eimsound/lazy-observe` 仓库**，不要给整个组织。
3. **授权时确认 App 拥有 Dependabot alerts: Read**（App 的「Configure」→「Permissions」里应能看到 `read` 访问）。没有这项权限时 `vulnerabilityAlerts` 不工作且不报错。Mend 更新权限时 GitHub 会给仓库管理员发「接受新权限」的提示，需要点接受。
4. 仓库设置复核：Dependency graph 开、Dependabot alerts 开、**Dependabot security updates 关**（避免与 Renovate 的 `[SECURITY]` PR 重复）、Grouped security updates 关。
5. Ruleset 保持「进 `main` 必须 PR + `ci-ok` 绿」，不给 Renovate 加 bypass；若开了「Require signed commits」，在首个 Renovate PR 上确认提交显示 Verified。
6. 安装后检查：Dependency Dashboard issue 出现；列出 Pending / Awaiting Schedule；Onboarding PR 不出现。
7. 紧急放行安全修复（发布不满 7 天时 Renovate 不会开 PR，pnpm 也会拒装）：npm 工程在对应 `pnpm-workspace.yaml` 加 `minimumReleaseAgeExclude`；Renovate 侧在 Dashboard 勾选该条目，或临时在 `renovate.json` 给该包加 `minimumReleaseAge: null` 的 `packageRules`；
   Python 把 `[tool.uv] exclude-newer` 临时改成具体时间戳；固定点直接人工改版本与 sha（版本、sha、`# <tag>` 一起改）。放行记录写在 PR 描述里。
8. 是否向 Mend 申请 OSS 白名单（Discussions → Mend Hosted Request）：本方案不依赖它。

## PR 审查清单

固定点 PR（Victoria / mise / Trivy）合并前：

- 同一 PR 里版本变量、所有 sha 行的值与行尾 `# tag` 一致（PR CI 的下载校验与 `trivy-pin` 会在 sha 错配时变红）；
- **首个固定点 PR 人工看 diff**：抽查任一 sha 与上游 release 的 `*_checksums.txt` / `SHASUMS256.txt` 一致；
- Renovate 不会做 `gh release verify`；Trivy 升级只有 7 天年龄和 sha 校验两道闸；
- Victoria PR：`third_party/victoria/SOURCES.txt` 的版本与 LICENSE 链接改成新 tag（`license-check` 用 `scripts/third-party-notices.sh` 校验，不改就红），并对照上游该 tag 的 LICENSE 是否变化；
- 无时间戳的依赖（Quay、seaweedfs、postgres、debian、`govulncheck`、distroless）不受 7 天限制，合并前看一眼上游 release 说明。

**Go 依赖 PR（`gomod minor-patch`、`opentelemetry`、`ent`、major）**：改了进二进制的模块版本时 `license-check` 会红，agent 跑 `mise run third-party-notices` 后把 `third_party/go/THIRD_PARTY_NOTICES.txt` 提交进该分支。

**`renovate/ent` PR（`entc.mod` 补 sum）**：PR 的 `entgo.io/ent` 在 `go.mod` 与 `entc.mod` 里必须是同一版本（错位时生成的 `.entc` 临时程序会报 `has no field`）；Renovate 补不出 `entc.sum`，agent 在仓库根目录执行（命令已实测，来自 `.agents/docs/research/ent-renovate.md`）：

```bash
rm entc.sum   # 必须先删，否则留陈旧条目
work=$(mktemp -d -p ~/.cache)
(cd internal/db && env -u GOAMD64 -u GOARM64 CGO_ENABLED=0 \
  go run -mod=mod -modfile=../../entc.mod ../../tools/entgen/main.go ./schema "$work" \
  github.com/eimsound/lazy-observe/internal/db/ent)
rm -rf "$work"
mise run ent-generate        # -mod=readonly 重新生成，同时确认 sum 已补全
mise run third-party-notices  # ent 版本进了 THIRD_PARTY_NOTICES.txt
git add entc.mod entc.sum internal/db/ent go.mod go.sum third_party/go/THIRD_PARTY_NOTICES.txt
mise run ent-generate-check
```

不要用 `go mod tidy -modfile=entc.mod`（会把 `entc.mod` 膨胀成根模块镜像）。补完 push 后 Renovate 会把该分支当作被人改过，不再自动 rebase（同一 ent 版本出新 release 时要人工处理分支）。

**首个 GitHub Actions PR 人工看 diff**：确认 SHA 与行尾 `# vX.Y.Z` 注释被同步改了（本地没能制造出一次真实的 action 升级来验证）。

## 没验证的

- Mend 托管 App 上的实际行为（分组分支名、Dashboard、`vulnerabilityAlerts` 的读取、PR 标题格式、托管环境里 Docker Hub 标签查询是否同样 403）：`platform=local` 不创建分支；
- 真实的版本 + sha 同时改写（`doAutoReplace`）：依据是源码与 extract / lookup 结果；
- gcr distroless 的 `timestamp-optional`：现在没有可升的版本，没能观察到；
- GitHub Actions 升级时 `# vX.Y.Z` 注释同步。
