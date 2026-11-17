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
| 新行为、跨层改动、一个会话能做完 | feature | `/skill:spec-to-plan` → `/skill:implement` | `docs/plans/<topic>.md` |
| 多票、跨会话、可并行 | feature-multi | `/skill:to-spec` → `/skill:to-tickets` → `/skill:implement-spec` | GitHub issue + integration branch |
| 报错、回归、flaky 测试 | bug-fix | `/skill:diagnosing-bugs` | 回归测试 + 证据 |
| 只读问题（"这块怎么工作"） | investigate | 直接查代码，不改 | 回答 + 文件行号 |
| 行为不变的整形 | refactor | `/skill:tdd` 保护接缝 | 全绿 diff |
| 发布、tag、容器、门面 | release | `/skill:wizard` 处理凭据 | `CHANGELOG.md` + tag |
| 文档、README、调研归档 | docs | 直接改 | 对应 md |
| 长时间无人值守 | afk-run | `/skill:chief-of-staff` + `/skill:implement-spec` | `docs/afk/<goal>/` |
| 方向没定、要拍板 | 先 `/skill:grill-with-docs`，再回本文件 | ADR | `docs/adr/` |
| 巨大且看不清全貌 | `/skill:wayfinder`，清完雾再回本文件 | 决策票 | GitHub issue |

`/skill:to-spec` 和 `/skill:to-tickets` 产出的票已经带 `ready-for-agent`，不要再 triage。
外部来的 bug 报告或功能请求才走 `/skill:triage`。

## 流程步骤

### feature

1. 读 `docs/requirements.md` 对应小节、相关 ADR、`GLOSSARY.md` 术语。不要改 `requirements.md`。
2. `/skill:spec-to-plan` 产出计划，落到 `docs/plans/<topic>.md`；计划里必须有仓库证据、两条备选路线、Tracer Bullet 切片、每切片的 red/green 命令。
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
| HA、多副本、存储集群 | `mise run e2e-ha`（很慢） |
| `web/` | `mise run web`（含 typecheck、build、playwright、JS 许可证；慢） |
| `sdk/`（客户端 SDK） | 按语言：`mise run sdk-go`、`mise run sdk-node`、`mise run sdk-browser`、`mise run sdk-python`；`sdk/go` 另跑 Go 代码那行；协议面改动同 PR 更新 `docs/version-matrix.md` |
| 依赖、许可证 | `mise run license-check`、`mise run mod-tidy-check` |
| 构建、嵌入、产物大小 | `mise run dist-cross`、`mise run gate-release-artifacts` |
| 发布配置、tag | `mise run release-prerequisites-test`、`mise run release-publish-check` |
| Helm | `mise run helm-lint`、`mise run helm-template-test` |
| CI workflow、改动面分组 | `mise run workflow-check`、`mise run ci-changes-test` |
| 安装脚本 | `mise run install-check` |
| 性能 | `mise run perf-ha`（很慢）、`mise run performance-gates`（最慢） |

规则：门禁失败就停，先修失败，不要继续下一个切片。`CGO_ENABLED=0` 是硬约束，任何需要 CGO 的方案直接否决。`internal/db/ent` 不跟踪、不手改、不用 golines 格式化。

CI 分层（CI 不替你跑的，改到对应改动面时本地必须跑）：

CI 跑在 GitHub 托管 runner 上（`ubuntu-24.04`、原生 arm64 `ubuntu-24.04-arm`），公开仓不挂自建 runner。
`ci.yml` 先由 `changes` 作业按改动路径分组，各业务 job 只在自己的改动面变化时运行，最后 `ci-ok` 汇总；
`ci-ok` 是唯一的 required check，被跳过的 job 视为通过。

