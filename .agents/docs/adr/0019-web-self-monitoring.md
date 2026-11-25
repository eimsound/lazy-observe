# Web 控制台前端自观测采用登录后下发凭据与独立回放开关

Web 控制台自身需要复用仓内 `@lazy-observe/browser` 与 `@lazy-observe/react` 进行前端自观测，将控制台错误、Web Vitals 与同源 API 请求写入内置自观测项目（`lazy-observe`）。由于网关的项目 API Token（`lo_…`）拥有所属项目的全信号摄取能力（[ADR 0010](0010-project-api-tokens-are-ingest-only.md)），若按传统公开前端的做法把 `self-monitoring-web` Token 直接注入未认证的 `index.html` 或公开接口，公网部署实例会向所有匿名访客暴露平台自观测项目的写入凭据；同时，错误触发的全量 DOM Session Replay 会在每个页面首屏常驻加载 `rrweb`，并在项目级权限未落地前带来管理员视图向 `viewer` 跨角色暴露的风险。

## 决策

- **登录后激活（`authSessionOnly`），不向未认证页面注入 Token**：新增 `GET /api/system/self-monitoring`（`routePolicy{auth: authSessionOnly}`、`Cache-Control: no-store`），仅向正常已登录会话（拒绝未认证、用户 API Token `lou_…` 及 `must_change_password` 会话）下发独立派生的 `self-monitoring-web` 项目 API Token。登出或会话 401 过期时立即调用客户端 `close()`、取消在途引导并清理 `sessionStorage` 中的 `lazy-observe:session-id` 与 `lazy-observe:replay-seq:*`。
- **信号分层与独立回放开关**：新增 `self_monitoring.web`（默认 `true`，采集 JS/React 错误、Web Vitals、同源 Fetch 链路追踪）与 `self_monitoring.web_replay`（默认 `false`，开启时使用 `onErrorOnly: true` 仅在报错时上传前 60 秒回放），两者均受 `self_monitoring.enabled` 门控，`web_replay` 另受 `web` 门控。Source Map 自动导入作为后续独立议题。
- **凭据生命周期全权收口**：因自观测项目受 `protected_project` 保护不允许管理接口手动吊销 Token，启动引导（`openServerMetadata`）负责闭环管理：开启 `self_monitoring.web` 时激活当前平台密钥派生的 `self-monitoring-web` Token 并自动吊销同名的旧密钥历史派生 Token；关闭 `self_monitoring.web` 或总开关时自动吊销所有未吊销的 `self-monitoring-web` Token。
- **项目级权限边界与临时隔离**：为支持按项目控制可见性与成员权限，已另建 Issue #364 推进通用项目级 RBAC（届时内置自观测项目默认仅 `admin` 可见）。在 #364 落地前，`web_replay` 保持默认关闭；当显式开启 `web_replay` 时，控制台对非 `viewer` 可访问路由的主内容区（由中央 RBAC 矩阵 `canAccessRoute` 驱动）、一次性明文 Token 卡片、个人会话/Token 管理卡片及 Replay 播放器保留 `data-lo-block` 隔离。
- **仓内 SDK 源码直引与 `AbortError` 语义**：`web/` 通过 Vite/TS 路径别名直引 `sdk/browser/packages/{browser,react}/src`（与服务端 `go.mod` `replace ./sdk/go` 一致），`.dockerignore` 同步放行 `sdk/browser`，`scripts/ci-changes.sh` 中 `sdk/browser/*` 联动触发 `web` 与 `build`；`sdk/browser` 的 `wrapFetch` 将 `AbortError` 记为 OpenTelemetry `UNSET (code=0)` 并附加 `http.aborted: true`，不标为错误 span（`code=2`）。

## 备选方案

- **未认证 `index.html` 内联注入与公开端点下发（拒绝）**：虽可覆盖 `/login` 与冷启动首屏，但会让所有公网部署默认向未登录访客暴露可写全信号的 `lo_…` Token。
- **另建基于会话 Cookie 的代理摄取入口（拒绝）**：可避免在浏览器内存放置 `lo_…` Token，但会偏离标准 `@lazy-observe/browser` 接入形态，削弱 dogfooding 价值。
- **直接放开管理员在 UI 手动吊销自观测 Token（拒绝）**：破坏 `protected_project` 不变量，可能误吊销后端 `self-monitoring` 凭据。
- **在本决定中先硬编码关闭非管理员对 `lazy-observe` 的访问或提前改造通用 RBAC（拒绝）**：单一团队内亦需要按项目授权，已统一拆入独立 Issue #364 设计，不与前端自观测耦合。

## 后果

- 协议面与部署配置保持兼容且文档与代码同 PR 同步（GLOSSARY 兼容性策略）：未登录页面（`/login`、`/change-password`）在首个认证会话建立前不上报前端自观测遥测；控制台切页产生的取消请求（`AbortError`）不再污染前端 Trace 与 Error 视图。
- 镜像构建与本地发布不再遗漏仓内浏览器 SDK 源码；未来 #364 落地后可按项目级 RBAC 决定是否保留或简化非 `viewer` 路由的整体 `data-lo-block`。
