# Go SDK

Go SDK 为 Fiber v3、`slog`、`database/sql`、运行时指标和 pprof 提供显式集成。导入该包不会安装全局钩子，也不会设置全局 OpenTelemetry Provider。

> **破坏性变更。** Go SDK 仅支持 Fiber v3。`lo.Fiber()` 返回 Fiber v3 处理器（`github.com/gofiber/fiber/v3`，`func(fiber.Ctx) error`）。SDK 不支持 Fiber v2，也不提供兼容层，因此 Fiber v2 应用无法调用 `app.Use(lo.Fiber())`。请先参考 [Fiber v3 迁移说明](https://github.com/eimsound/lazy-observe/blob/main/sdk/go/README.md#fiber-v3-migration-notes) 将应用迁移到 v3。由于 SDK 尚未发布带标签的版本，该变更不影响任何已发布版本。

## 安装

Go SDK 是本仓库中的嵌套模块 `github.com/eimsound/lazy-observe/sdk/go`，以 `sdk/go/vX.Y.Z` 标签发布：

```sh
go get github.com/eimsound/lazy-observe/sdk/go@latest
```

在首个 SDK 标签发布前，将模块路径指向本地检出目录：

```sh
go mod edit \
  -replace github.com/eimsound/lazy-observe/sdk/go=/absolute/path/to/lazy-observe/sdk/go
```

在仓库内开发时，请从独立模块目录 `sdk/go/examples/fiber` 运行 Fiber 示例。该目录下的 `go.mod` 将仅用于示例的依赖（例如 `modernc.org/sqlite`）与 `sdk/go/go.mod` 隔离。

## 初始化与关闭

```go
lo := lazyobserve.Init(lazyobserve.Config{
    DSN:         "http://127.0.0.1:3100/p/default@TOKEN",
    ServiceName: "checkout-api",
})
if err := lo.Err(); err != nil {
    slog.Error("lazy-observe initialization failed", "error", err)
}

defer func() {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := lo.Shutdown(ctx); err != nil {
        slog.Error("lazy-observe shutdown failed", "error", err)
    }
}()
```

配置无效时，`Init` 不会 panic，而是返回一个安全的降级客户端。DSN 格式必须为 `http(s)://host[:port]/p/<project>@<token>`，不能包含查询参数或片段（fragment）。

## Fiber 与日志

```go
app := fiber.New()
app.Use(lo.Fiber())
slog.SetDefault(lo.Slog())

app.Get("/orders/:id", func(c fiber.Ctx) error {
    slog.InfoContext(c.Context(), "loading order", "id", c.Params("id"))
    return c.SendStatus(fiber.StatusNoContent)
})
```

`Fiber()` 会提取 W3C `traceparent`，创建服务端 Span，记录请求耗时与活跃请求数指标，并将 panic 恢复为 HTTP 500，同时输出带关联上下文的堆栈日志。对于已恢复的 panic 以及最终状态码为 5xx 的处理器返回错误，中间件会异步将其作为错误事件上报，并由平台聚合为 Issue。`url.scheme` 读取 Fiber v3 的 `Ctx.Scheme()`，仅当应用通过 `fiber.Config{TrustProxy: true}` 显式信任上游代理时才采信转发的协议头。请使用带上下文的 `slog` 方法，以便附加 `trace_id` 和 `span_id`。

如需更严格的宿主隔离，可调用 `lo.FiberWithOptions(lazyobserve.FiberOptions{...})`：

```go
app.Use(lo.FiberWithOptions(lazyobserve.FiberOptions{
    // Map domain errors to status codes without invoking Fiber's ErrorHandler inside the middleware.
    StatusFromError: func(err error) int { ... },
    // Re-panic after recording spans, metrics, logs, and error events when an outer recovery middleware owns HTTP 500 responses.
    RepanicOnPanic:  true,
    // Sanitize sensitive path segments before recording url.path, panic logs, and error-event URLs.
    RedactPath:      func(path string) string { ... },
}))
```

SDK 不会调用 `slog.SetDefault`，是否替换默认 logger 由应用自行决定。

## 数据库插桩

```go
db, err := lo.WrapDB("postgres", os.Getenv("DATABASE_URL"))
if err != nil {
    return err
}
defer db.Close()

row := db.QueryRowContext(c.Context(), "SELECT status FROM orders WHERE id = $1", id)
```

`WrapDB` 替代 `sql.Open`，在不修改或注册驱动的情况下创建客户端 Span 并注册连接池指标。`lo.Shutdown` 会自动注销对应的 `metric.Registration`。对于短生命周期或动态轮换的数据库连接池，请使用 `lo.WrapDBContext(poolCtx, driverName, dsn)`，使连接池指标在 `poolCtx` 取消时（以及 `Shutdown` 时）立即注销，避免回调泄漏。数据库驱动按常规方式导入即可。默认不采集 SQL 文本；设置 `CaptureDBStatement: true` 后会采集查询语句，但绝不采集绑定参数值。

## 配置与能力开关

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `DSN` | 必填 | 网关地址、项目与项目 API Token。该 Token 为 `lo_...` 项目 API Token，仅能用于摄取；读取路由会返回 `403 ingest_only_token`。查询需使用 `lou_...` 用户 API Token 并携带 `X-LO-Project` 请求头，见[部署指南](../deployment.md)。 |
| `ServiceName` | 可执行文件名 | OpenTelemetry `service.name` 及 `slog` 字段。 |
| `Metrics`、`Logs`、`Traces` | 全为 `false` 时全部启用 | 只要任一字段设为 `true`，这三个布尔值就构成显式启用集合。 |
| `SampleRate` | `1` | 基于父 Span 的头部采样；`0` 表示“未设置”并回退为 `1`，显式有效范围为 `(0, 1]`。 |
| `TeeToStderr` | 启用 | 设为指向 `false` 的指针可关闭本地文本输出。 |
| `CaptureDBStatement` | `false` | 在数据库 Span 中附带 SQL 语句文本。 |
| `Release`、`Environment` | 空 | 附加到错误事件的部署元数据。 |
| `Tags` | 空 | 初始化时复制并附加到错误事件的应用标签。 |
| `ExportInterval` | 5 秒 | 指标与日志的周期性导出间隔。 |
| `Profiling` | 启用 | 设为指向 `false` 的指针可关闭 CPU、heap 和 goroutine 性能剖析。 |
| `ProfilingCPU` | 启用 | 设为指向 `false` 的指针可关闭 CPU pprof 采样，同时保留 heap 和 goroutine 快照（适用于其他剖析器占用 `runtime/pprof.StartCPUProfile` 的场景）。 |
| `ProfilingInterval` | 60 秒 | 剖析周期启动之间的基准间隔，首次启动和后续每个周期均附带随机抖动。 |

仅启用指标和链路追踪的示例：

```go
lo := lazyobserve.Init(lazyobserve.Config{
    DSN:     dsn,
    Metrics: true,
    Traces:  true,
})
```

因为三个信号全为零值时会全部启用，如需关闭全部信号，直接不初始化 SDK 即可。

## 错误上报

Fiber 中间件会上报已恢复的 panic 以及最终状态码大于等于 500 的处理器错误。返回 4xx 错误不会创建 Issue。不返回错误而直接写出 5xx 响应时，请求仍会体现在指标和链路中，但不会生成异常事件。

在 Fiber 之外，可通过核心 API 上报错误：

```go
eventID := lo.CaptureException(ctx, err)
```

错误事件包含外层优先顺序的 Go 堆栈、已配置的 `Release`/`Environment`/`Tags`、`service.name` 以及当前活跃的 `trace_id` 和 `span_id`。Fiber 还会补充请求方法、路由、状态码以及去除查询参数后的请求 URL。SDK 不采集请求体、请求头、Cookie、鉴权凭据或 URL 查询字符串。

错误发送使用有界非阻塞队列，每批最多发送 20 条事件。入队前，SDK 会按网关 200 KiB 的单事件预算逐项裁剪，按 UTF-8 安全步长限制消息、异常类型与值、Release、Environment、URL、标签和堆栈帧。队列满时会丢弃新事件，更新 `Diagnostics().DroppedTelemetry` 与丢弃指标，并输出限速的本地警告，不会阻塞请求或刷屏日志。`Shutdown` 会排空队列并报告最近一次导出失败。

## 持续性能剖析

除非显式关闭，性能剖析在调用 `Init` 后自动启动：

```go
enabled := true
lo := lazyobserve.Init(lazyobserve.Config{
    DSN:               dsn,
    ServiceName:       "checkout-api",
    Profiling:         &enabled,
    ProfilingInterval: time.Minute,
})
```

每个周期会先采集 10 秒 CPU 窗口（除非 `ProfilingCPU` 为 `false`），再获取 heap 和 goroutine 快照，随后将 gzip 压缩后的 pprof 负载上传至 `/api/profiles`。首次启动与后续周期间隔均带随机抖动，防止集群内多实例同步出现峰值。如果其他剖析器已经占用 `pprof.StartCPUProfile`，SDK 只记录一次警告，并继续采集 heap 和 goroutine 快照。

剖析上传复用 SDK 专用的摄取 HTTP 客户端与有界 `Delivery` 重试预算（包含 `MaxAttempts`、Full Jitter 指数退避及服务端 `Retry-After` 支持），遇到永久性 4xx 响应或超过 10 MiB 的负载会立即丢弃，绝不导致业务请求失败。如果 `Shutdown` 中断了正在采集的 CPU 窗口，只有已运行至少 5 秒的窗口会被上传；更短的窗口直接丢弃，且不计入丢弃遥测。heap 和 goroutine 快照为瞬时采集，仍会在关闭前完成，所有已入队数据将在 `Shutdown` 上下文截止时间内上传。

可运行的完整示例见 [Fiber 示例](https://github.com/eimsound/lazy-observe/blob/main/sdk/go/examples/fiber/README.md)，包含嵌套订单链路、数据库子 Span、关联日志、可控的快/慢/报错路由以及用于填充 Performance 页面的并发压测生成器。