| 改动路径 | 跑哪些 job |
| --- | --- |
| `*.md`、`docs/**`、`LICENSE`、`.agents/**` | 不跑业务 job，`ci-ok` 直接绿 |
| `sdk/node/**`、`sdk/browser/**`、`sdk/python/**` | 对应语言的 SDK job |
| `sdk/go/**` | Go SDK job + 服务端 Go 检查、构建、e2e |
| `web/**` | web |
| `internal/db/**`、`internal/meta/**` | 另加 PG 元数据测试 |
| `cmd/**`、`internal/**` | Go 检查、构建、e2e、e2e-restart |
| `go.mod`/`go.sum` | 同上 + meta-pg |
| 打包链路 | 零配置产物冒烟 + 原生 arm64 构建与冒烟 |
| `.github/**`、`mise.toml`、`scripts/ci-*`、`Dockerfile*` | 全量 |

| 触发 | workflow | 跑什么 |
| --- | --- | --- |
| PR、push 到 `main`、`integration/**` | `ci.yml` | 上表按改动面挑出的 job，由 `ci-ok` 汇总 |
| `v*` tag、手动 release | `release.yml` | 双架构容器冒烟、性能门禁，全过才出包 |
| 手动 | `bench.yml` | 依次跑 `bench`（30 秒摄入基准 + 基线门禁）、`perf-release`（双架构产物门禁 + 零配置 release smoke）、`perf-ha`（`e2e-ha` 全部 HA 断言 + 查询 P95 与弱扩展门禁）、`perf-results-check` |

切片分支没有 push 触发的 run，PR run 是唯一一遍。性能门禁在共享 runner 上噪声大，只在手动和发版前跑，不挡普通 PR：改了性能敏感路径就本地跑「性能」那行。分组规则由 `mise run ci-changes-test` 钉住，改分组时同步改测试。

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
3. **可审计的记忆**：长任务用 `/skill:chief-of-staff`，记忆放 `docs/afk/<goal>/`（`index.md` + 每页一件事）。每条结论带日期；涉及代码的结论带 commit。短任务把执行日志追加到 `docs/plans/<topic>.md` 的 `## 执行日志`，每条一行：`<时间> <命令> → <结论>`。不进日志的动作视为没发生。

必须停下来等人的情况：

- 打 tag、发 release、推容器镜像；
- 直接往 `main` 或 integration branch 推；对共享分支强推；
- 删数据、改 `docs/requirements.md`、动根 `LICENSE`；
- 调外部付费服务、发通知给人；
- 同一门禁连续失败两次以上，且原因不明；
- 需要新增依赖（先过许可证 allowlist：只允许 Apache-2.0/MIT/BSD，禁 AGPL/GPL）；
- 改宿主机全局环境：docker 镜像改 tag 或换拉取来源、全局安装工具、改 DNS 或代理、改 shell 配置。拉镜像或装工具失败时停下报告，不要自行绕路。

可以自己往前走的：本地改代码、本地跑门禁、写测试、更新 `CHANGELOG.md`、本地 commit、按下一节推自己的切片分支、开 PR、等 CI、CI 绿后合并该 PR。

`/skill:implement-spec` 会开 worktree 和 integration branch。每个 implementer 必须基于 integration branch 开工，收尾前把 integration branch 的 tip 合回自己的分支。并行实现线程之间不共享工作树。

编排者只分派和验收，不自己跑切片的门禁。subagent 汇报的「通过」「已修复」，先抽查它贴的命令输出或 CI 日志再采信；拿不出证据就当没跑。subagent 遇到上面「必须停下来等人」的情况，先回报编排者，由编排者问负责人。

## 提交、推送与合入

一个切片 = 一个 commit = 一条分支 = 一个 PR。不要在一条分支上堆多个切片，也不要让一个 PR 混两个不相关的改动。

查 CI 一律用 `gh`（`gh pr checks`、`gh run list`、`gh run view`）；仓库是 `eimsound/lazy-observe`，在 clone 内运行时 `gh` 自动推断。

