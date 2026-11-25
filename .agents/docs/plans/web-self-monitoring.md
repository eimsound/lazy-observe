# 实施计划：为 Web 控制台接入前端 Self-Monitoring

关联决策：[ADR 0019](../adr/0019-web-self-monitoring.md)｜相关议题：[#364](https://github.com/eimsound/lazy-observe/issues/364)（项目级 RBAC 与内置自观测项目可见性）

## 目标与完成条件

- **目标**：依据 [ADR 0019](../adr/0019-web-self-monitoring.md) 为 `lazy-observe` Web 控制台接入登录后激活的前端自观测：受 `self_monitoring.enabled`（总开关，默认 `true`）、`self_monitoring.web`（默认 `true`，Errors + Web Vitals + 同源 Fetch Tracing）与 `self_monitoring.web_replay`（默认 `false`，开启时 `onErrorOnly: true` 仅报错录制）控制；启动引导全权管理 `self-monitoring-web` 项目 API Token 生命周期（开启时幂等创建并吊销历史密钥派生 Token，关闭时自动吊销）；通过 `GET /api/system/self-monitoring`（`authSessionOnly`、`Cache-Control: no-store`）仅在正常已登录会话下发配置并在登出/401 时注销清理；同步修复 `.dockerignore` 对 `sdk/browser` 的屏蔽及 `sdk/browser` `AbortError` span 语义。
- **范围**：
  - In scope：`self_monitoring.web` / `self_monitoring.web_replay` 配置与 Helm/示例同步、启动自动创建与自动吊销 `self-monitoring-web` Token、网关 `authSessionOnly` 配置端点与 Fiber 4 路径防放大豁免、`sdk/browser` `wrapFetch` `AbortError -> UNSET (code=0)` 修正、前端登录后激活/登出销毁/路由模板 `setPage`/ErrorBoundary/RBAC+凭据 `data-lo-block` 隔离、`.dockerignore` 与 `scripts/ci-changes.sh` 修复、`docs/` 协议与运维文档同步。
  - Out of scope：前端构建产物 Source Map 自动导入（独立议题）；项目级权限与内置自观测项目默认仅 `admin` 可见（已在 #364 独立跟踪）。
- **完成条件（必跑门禁）**：
  1. `mise run vet && mise run test && mise run lint && mise run fmt-check`
  2. `mise run test-meta-pg && mise run helm-lint && mise run helm-template-test && mise run workflow-check`
  3. `mise run sdk-browser`
  4. `mise run web`
  5. `mise run build && mise run mod-tidy-check && mise run e2e-contract-check`
  6. 转 Ready 前还需补齐真实 Chromium 对实时网关开启 `self_monitoring.web` 的端到端验收（Draft 阶段先标明）。

## 切片划分

- **Slice 1**：`self_monitoring.web` + `self_monitoring.web_replay` 配置、Helm/示例同步，以及 `BootstrapSelfMonitoringWebToken` / `RevokeSelfMonitoringWebTokens` 生命周期闭环
- **Slice 2**：网关 `GET /api/system/self-monitoring`（`authSessionOnly`、`Cache-Control: no-store`）与 Fiber 中间件仅对 4 条 browser ingest 路径的 `webSelfMonitoring.Token` 防放大豁免
- **Slice 3**：`sdk/browser` `wrapFetch` 的 `AbortError` span 语义修正，`web/` 登录后初始化、登出清理、ErrorBoundary、低基数 `setPage` 与敏感/非 `viewer` 页面 `data-lo-block` 隔离
- **Slice 4**：`.dockerignore` 放行 `sdk/browser`、`scripts/ci-changes.sh` 联动、`docs/`（version-matrix/deployment/operations）同步及全套门禁验证（Draft PR 先行，真实浏览器 E2E 作为转 Ready 前待补项）
