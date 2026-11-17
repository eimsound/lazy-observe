# lazy-observe SDKs

lazy-observe 官方客户端 SDK，包含 Go / Node / Browser / Python 四语言包，位于本仓库
`sdk/` 下。各语言包相互独立，可按需选用。

## 包目录

| 语言 | 目录 | 说明 | 文档 |
| ---- | ---- | ---- | ---- |
| Go | `sdk/go` | 嵌套 Go 模块 `github.com/eimsound/lazy-observe/sdk/go`（Fiber 中间件、slog handler、LLM 追踪等） | [docs/sdk/go.md](../docs/sdk/go.md) |
| Node | `sdk/node` | Node.js 客户端（pnpm workspace，包位于 `packages/node`） | [docs/sdk/node.md](../docs/sdk/node.md) |
| Browser | `sdk/browser` | 浏览器端 SDK（pnpm workspace，含 `packages/browser`、`packages/react`、`packages/bundler-plugin`） | [docs/sdk/browser.md](../docs/sdk/browser.md) |
| Python | `sdk/python` | Python 客户端（含 Django / DRF 集成） | [docs/sdk/python.md](../docs/sdk/python.md) |

网关自身通过 `go.mod` 里的 `replace github.com/eimsound/lazy-observe/sdk/go => ./sdk/go`
引用 Go SDK 做自监控，因此 Go SDK 的改动会同时触发服务端 Go 检查。

## 发布

SDK 用带前缀的 tag 从本仓库发布：Go 为 `sdk/go/vX.Y.Z`（Go 嵌套模块约定），
Node / Browser / Python 分别为 `sdk-node-vX.Y.Z`、`sdk-browser-vX.Y.Z`、`sdk-python-vX.Y.Z`。

## 版本兼容

服务端与各 SDK 包的版本兼容关系（协议锚点 **v0**、每个服务端版本对应的推荐 SDK
版本、维护规则与 SDK 降级守则）以 [docs/version-matrix.md](../docs/version-matrix.md)
为事实来源。各 SDK 对私有 ingest API 采用**静默降级**策略：服务端返回 404/4xx、
出现未知字段或未知枚举值时丢弃/缓存重试并上报 diagnostics，**绝不抛错、绝不中断
业务**。详见 [AGENTS.md](AGENTS.md) 与矩阵文档。

## 许可

[MIT](../LICENSE)。