0. 记基线：`gh run list --workflow ci.yml --branch <base> --event push --limit 1 --json databaseId,conclusion,headSha` 取最新 run；`conclusion` 为 `failure` 时用 `gh run view <id> --json jobs --jq '.jobs[] | select(.conclusion=="failure") | .name'` 抄下失败 job 名。base 本来就红时，这份名单是后面判定「是否新增失败」的基线。
1. 切分支：`git switch -c slice/<topic>-<n> <base>`。`base` 默认 `main`；feature-multi 里是 integration branch。
2. 一个切片只留一个 commit，信息用 `fix:` / `feat:` / `chore:` + 中文。CI 修复用 `git commit --amend` 折叠回这个 commit，不新增提交。
3. 本地门禁先全绿（按验证矩阵那张表），再推：`git push -u origin slice/<topic>-<n>`。只推自己的切片分支；`--force-with-lease` 只允许用在自己的切片分支上，禁止 `--force`。
4. 开 PR：`gh pr create --head slice/<topic>-<n> --base <base> --title "<标题>" --body-file -`（body 用 heredoc）。body 按 `/skill:pr` 的形状写：一张能说明改动的最小视觉材料、改前改后的证据、单向门还是双向门的判断。
5. 等 CI：切片分支没有 push run，只看 PR run。用 `gh pr checks <number> --watch --interval 30 --required` 等 required check（`ci-ok`）；需要逐 job 结果时用 `gh pr checks <number> --json name,state,bucket`。全部通过就是绿；`ci-ok` 失败时用 `gh run view <run-id> --json jobs` 列出失败 job，与第 0 步的基线比对：全部是既有失败就算通过，出现基线外的新失败才转第 6 步。上限 30 分钟；超时就停下报告，不要盲等。
6. CI 红：`gh run view <run-id> --log-failed` 看失败步骤（run id 来自 `gh pr checks` 的链接或 `gh run list --branch slice/<topic>-<n>`），修完 `git commit --amend` 后 `git push --force-with-lease`，回到第 5 步。同一个 PR 连续两次红且原因不明，停下。
7. 绿了才合并：`gh pr merge <number> --rebase --delete-branch`（一个切片一个 commit，不改写历史）；仓库不允许 rebase merge 时改用 `--squash`。合并后切回 `base`，`git pull --ff-only`，并 `git branch -D slice/<topic>-<n>` 删掉本地分支。
8. 合并后在 `base` 上确认一次 CI：`gh run list --workflow ci.yml --branch <base> --event push --limit 1 --json databaseId,headSha,status,conclusion` 的 `headSha` 应等于合并后的 tip；用 `gh run watch <id>` 等它结束，再用 `gh run view <id> --json jobs` 的失败 job 比对第 0 步基线。出现基线外的新失败 job 就停下、不要继续下一个切片，并在报告里写明是哪个 job。

CI 是第二道门，不能替代本地门禁。本地没跑绿就推上去等 CI 算违规。

feature-multi：每个 implementer 的 PR base 是 integration branch，各自走第 0–7 步；integration branch 按第 8 步确认不新增失败后，再开一条 `integration/<spec>` → `main` 的 PR，同样走第 0–8 步。

## 收尾

- `/skill:code-review`：Standards 对照 `AGENTS.md`，Spec 对照计划或票。放在最后一条切片 PR 合并之前跑；发现问题就开一条修复分支走同一条流水线。
- 更新 `CHANGELOG.md` 作为独立的 `chore/changelog-<topic>` 分支，走同一条流水线。
- `/skill:retro`：回看这次会话，给出环境改进（导航指针、自动检查、编码标准、工具）。机械性错误变成确定性检查，判断题变成编码标准。
- 汇报时列出真实跑过的命令和结果，以及每个 PR 的链接和它对应 CI run 的结论。没跑的不要说通过。
- 发现流程本身有缺口（比如某类改动没有对应门禁），在这里补一行，不要绕过去。
