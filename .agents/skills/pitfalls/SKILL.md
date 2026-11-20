---
name: pitfalls
description: lazy-observe 全仓防坑参考。修改或审查后端（`internal/`、`cmd/`）、Web 控制台（`web/`）、四端 SDK（`sdk/`）或部署/CI/发布链路（`deploy/`、`.github/`、`Dockerfile*`、`scripts/`）时使用。
---

# pitfalls

基于全仓代码审查沉淀的高频缺陷模式。实现、修复或审查对应改动面时，先确认以下通用不变量，再按改动面加载对应参考文件逐项核对。

## 全仓通用不变量

1. **凭据零泄漏**：错误信息、审计与投递 `last_error`、前端列表卡片、常驻弹窗、SDK 异常/`repr`/示例以及跨源重定向一律脱敏或剥离令牌与密钥。
2. **入口有界且 fail-closed**：所有外部写入与查询路径强制项目隔离、请求体与解压尺寸上限、批大小与高基数标签规范化、客户端时间戳钳位、上游超时与 body drain。
3. **跨层契约同源对齐**：后端 DTO/HTTP 状态码、前端展示单位与错误语义、MCP 工具及四端 SDK 协议保持一致，由真实端到端或契约测试守护，不靠过时 stub 掩盖漂移。
4. **并发与多副本可恢复**：共享可变状态全程受锁或封装为不可变快照，后台循环捕获 panic 并响应取消信号，HA 租约使用全局唯一 owner 与单调纪元且被拒时按 `AGENTS.md` 记录 `slog.Warn`。
5. **静默降级与可靠发布**：SDK 内部异常静默降级且不中断宿主业务；部署与 CI 链路保持镜像/产物一致、最小权限与传递依赖边界隔离。

## 按改动面加载参考

- 修改或审查后端（`internal/`、`cmd/`）：读 [`backend.md`](backend.md)
- 修改或审查 Web 控制台（`web/`）：读 [`web.md`](web.md)
- 修改或审查四端 SDK（`sdk/go`、`sdk/node`、`sdk/browser`、`sdk/python`）：读 [`sdk.md`](sdk.md)
- 修改或审查部署、CI、脚本或依赖边界（`deploy/`、`.github/`、`Dockerfile*`、`scripts/`）：读 [`release-ops.md`](release-ops.md)

## 完成判据

提交或审查结论前，逐条核对所涉改动面参考文件中命中的条目，确认新改动未重演已知缺陷或引入同类回归。
