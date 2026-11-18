# lazy-observe — worker 约定

- Go 1.26，模块 `github.com/eimsound/lazy-observe`。`CGO_ENABLED=0` 是硬性约束（交叉编译 linux × amd64/arm64）。
- 需求基线：docs/requirements.md (v1.0)。不要编辑它。
- PoC/spike 代码位于 `poc/<topic>/` 下，并带 FINDINGS.md；生产代码在 `cmd/` 和 `internal/` 下。
- 客户端 SDK 在 `sdk/{go,node,browser,python}` 下（约定见 `sdk/AGENTS.md`）；Go SDK 是嵌套模块，
  根 `go.mod` 以 `replace => ./sdk/go` 引用。协议面（端点、字段、DSN、鉴权）改动与 `docs/version-matrix.md`
  在同一个 PR 里修改。
- 禁止引入 AGPL/GPL 的生产或运行时依赖。生产依赖的许可证白名单以检查脚本为准：
  Go 侧（`scripts/check-go-licenses.sh`，服务端与 `sdk/go`）只允许 Apache-2.0、MIT、BSD-2-Clause、BSD-3-Clause；
  JS 侧（`scripts/check-js-licenses.sh`，`web`、`sdk/node`、`sdk/browser`）在此之外还允许 ISC、0BSD、Unlicense
  （宽松许可证，可与 MIT 一起分发）。白名单外的许可证先改本条和对应脚本，再引入依赖。
  白名单之外只接受以下两项开发期例外：
  - Air GPL-3.0：由 mise 管理的本地开发 CLI，不得链接进 Go 代码或打包进任何产品产物。
  - MPL-2.0（目前只有 `lightningcss`，Vite 8 与 `@tailwindcss/vite` 的 CSS 处理器；web 已经通过
    `@tailwindcss/vite` 使用它）：仅限开发和构建期工具，只能出现在 devDependencies 或构建工具的依赖树里，
    不得进入任何生产或运行时依赖树，不得链接进 Go 二进制或打包进产品。它产出的 CSS 等构建结果不属于其衍生作品。
    `scripts/check-js-licenses.sh` 只扫生产依赖，并继续拒绝生产依赖里的 MPL-2.0。
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

Every skill under `.agents/skills/` except `do` and `spec-to-plan` is an unmodified copy from
`mattpocock/skills` (MIT, see `.agents/skills/LICENSE-mattpocock-skills`), pinned in `skills-lock.json`. Do not edit them in place: `npx skills@latest update`
overwrites them. Put repo-specific rules in `do` or this file. This repo is already configured
(`docs/agents/`), so ignore any prompt to run `/setup-matt-pocock-skills`.

### Issue tracker

Issues and specs live as GitHub Issues on `eimsound/lazy-observe`, managed with the `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Five canonical triage roles map to default label strings. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` at the repo root with `docs/adr/`. See `docs/agents/domain.md`.
