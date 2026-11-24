# lazy-observe — worker 约定

- `CGO_ENABLED=0` 是硬约束（交叉编译 linux × amd64/arm64）。
- 文档按读者选目录：`docs/` 给部署者、运维、SDK 接入方；`.agents/docs/` 给 AI 工作流、规划和开发决策（ADR、plan、spec、调研等）。
- PoC/spike 放本地 `poc/<topic>/`（git 忽略，不提交）；要保留的结论写进 `.agents/docs/research/<topic>.md`。
- 协议面（端点、字段、DSN、鉴权）改动与 `docs/version-matrix.md` 在同一个 PR 里改。
- 改 `sdk/` 下的代码或文档前先读 `sdk/AGENTS.md`。
- 改 `web/` 下的页面、UI 组件或样式前先读根目录 `DESIGN.md`（Google DESIGN.md 格式，改后跑 `npx @google/design.md@0.4.0 lint DESIGN.md`）；审查通用交互与可访问性用 `web-design-guidelines` 技能。
- 禁止引入 AGPL/GPL 的生产或运行时依赖。白名单以 `scripts/check-go-licenses.sh`（Apache-2.0、MIT、BSD-2/3-Clause）和
  `scripts/check-js-licenses.sh`（另加 ISC、0BSD、Unlicense）为准；白名单外的许可证先改本条和脚本，再引入依赖。仅有两项开发期例外：
  - Air（GPL-3.0）：本地开发 CLI，不得链接进 Go 代码或打包进产物。
  - MPL-2.0（目前有 `lightningcss` 与 `pathspec`）：只能在 devDependencies 或构建工具依赖树里（其中 `pathspec` 仅限文档静态站构建工具依赖树），不得进生产或运行时依赖树，不得链接进 Go 二进制或产品。
- 如实汇报：列出运行的每条命令及真实结果；没跑的检查不说通过。
- CI 入口是 mise tasks；测试命令变更时同步改 `mise.toml`。
- 非生成的 Go 文件用 `golines -m 120 --chain-split-dots` 格式化（就地加 `-w`），不格式化 `internal/db/ent`。
- 代码或查询语言的输入框、文本展示优先用 CodeMirror。
- `internal/db/ent` 是生成代码并纳入 git：改 `internal/db/schema` 后运行 `mise run ent-generate`，与 schema 一并提交，不手改。
- fencing/租约被拒（如 `meta.ErrStaleRuleFence`）不得静默丢弃：放弃结果的调用方至少 `slog.Warn`，带规则、成员和纪元；
  `errors.Is` 命中后直接 `return`/`continue` 不留日志算缺陷。
- `alert_engine_members`、`alert_rule_leases`、`source_map_upload_pins` 读数据库时间只走 `alertClockSQL`/`alertLeaseExpiry`（ADR 0011）。

## 工作流

- 改代码、改配置、发版、写文档或排查代码问题前，先读 `.agents/skills/do/SKILL.md` 按情境选流程（单行改错字/纯闲聊除外），不要只凭 skill description 选。
- 路线图与待办看板：https://github.com/orgs/eimsound/projects/4
- issue tracker、triage labels、domain docs（`.agents/docs/GLOSSARY.md`、`.agents/docs/adr/`）的配置在 `.agents/docs/agents/`。
- 审查代码，或修改后端、Web、SDK、部署/CI 的逻辑与配置时，按改动面查阅 `.agents/skills/pitfalls/SKILL.md`（仅改错字/注释/纯文档可跳过）。
