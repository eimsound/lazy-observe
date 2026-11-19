---
name: do
description: lazy-observe 的单一工作入口。在开始任何非平凡改动前使用：把一句目标路由到一条流程（feature / feature-multi / bug-fix / investigate / refactor / release / docs / afk-run），给出该流程要调用的 skill、产物路径、必跑门禁和提交纪律。用户提到"走流程""选 skill""AFK""无人值守""该跑哪些检查"时也用它。
---

# do

本仓库唯一的自动入口。其它 skill 保持手动调用，由这里按情境选一条流程。
先读 `AGENTS.md`，再读本文件；两者冲突时以 `AGENTS.md` 为准。

## 入口契约

用户给的是一句话目标。开工前先确定两件事，缺一件就先问，不要猜：

1. **目标**：一句话说清改完以后用户能看到什么。
2. **完成条件**：一条能跑的检查命令，或一个可观察的事实。说不出来就说明还没准备好，退回 `/skill:grill-with-docs` 或 `/skill:spec-to-plan`。

拿到这两件后，把匹配到的流程步骤逐字抄进 todo list，再动手。跳过的步骤留在列表里，写一行 `skip: <理由>`。悄悄少做一步是不允许的。

### 开工前置检查

动第一个文件前跑两条命令，结果记进 todo list：

1. `gh auth status`。失败时，只读的 CI 查询改用公开 API，例如
   `curl -fsS "https://api.github.com/repos/eimsound/lazy-observe/actions/runs?branch=main&per_page=1"`；
   要开 PR、触发 workflow、合并时，停下来请负责人登录。不要做到一半才发现。
2. `git status --short`。有不属于本任务的未提交改动时，切片一律在 worktree 里基于 base 开工
   （`git worktree add <dir> -b slice/<topic>-<n> <base>`，代替「提交、推送与合入」第 1 步的 `git switch`），不碰这些改动。
   切片要改的文件和这些改动重叠时，先问负责人；等负责人提交后，再基于新的 base 开分支。

## 路由

| 情境 | 流程 | 主 skill | 产物 |
| --- | --- | --- | --- |
| 新行为、跨层改动、一个会话能做完 | feature | `/skill:spec-to-plan` → `/skill:implement` | `.agents/docs/plans/<topic>.md` |
| 多票、跨会话、可并行 | feature-multi | `/skill:to-spec` → `/skill:to-tickets` → `/skill:implement-spec` | GitHub issue + integration branch |
| 报错、回归、flaky 测试 | bug-fix | `/skill:diagnosing-bugs` | 回归测试 + 证据 |
| 只读问题（"这块怎么工作"） | investigate | 直接查代码，不改 | 回答 + 文件行号 |
| 行为不变的整形 | refactor | `/skill:tdd` 保护接缝 | 全绿 diff |
| 发布、tag、容器、门面 | release | `/skill:wizard` 处理凭据 | `CHANGELOG.md` + tag |
| 文档、README、调研归档 | docs | 直接改 | 按读者选目录：用户文档放 `docs/`，AI/规划放 `.agents/docs/`（见 `AGENTS.md`） |
| 长时间无人值守 | afk-run | `/skill:chief-of-staff` + `/skill:implement-spec` | `.agents/docs/afk/<goal>/` |
| 方向没定、要拍板 | 先 `/skill:grill-with-docs`，再回本文件 | ADR | `.agents/docs/adr/` |
| 巨大且看不清全貌 | `/skill:wayfinder`，清完雾再回本文件 | 决策票 | GitHub issue |

`/skill:to-spec` 和 `/skill:to-tickets` 产出的票已经带 `ready-for-agent`，不要再 triage。
外部来的 bug 报告或功能请求才走 `/skill:triage`。

## 流程步骤

### feature

1. 读 `.agents/docs/requirements.md` 对应小节、相关 ADR、`.agents/docs/GLOSSARY.md` 术语。
2. `/skill:spec-to-plan` 产出计划，落到 `.agents/docs/plans/<topic>.md`；计划里必须有仓库证据、两条备选路线、Tracer Bullet 切片、每切片的 red/green 命令。
3. `/skill:implement`：每个切片先写失败测试，再最小实现，跑门禁，在计划里勾选并贴证据（命令 + 结论）。
4. 收尾：`/skill:code-review`（Standards 对照 `AGENTS.md`，Spec 对照计划）→ 更新 `CHANGELOG.md` → `/skill:retro`。

