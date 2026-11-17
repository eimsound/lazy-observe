# Fiber 可观测示例

该示例同时演示 Fiber v2 的 HTTP 指标、分布式 Trace、关联日志、`database/sql` 子 span、自定义业务指标和持续性能剖析。它不会安装全局 OpenTelemetry Provider，所有能力都由 `lazyobserve.Client` 显式提供。

## 启动

先启动 lazy-observe，使用首次启动日志中的项目 API Token：

```sh
cd sdk/go
LO_DSN='http://127.0.0.1:3100/p/default@lo_xxx' \
  mise exec -- go run ./examples/fiber
```

示例默认监听 `127.0.0.1:3320`。可通过 `PORT` 修改端口；不需要持续性能剖析时设置 `LO_PROFILING=false`。

## Trace 演示

```sh
curl http://127.0.0.1:3320/trace/order-42
```

一次请求会形成以下调用链：

```text
GET /trace/:orderID
├── validate order
├── load order
│   └── SQL query
└── calculate order total
```

响应会返回 `trace_id`。处理日志携带相同 ID，可直接在 lazy-observe 的 Trace 和日志页面交叉查询。

## 性能测量演示

三个路由提供稳定且可区分的延迟/错误特征：

| 路由 | 行为 | 预期状态 |
|---|---|---|
| `/performance/fast` | 约 5ms | 200 |
| `/performance/slow` | 约 150ms | 200 |
| `/performance/error` | 约 30ms 的模拟上游错误 | 503 |

Fiber 中间件记录每个路由的 HTTP span、`http.server.request.duration` 直方图和活跃请求数。示例还记录 `example.work.duration` 与 `example.work.requests` 两个业务指标。

使用同一个示例二进制生成并发流量：

```sh
mise exec -- go run ./examples/fiber load \
  --url http://127.0.0.1:3320 \
  --duration 30s \
  --concurrency 8
```

流量按固定比例覆盖快、慢、错误和订单 Trace 路由。等待约 1 秒导出后，在 lazy-observe 的“性能”页面查看吞吐量、P50/P95/P99 和错误率，在“Trace”页面查看具体慢请求。

其他已有路由可通过 `/` 返回的列表查看；`/db`、`/llm`、`/panic` 和
`/cpu` 分别演示数据库、LLM、异常捕获和 CPU profiling。访问 `/panic`
后，panic 会同时写入关联日志和 Trace，并通过错误事件协议聚合到“问题”
页面；`/performance/error` 只返回模拟的 503 响应，没有异常对象，因此只
计入性能错误率，不会伪造 Issue。
