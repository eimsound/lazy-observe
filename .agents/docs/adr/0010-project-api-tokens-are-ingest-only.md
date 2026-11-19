# 项目 API Token 只能摄取，查询只认会话与用户 API Token

浏览器 SDK 会把项目 API Token 直接写进公开页面，任何访客都能拿到它。此前 `queryRequired` 和 `/mcp` 在收到非 `lou_` 的 bearer 时直接走项目 Token 鉴权，等于把整个项目的日志、链路、Replay、Issue 读权限交给了公开页面的访客。我们决定：项目 API Token 回到需求基线（`.agents/docs/requirements.md` 5.5 节「写入侧由 ingest token 解析项目，查询侧由会话校验」）的 ingest token 语义，只能用于摄取路由；所有读路由（含 `/mcp`、`/api/system/info`）对项目 Token 返回 `403`，错误码 `ingest_only_token`。查询只接受会话 Cookie，或用户 API Token（`lou_…`）加 `X-LO-Project` 选择项目。MCP 的 DSN 形状不变（`/p/<slug>@<token>`），但 token 必须是用户 API Token，slug 通过 `X-LO-Project` 转发。

## 备选方案

- **项目 Token 不可查询，查询只认会话/用户 Token（采用）**：与需求基线一致；读操作携带用户身份，沿用 RBAC 与审计；不需要 schema 变更。代价是 MCP、perf-gate 与 HA/发布冒烟脚本改用用户 Token。
- **给项目 Token 增加 `scope`（`ingest` 默认 / `ingest_query`）（拒绝）**：保留了「可查询的项目 Token」这一与需求基线相悖、又容易被误嵌入浏览器的形态；还需要改两份基线 SQL、管理 API 与 UI，安全收益反而更小。

## 后果

- 项目处于发布前阶段（GLOSSARY 兼容性策略），不保留旧行为或回退开关。
- `perf-gate scale` 区分摄取 Token（`LO_PERF_TOKEN`）与读 Token（`LO_PERF_QUERY_TOKEN`）；`perf-gate query` 只用用户 Token。