### feature-multi

1. 同一个上下文窗口里做完 `/skill:to-spec` 和 `/skill:to-tickets`，不要中途 compact 或 clear。
2. 票落 GitHub Issues，blocking 边在票正文写 `Blocked by #<n>`（可用 sub-issue 挂到 spec 票下）；按 frontier 推进。
3. `/skill:implement-spec` 跑整个 spec：integration branch + 每个 implementer 自己的 worktree，收尾在 integration branch 上跑一次 `/skill:code-review`。
4. 结束后清理 worktree，再 `/skill:retro`。

### bug-fix

1. 先建一个**已经红**的反馈环：一条命令，跑出来就是这个 bug。
2. 定位到最小接缝，写回归测试，再修。
3. 修完跑同一命令转绿，附上修复前后输出。
4. `/skill:retro` 问一句：什么环境改动能防止这类 bug 再发生。

### release

1. 打 tag 前，在 Renovate Dependency Dashboard 勾选重跑，确认 Victoria、Trivy 等手工固定点没有待合并的升级；有就先合并或明确放过。
   版本是否落后只看这一步，`storage-bin-scan` 不查（ADR 0016）。Renovate 由后续切片接入，接入前先人工核对
   `scripts/download-storage-bins.sh` 与 `scripts/install-trivy.sh` 里的 pin。
2. 更新 `docs/version-matrix.md`（先矩阵、后发布），确认该提交的 CI 为绿，更新 `CHANGELOG.md`。
3. 打 tag 属于「必须停下来等人」的动作：由负责人确认后再打。`release.yml` 的 `storage-bin-scan` 命中只警告，
   发版后看 run 的 step summary 里的警告表即可；扫描失败才会挡住出包。

### afk-run

见下一节。afk-run 是 feature/feature-multi 的无人值守版本，步骤相同，只多了暂停条件和决策日志。

## 验证矩阵

按改动面选，不要无脑全跑。慢任务在后面标注。

| 改动面 | 必跑 |
| --- | --- |
| Go 代码（`internal/`、`cmd/`） | `mise run vet`、`mise run test`、`mise run fmt-check`、`mise run lint` |
| Ent schema 或基线 SQL | `mise run migration-check`、`mise run ent-generate-check`、`mise run test-meta-pg` |
| 网关路由、API 契约、e2e 脚本 | `mise run e2e-contract-check`、`mise run e2e`（慢） |
| 重启、持久接管、恢复 | `mise run e2e-restart`（慢） |
| HA、多副本、存储集群 | 命中下面「本地必跑 `mise run e2e-ha` 的改动面」时必跑（需 Docker，很慢）；其余可选（PR CI 的 HA E2E job 已按改动面跑，只含功能断言） |
| `web/` | `mise run web`（含 typecheck、build、playwright、JS 许可证；慢） |
| `sdk/`（客户端 SDK） | 按语言：`mise run sdk-go`、`mise run sdk-node`、`mise run sdk-browser`、`mise run sdk-python`；`sdk/go` 另跑 Go 代码那行；协议面改动同 PR 更新 `docs/version-matrix.md` |
| 依赖、许可证、漏洞 | `mise run license-check`、`mise run mod-tidy-check`、`mise run vulncheck` |
| 构建、嵌入、产物大小 | `mise run dist-cross`、`mise run gate-release-artifacts` |
| 发布配置、tag | `mise run release-prerequisites-test`、`mise run release-publish-check` |
| Helm | `mise run helm-lint`、`mise run helm-template-test` |
| CI workflow、改动面分组 | `mise run workflow-check`、`mise run ci-changes-test` |
| 安装脚本 | `mise run install-check` |
| 性能 | `mise run perf-ha`（很慢）、`mise run performance-gates`（最慢） |

规则：门禁失败就停，先修失败，不要继续下一个切片。`CGO_ENABLED=0` 是硬约束，任何需要 CGO 的方案直接否决。`internal/db/ent` 不跟踪、不手改、不用 golines 格式化。

脚本测试与结构断言只守「判错会悄悄放行、且后果重」的规则：安全、供应链、发版放行。实现细节不写断言，报告类和辅助脚本不写测试；改错了 CI 自己会红的东西，不需要再加一道断言。

