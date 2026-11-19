# sdk/ — worker 约定

本目录是 lazy-observe 官方客户端 SDK，四语言包（Go / Node / Browser / Python）位于
`sdk/{go,node,browser,python}`。仓库根 `AGENTS.md` 的全部约定同样适用；本文件只补充
SDK 特有的规则。服务端 ↔ SDK 的版本兼容关系以 `docs/version-matrix.md` 为事实来源。

## 版本兼容与降级守则

SDK 面向 lazy-observe 服务端上报遥测时必须遵守以下守则（与 `docs/version-matrix.md`
第 5 节保持一致，修改任一处须在同一个 PR 里同步另一处）：

1. **静默降级，严禁抛错**：当私有 ingest API（`/api/events/errors`、
   `/api/events/vitals`、`/api/events/diagnostics`、`/api/replay/segments`、
   `/api/profiles`、`/api/releases/:release/sourcemaps`）返回 404/4xx、或请求/
   响应中出现未知字段 / 未知枚举值时，SDK 一律静默降级：丢弃该次负载，可重试
   场景（如回放分段、profile 分片）缓存后退避重试，并叠加 diagnostics 上报。
   **绝不向应用抛错、绝不中断业务线程、绝不改变上层业务行为。**
   服务端一侧的现状见 `docs/version-matrix.md` §5：`/api/events/vitals` 的指标名与
   `/api/events/diagnostics` 的 `signal` / `reason` 都已实现跳过未知值并在响应的
   `rejected` 里计数（同批的已知值照常写入，所以那里的 `rejected > 0` 不是降级原因）；
   其它私有 ingest 端点遇到未知枚举值仍整批 `422`，仍按本条静默降级并上报
   diagnostics。被整批拒绝的若是 diagnostics 批次本身，它同样在重试预算耗尽后被丢弃，
   丢弃时按批次条数累加本地计数并记为 `diagnostic` / `retry_exhausted`（不递归上报），
   所以同批里服务端认识的诊断也会一起丢——新 SDK 枚举值配旧服务端时就是这个后果。
2. **向前兼容**：响应必须忽略一切未知字段，只解析自身认识的结构；新增服务端
   特性不得导致旧 SDK 报错。
3. **协议/端点变更同 PR**：改动任何 protocol 面（端点路径、字段、DSN、鉴权方式）
   时，`docs/version-matrix.md`（协议面清单 + 兼容矩阵）、服务端与 SDK 在同一个 PR
   里修改。向后不兼容变更视为服务端协议版本升级（当前基线 v0）。
4. **发布**：SDK 用带前缀的 tag 发布——`sdk/go/vX.Y.Z`、`sdk-node-vX.Y.Z`、
   `sdk-browser-vX.Y.Z`、`sdk-python-vX.Y.Z`。打 tag 的提交上服务端构建、e2e 与该
   SDK 的测试必须为绿。

## 目录约定

- 四语言包各自独立：`sdk/go`（嵌套模块 `github.com/eimsound/lazy-observe/sdk/go`，
  网关经根 `go.mod` 的 `replace => ./sdk/go` 引用）、`sdk/node`（pnpm workspace，包在
  `packages/`）、`sdk/browser`（pnpm workspace，含 browser/react/bundler-plugin）、
  `sdk/python`（`lazyobserve` 包，含 Django/DRF 适配）。
- 本地入口：`mise run sdk-go`、`mise run sdk-node`、`mise run sdk-browser`、
  `mise run sdk-python`；四个语言的 journey（对着真实平台的端到端）由 `mise run sdk-journeys`
  起临时平台后依次跑。CI 由根 `.github/workflows/ci.yml` 的 `changes` 作业按目录
  只跑改动到的语言包，`sdk-journeys` job 在 SDK、服务端或前端改动时跑四个 journey。
- 所有脚本用 `bash`，命名小写连字符；新增脚本需 `sh -n` 通过。
