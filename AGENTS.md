# lazy-observe — worker 约定

- Go 1.26，模块 `github.com/eimsound/lazy-observe`。`CGO_ENABLED=0` 是硬性约束（交叉编译 linux × amd64/arm64）。
- 需求基线：`.agents/docs/requirements.md` (v1.0)。不要编辑它。
- 文档按读者分目录，新文档也按读者选目录：
  - `docs/` 只放面向用户的文档，读者是部署者、运维和 SDK 接入方，例如部署、SDK 接入、版本与协议兼容矩阵。
  - `.agents/docs/` 放 AI 工作流、规划和开发决策，包括需求基线、架构、ADR、术语表、plan、spec、调研、
    skill 配置和 AFK 记忆。索引见 `.agents/docs/index.md`。
- PoC/spike 代码位于 `poc/<topic>/` 下，并带 FINDINGS.md；生产代码在 `cmd/` 和 `internal/` 下。
- 客户端 SDK 在 `sdk/{go,node,browser,python}` 下（约定见 `sdk/AGENTS.md`）；Go SDK 是嵌套模块，
  根 `go.mod` 以 `replace => ./sdk/go` 引用。协议面（端点、字段、DSN、鉴权）改动与 `docs/version-matrix.md`
  在同一个 PR 里修改。
- 禁止引入 AGPL/GPL 的生产或运行时依赖。仅允许 Apache-2.0/MIT/BSD。
  Air GPL-3.0 是唯一接受的例外：它是由 mise 管理的本地开发 CLI，
  不得链接进 Go 代码或打包进任何产品产物。
- 如实汇报：列出你运行的每条命令及其真实结果。不要声称某项检查通过却没有实际运行。
- CI 入口是 mise tasks；测试命令变更时，同步更新 `mise.toml`。
- 非生成的 Go 文件使用 `golines -m 120 --chain-split-dots` 格式化；
  就地格式化时用 `-w`。不要格式化 `internal/db/ent`。
- 涉及代码或查询语言的输入框、文本展示，优先使用 CodeMirror。
- Ent schema 记录在 `internal/db/schema`；`internal/db/ent` 由 mise 生成，必须保持不被跟踪。
- fencing/租约被拒（如 `meta.ErrStaleRuleFence`）不得静默丢弃：放弃结果的调用方至少打一条 `slog.Warn`，
  带上规则、成员和纪元；`errors.Is` 命中后直接 `return` 或 `continue` 而不留日志算缺陷。
- `alert_engine_members`、`alert_rule_leases` 上读数据库时间的查询只走 `alertClockSQL`/`alertLeaseExpiry`，
  原因见 ADR 0011（SQLite `CURRENT_TIMESTAMP`/`datetime()` 只有秒精度）。

## Agent skills

### Workflow entry

Start non-trivial work through `.agents/skills/do/SKILL.md`: it routes a one-line goal to one
flow, names the skills that flow uses, and fixes the gates and commit rules. Do not pick a workflow
skill from its description alone when `do` covers the situation.

### Upstream skills

Every skill under `.agents/skills/` except `do` and `spec-to-plan` is a copy from
`mattpocock/skills` (MIT, see `.agents/skills/LICENSE-mattpocock-skills`), pinned in `skills-lock.json`. Do not edit them in place: `npx skills@latest update`
overwrites them. Put repo-specific rules in `do` or this file. The one local patch: the glossary and ADR paths
(`GLOSSARY.md`, `docs/adr/`) are rewritten to `.agents/docs/GLOSSARY.md` and `.agents/docs/adr/`. After an update,
re-apply it and confirm with `rg -nP '(?<![/\w.])(GLOSSARY\.md|docs/adr/)' .agents/skills` (only generic
per-context examples and headings may remain). This repo is already configured
(`.agents/docs/agents/`), so ignore any prompt to run `/setup-matt-pocock-skills`.

### Issue tracker

Issues and specs live as GitHub Issues on `eimsound/lazy-observe`, managed with the `gh` CLI. See `.agents/docs/agents/issue-tracker.md`.

### Triage labels

Five canonical triage roles map to default label strings. See `.agents/docs/agents/triage-labels.md`.

### Domain docs

Single-context: one glossary at `.agents/docs/GLOSSARY.md` with ADRs in `.agents/docs/adr/`. See `.agents/docs/agents/domain.md`.