`mise run test-meta-pg` 从外网 Maven Central 取 Postgres 归档（`LO_TEST_PG_CACHE`，默认 `$HOME/.cache/embedded-postgres`；命中缓存就不联网，CI 也用 actions/cache 缓存同一目录）。缓存未命中又碰上 Maven Central 对共享出口 IP 限流时会失败，测试会把归档 URL 的真实 HTTP 状态码一起打出来（库本身只报 `no version found matching 18.3.0`），看到 403/429/5xx 重跑即可，不要去改测试。

### 本地必跑 `mise run e2e-ha` 的改动面

`mise run e2e-ha` 启两个副本，共享同一个元数据库（PostgreSQL）、Valkey、S3（SeaweedFS）和平台密钥。下面这些改动面只要碰了，
推之前本地必须跑一遍（需 Docker，很慢）：

- 启动和 bootstrap 路径：`cmd/lazy-observe/**`（`openServerMetadata`、`bootstrapAdmin`）、`internal/meta/store.go` 的 `Bootstrap`/`BootstrapSelfMonitoring`、`internal/meta/bootstrap_admin.go`；
- 平台密钥：`internal/secret/**`、`internal/config` 对 `security.secret_key_file` 的解析与校验；
- 元数据库首次写入：`internal/meta/migrations/**`、新增的 schema、首写与并发首写（如 `INSERT ... ON CONFLICT`）；
- 跨副本状态：会话（`internal/account/session.go`）、`internal/state/**`、成员与租约（`internal/meta/alert_engine*.go`、续期与 fencing）；
- 恰好一次与去重：通知 outbox（`internal/meta/notification_outbox.go`、`internal/alert/outbox.go`）、告警评估所有权（`internal/alert/ownership/`、`internal/alert/rule_evaluator.go`）、Issue 聚合（`internal/meta/error_inbox.go`、`internal/errortrack/`）、replay 与 blob（`internal/blob/s3.go`、`internal/replay/`）。

K1（PR #60，commit `3ce6c19`）就是反例：它让元数据库记录平台密钥指纹、启动时不一致即拒绝启动，本地没跑 e2e-ha，
PR CI 的 HA E2E 红了——两个副本没有共享密钥，后启动的那个直接被拒绝启动。

### 多 worktree 并行

`mise run lint`、`mise run sdk-go` 里的 golangci-lint 带 `--allow-serial-runners`：撞上另一个 worktree 时排队等锁，不再报
`parallel golangci-lint is running`；跨 worktree 不需要编排者排队，`go build`/`go test` 的缓存本身支持并发。
默认缓存 `~/.cache/golangci-lint` 是所有 worktree 共用的，出现过「报出别的 worktree 的问题」的误报（没能复现出来）；
再碰到就把 `GOLANGCI_LINT_CACHE=$(mktemp -d)` 加在 `mise run lint` 前面重跑，代价是冷启动约 20 秒。
同一个 worktree 内 Go 门禁仍串行：`vet` → `test` → `lint` → `fmt-check` 一条一条跑，这几条都以 `ent-generate` 为依赖，
并行会抢着 `mv` 同一个 `internal/db/ent`（`scripts/generate-ent.sh` 第 38–41 行）。

CI 分层（CI 不替你跑的，改到对应改动面时本地必须跑）：

CI 跑在 GitHub 托管 runner 上（`ubuntu-24.04`、原生 arm64 `ubuntu-24.04-arm`），公开仓不挂自建 runner。
`ci.yml` 先由 `changes` 作业按改动路径分组，各业务 job 只在自己的改动面变化时运行，最后 `ci-ok` 汇总；
`ci-ok` 是 main ruleset 的 required check，被跳过的 job 视为通过；等 CI 见「提交、推送与合入」第 5 步。

