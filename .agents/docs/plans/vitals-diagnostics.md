# Browser vitals 失败可观测 + diagnostics 端点部分接受

## 目标与来源

- 源：本切片任务说明（`feat: browser vitals 失败上报 diagnostics，diagnostics 端点对未知枚举部分接受`）。
- 最终状态：
  1. `/api/events/diagnostics` 遇到未知 `signal` / `reason` 跳过并计数，响应含 `accepted` / `rejected`；
     结构性错误仍整批 `422`。
  2. browser SDK 的 Web Vitals 上报失败不再静默：`droppedTelemetry` 累加、`console.warn`、
     上报一条 `signal="vitals"` 的 diagnostics；4xx 记 `server_rejected`，网络错误/5xx 记
     `retry_exhausted`。绝不向应用抛错。
  3. `docs/version-matrix.md` §2.1/§2.3/§5、`sdk/AGENTS.md` 第 1 条、`docs/sdk/browser.md`
     与 `sdk/browser/README.md` 与实现同口径；协议版本仍为 v0。
- 范围外：不改 Ent schema、不加依赖、不改 `requirements.md`、不动其它语言的 SDK。

## 仓库证据

- `internal/gateway/error_api.go:230` `sdkDiagnosticsIngest` 把 `validSDKDiagnosticSignal` /
  `validSDKDiagnosticReason` 与结构性校验混在一个 `if` 里，任何一项不通过就整批 `422`；
  `:400` 响应只有 `accepted`；`:373` 起是 #55 引入的有界收集器 `unknownVitalNames`。
- `internal/gateway/error_api.go:102` `vitalsIngest` 已经示范：结构性错误整批 `422`、
  未知枚举跳过 + `skippedNames.add` + 一条 Warn + `accepted`/`rejected` 响应、空批不导入。
- `sdk/browser/packages/browser/src/vitals.ts` 对每个指标单独 POST，失败走
  `.catch(() => undefined)`，是唯一没有 diagnostics 覆盖的信号。
- `sdk/browser/packages/browser/src/client.ts:369` `recordTelemetryDrop` 负责累加
  `droppedTelemetry`、`console.warn`、入队 diagnostics；`exportDiagnostic=false` 是
  diagnostics 自身失败时防递归的现有机制（`:126`）。`client.ts:152` 调用 `startVitals`。
- `sdk/browser/packages/browser/src/transport.ts:7` `DiagnosticReason` 只有三种；
  `post()` 对非 2xx 抛 `Error("... returned <status>")`，调用方无法区分 4xx 与网络错误。
- 文档口径：`docs/version-matrix.md` §2.3/§5 与 `sdk/AGENTS.md` 第 1 条当前写「只有 vitals」。

## 实施策略

- 推荐路线（采用）：
  - **泛化 #55 收集器**为 `unknownEnumValues`（`error_api.go`），vitals 与 diagnostics 共用；
    截断/去重/上限与告警边界不变，避免两套近似实现各自跑偏。
  - **diagnostics 拆分校验**：先做结构性校验（`measurement_id`、时间戳、`service` 1–128 字节、
    非空 `signal`/`reason`、`dropped` 1–1e9），任一条不通过整批 `422`；再对非空但不认识的
    `signal`/`reason` 跳过并计数。`signal` 集合加 `vitals`，`reason` 集合加 `server_rejected`。
    只在 `samples.Len() > 0` 时导入，空批与全未知批不写上游。
  - **SDK 用类型化错误区分 4xx**：`TransportError` 携带 status，`isServerRejection` 判断
    400–499；`startVitals` 增加可选失败回调，回调包在 `try/catch` 里，避免 reporter 抛错变成
    unhandled rejection（SDK 自己监听了 `unhandledrejection`，可能形成回路）。
    `recordTelemetryDrop` 的 drop 参数放宽为 `DiagnosticReason`，vitals 走同一条计数/告警/上报路径。
  - **网络错误/5xx 复用 `retry_exhausted`**：vitals 是单次投递（没有队列、没有重试），
    网络错误或 5xx 意味着这一次投递预算已经耗尽，与队列 `maxAttempts` 用尽同义；不再新增
    第二个枚举值，降低旧服务端损失面。
- 备选路线（否决）：
  - **给 vitals 建 `DeliveryQueue`**：改动面大（重试/keepalive/顺序），且违背「按现状不重试」；
    只为拿到 reason 引入重试会改变上报语义。
  - **从 `Error.message` 里解析状态码**：字符串是日志格式而不是契约，改动 `post()` 的
    文案就会静默把 4xx 记成 `retry_exhausted`。
  - **网络错误用 `shutdown_budget` 或新增 `network_error`**：前者语义是 close 之后的拒绝，
    后者再扩一个枚举值，旧服务端的整批 `422` 损失面更大。

## Tracer Bullet 切片

1. 服务端：先写 `TestSDKDiagnosticsIngest*`（部分接受、全未知不写、空批不写、结构性错误、
   日志边界、溢出汇总），跑红；再改 `error_api.go` 转绿。
2. SDK：先写 `test/vitals-delivery.test.ts`（422、网络错误、5xx、diagnostics 自身失败不递归），
   跑红；再改 `transport.ts` / `vitals.ts` / `client.ts` 转绿。
3. 文档：version-matrix、sdk/AGENTS.md、docs/sdk/browser.md、browser README 同步口径。

每个切片的 red/green 命令与结论见下方执行日志。

## 执行日志

- 2026-11-19 `CGO_ENABLED=0 go test ./internal/gateway -run 'TestSDKDiagnosticsIngest' -count=1`
  （实现前）→ 红：5 个新测试失败，未知枚举仍 `422`、空批仍导入 1 次上游。
- 2026-11-19 同上（实现后）→ 绿。
- 2026-11-19 `mise exec -- pnpm vitest run test/vitals-delivery.test.ts`（实现前）→ 红：
  4 个测试全失败，`droppedTelemetry` 仍为 0。
- 2026-11-19 同上（实现后）→ 绿：4 个测试通过。