| 改动路径 | 跑哪些 job |
| --- | --- |
| `*.md`、`docs/**`、`LICENSE`、`.agents/**`、`.claude/**` | 不跑业务 job，`ci-ok` 直接绿 |
| `sdk/node/**`、`sdk/browser/**`、`sdk/python/**` | 对应语言的 SDK job + sdk-journeys（范围见表后说明：node → node、browser，python → python，browser → browser） |
| `sdk/go/**` | Go SDK job + 服务端 Go 检查、构建、e2e、HA E2E + sdk-journeys（go、browser） |
| `web/**` | web + 构建 + sdk-journeys（只跑 browser） |
| `cmd/**`（`cmd/perf-gate/**` 除外）、`internal/**`、`go.mod`/`go.sum`、`scripts/download-storage-bins.sh`、`scripts/prepare-web-assets.sh`、`scripts/sdk-journeys.sh` | 另加 sdk-journeys 全跑（四个 journey） |
| `cmd/perf-gate/**` | 同 `cmd/**` 的 Go 检查、构建、e2e、HA E2E，但不跑 sdk-journeys（它不进平台二进制） |
| `internal/db/**`、`internal/meta/**` | 另加 PG 元数据测试 |
| `cmd/**`、`internal/**` | Go 检查、构建、e2e、e2e-restart、HA E2E |
| `go.mod`/`go.sum` | 同上 + meta-pg |
| `scripts/e2e-multi-replica.sh` 及其辅助程序、`bench/performance-gates.json`、`scripts/download-storage-bins.sh` | HA E2E（另加各自原有的 job） |
| 打包链路 | 零配置产物冒烟 + 原生 arm64 构建与冒烟 |
| `.github/dependabot.yml`、`.trivyignore.yaml` | 只跑 Go 检查（workflow-check 校验它们） |
| `scripts/download-storage-bins.sh`、`scripts/mise-release.env`、`scripts/check-image-vulns.sh`、`scripts/install-trivy.sh`、`scripts/e2e-multi-replica.sh`、`scripts/check-pinned-releases.sh`、`scripts/release-age.env` | 叠加 `pins`：`pinned-releases` job（手工固定点的最小发布年龄门禁，要联网查 GitHub API；叠加在上面原有的分组上） |
| `.github/**`、`mise.toml`、`scripts/ci-*`、`scripts/mise-release.env`、`Dockerfile*` | 全量 |

`sdk-journeys` job（`mise run sdk-journeys`，PR CI 里最长的一条，冷缓存约 9.5 分钟）：起一个真实平台，再依次跑
`sdk/{go,node,python,browser}/e2e.sh`。范围由 `ci-changes.sh` 的 `sdk_journeys_only` 输出决定，经 `LO_SDK_JOURNEYS_ONLY` 传入：
空 = 四个都跑；否则只跑列出的语言，混合改动取并集，其它任何会触发 sdk-journeys 的路径（服务端、Go 依赖、journey/storage/web 资产脚本、`sdk/` 根共享文件、CI、未分类）都回到全跑。
窄范围只有：`sdk/go/**` → go、browser，`sdk/node/**` → node、browser（browser journey 会运行这两个 SDK 的示例），
`sdk/python/**` → python，`sdk/browser/**` 与 `web/**` → browser。本地同样用 `LO_SDK_JOURNEYS_ONLY=go,browser mise run --skip-deps sdk-journeys`。

| 触发 | workflow | 跑什么 |
| --- | --- | --- |
| PR、push 到 `main`、`integration/**` | `ci.yml` | 上表按改动面挑出的 job，由 `ci-ok` 汇总 |
| `v*` tag、手动 release | `release.yml` | 双架构容器冒烟、镜像漏洞扫描（Trivy，有修复的 CRITICAL/HIGH 即失败）、`storage-bin-scan`（对下载并校验过 sha256 的嵌入 Victoria 上游二进制做 Trivy 扫描，参数与镜像扫描一致；ADR 0016：有命中只 `::warning::` 并写 step summary、不阻塞，扫描没跑成才失败）、性能门禁，全过才出包 |
| 手动 | `bench.yml` | 依次跑 `bench`（30 秒摄入基准 + 基线门禁）、`perf-release`（双架构产物门禁 + 零配置 release smoke）、`perf-ha`（`e2e-ha` 全部 HA 断言 + 查询 P95 与弱扩展门禁）、`perf-results-check` |

切片分支没有 push 触发的 run，PR run 是唯一一遍。性能门禁在共享 runner 上噪声大，只在手动和发版前跑，不挡普通 PR：改了性能敏感路径就本地跑「性能」那行。`mise run ci-changes-test` 只守「CI 自身、工具链、未分类路径、取不到 base 一律全量」这几条兜底；具体分组改了不用同步改测试。

只补验证、不改代码时（确认基线、补跑门禁、发版前复核），上面「本地必须跑」不适用：重门禁优先走 CI 手动入口，只有 CI 没有对应入口时才在本地跑。

| 要补的门禁 | 手动入口 |
| --- | --- |
| `bench`、`perf-release`（含 `check-release-smoke.sh`）、`perf-ha`（含 `e2e-ha`）、`perf-results-check`；`performance-gates` = `perf-release` + `perf-ha` | `bench.yml` |
| 双架构容器冒烟、性能门禁、snapshot 归档 | `release.yml`（手动只出 snapshot，不发布） |
| `ci.yml` 业务 job（e2e、e2e-restart、meta-pg、web、SDK 等） | `ci.yml`（手动触发即全量） |

触发：`gh workflow run bench.yml --ref <branch>`，再用 `gh run list --workflow bench.yml --event workflow_dispatch --limit 1` 取 run id，`gh run watch <id>`。`gh` 未登录时，请负责人在 GitHub 网页 Actions → 对应 workflow → Run workflow 手动触发。

## AFK 契约

无人值守只在同时满足三条时开始：

1. **可判定的退出谓词**：一条命令，例如 `mise run test` 或 `mise run e2e`。它现在要么红（bug-fix），要么绿（feature 的基线），不能是"看起来差不多"。
2. **有界范围**：一个 plan 文件或一张票，写清 in scope / out of scope。
3. **可审计的记忆**：长任务用 `/skill:chief-of-staff`，记忆放 `.agents/docs/afk/<goal>/`（`index.md` + 每页一件事）。每条结论带日期；涉及代码的结论带 commit。短任务把执行日志追加到 `.agents/docs/plans/<topic>.md` 的 `## 执行日志`，每条一行：`<时间> <命令> → <结论>`。不进日志的动作视为没发生。

必须停下来等人的情况：

- 打 tag、发 release、推容器镜像；
- 直接往 `main` 或 integration branch 推；对共享分支强推；
- 删数据、改 `.agents/docs/requirements.md`、动根 `LICENSE`；
- 调外部付费服务、发通知给人；
- 同一门禁连续失败两次以上，且原因不明；
- 需要新增依赖（先过许可证 allowlist：Go 侧 Apache-2.0/MIT/BSD，JS 侧另加 ISC/0BSD/Unlicense；MPL-2.0 仅限开发/构建期工具（如 lightningcss）；以 `AGENTS.md` 为准；禁 AGPL/GPL）；
- 改宿主机全局环境：docker 镜像改 tag 或换拉取来源、全局安装工具、改 DNS 或代理、改 shell 配置。拉镜像或装工具失败时停下报告，不要自行绕路。

可以自己往前走的：本地改代码、本地跑门禁、写测试、更新 `CHANGELOG.md`、本地 commit、按下一节推自己的切片分支、开 PR、等 CI、CI 绿后合并该 PR。

`/skill:implement-spec` 会开 worktree 和 integration branch。每个 implementer 必须基于 integration branch 开工，收尾前把 integration branch 的 tip 合回自己的分支。并行实现线程之间不共享工作树。

编排者只分派和验收，不自己跑切片的门禁。subagent 汇报的「通过」「已修复」，先抽查它贴的命令输出或 CI 日志再采信；拿不出证据就当没跑。subagent 遇到上面「必须停下来等人」的情况，先回报编排者，由编排者问负责人。

## 提交、推送与合入

一个切片 = 一个 commit = 一条分支 = 一个 PR。不要在一条分支上堆多个切片，也不要让一个 PR 混两个不相关的改动。

查 CI 一律用 `gh`（`gh pr checks`、`gh run list`、`gh run watch`、`gh run view`）；仓库是 `eimsound/lazy-observe`，在 clone 内运行时 `gh` 自动推断。

0. 记基线：`gh run list --workflow ci.yml --branch <base> --event push --status completed --limit 1 --json databaseId,conclusion,headSha` 取最近一次**已完成**的 push run。base 上可能有正在跑的 run，不加 `--status completed` 会拿到 `in_progress` 那条，`conclusion` 为空、基线作废。`conclusion` 为 `failure` 时用 `gh run view <id> --json jobs --jq '.jobs[] | select(.conclusion=="failure") | .name'` 抄下失败 job 名。base 本来就红时，这份名单是后面判定「是否新增失败」的基线。
1. 切分支：`git switch -c slice/<topic>-<n> <base>`。`base` 默认 `main`；feature-multi 里是 integration branch。
2. 一个切片只留一个 commit，信息用 `fix:` / `feat:` / `chore:` + 中文。CI 修复用 `git commit --amend` 折叠回这个 commit，不新增提交（推送方式见第 6 步）。
3. 本地门禁先全绿（按验证矩阵那张表），再推：`git push -u origin slice/<topic>-<n>`。只推自己的切片分支，不强推（修复流程见第 6 步）。
4. 开 PR：`gh pr create --head slice/<topic>-<n> --base <base> --title "<标题>" --body-file -`（body 用 heredoc）。body 按 `/skill:pr` 的形状写：一张能说明改动的最小视觉材料、改前改后的证据、单向门还是双向门的判断。
5. 等 CI：`ci-ok` 是 main ruleset 的 required check，按它等：`gh pr checks <number> --watch --interval 30 --required`。
   刚开 PR 时 `ci-ok` 还没登记，上面这条会以 `no required checks reported` 退出；这时改按 `--commit` 找这次 push 的 run 来等：

   ```bash
   gh run list --workflow ci.yml --event pull_request --commit "$(git rev-parse HEAD)" --limit 1 \
     --json databaseId --jq '.[0].databaseId // empty'   # 空 = run 还没登记，隔 15 秒再取，最多等 5 分钟
   gh run watch <run-id> --interval 30 --exit-status      # 非零 = run 结论不是 success
   ```

   逐 job 结果用 `gh pr checks <number> --json name,state,bucket`。全部通过就是绿；`ci-ok` 失败时用 `gh run view <run-id> --json jobs` 列出失败 job，与第 0 步的基线比对：全部是既有失败就算通过，出现基线外的新失败才转第 6 步。上限 30 分钟；超时就停下报告，不要盲等。
6. CI 红：`gh run view <run-id> --log-failed` 看失败步骤（run id 来自第 5 步）。修完 `git commit --amend`，换新分支名、开新 PR 取代旧 PR
   （本环境拦截强推，`--force-with-lease` 也推不上去）：
   1. `git branch -m slice/<topic>-<n+1>`，`git push -u origin slice/<topic>-<n+1>`；
   2. 按第 4 步开新 PR，body 第一行写 `取代 #<旧号>`；
   3. `gh pr close <旧号> --comment "由 #<新号> 取代：<一句原因>"`。

   然后回到第 5 步。同一个切片连续两次红且原因不明，停下。
7. 绿了才合并：`gh pr merge <number> --rebase --delete-branch`（一个切片一个 commit，不改写历史）；仓库不允许 rebase merge 时改用 `--squash`。合并后切回 `base`，`git pull --ff-only`，并 `git branch -D slice/<topic>-<n>` 删掉本地分支。
   合并被拒、报错含 ``without `workflow` scope``：PR 的 base 之后 `main` 上有别的提交改了 `.github/workflows/*`，
   本环境的 PAT 又没有 workflow scope。处理：`git fetch origin && git rebase origin/main`，重跑本地门禁，再按第 6 步的 1–3 换新分支、开新 PR、关旧 PR，
   回到第 5 步。PAT 的 scope 由负责人决定，不换别的 API 或令牌绕过。
8. 合并后在 `base` 上确认一次 CI：`gh run list --workflow ci.yml --branch <base> --event push --limit 1 --json databaseId,headSha,status,conclusion` 的 `headSha` 应等于合并后的 tip；用 `gh run watch <id>` 等它结束，再用 `gh run view <id> --json jobs` 的失败 job 比对第 0 步基线。出现基线外的新失败 job 就停下、不要继续下一个切片，并在报告里写明是哪个 job。

CI 是第二道门，不能替代本地门禁。本地没跑绿就推上去等 CI 算违规。

feature-multi：每个 implementer 的 PR base 是 integration branch，各自走第 0–7 步；integration branch 按第 8 步确认不新增失败后，再开一条 `integration/<spec>` → `main` 的 PR，同样走第 0–8 步。

## 收尾

- `/skill:code-review`：Standards 对照 `AGENTS.md`，Spec 对照计划或票。放在最后一条切片 PR 合并之前跑；发现问题就开一条修复分支走同一条流水线。
- 更新 `CHANGELOG.md` 作为独立的 `chore/changelog-<topic>` 分支，走同一条流水线。
- `/skill:retro`：回看这次会话，给出环境改进（导航指针、自动检查、编码标准、工具）。机械性错误变成确定性检查，判断题变成编码标准。
- 汇报时列出真实跑过的命令和结果，以及每个 PR 的链接和它对应 CI run 的结论。没跑的不要说通过。
- 发现流程本身有缺口（比如某类改动没有对应门禁），在这里补一行，不要绕过去。
