# Node SDK

Node SDK 为 Express、Pino、Winston、运行时指标和平台错误事件提供显式插桩。导入 `@lazy-observe/node` 不会修改任何模块，也不会设置全局 OpenTelemetry Provider。

## 安装

该包目前位于仓库的 pnpm workspace 中，尚未发布到包注册表。在 `sdk/node` 目录下执行：

```sh
mise exec -- pnpm install --frozen-lockfile
mise exec -- pnpm build
```

workspace 内的示例会直接解析 `@lazy-observe/node`。待包正式发布后，再提供注册表安装命令。

## Express 接入

```ts
import express from "express";
import { init } from "@lazy-observe/node";

const lo = init({
  dsn: "http://127.0.0.1:3100/p/default@TOKEN",
  serviceName: "checkout-api"
});
const app = express();
app.use(lo.express());
const logger = lo.pino();

app.get("/", (_req, res) => {
  logger.info("hello from express");
  res.json({ ok: true });
});

app.use(lo.expressError());
app.use((error: Error, _req, res, _next) => {
  res.status(500).json({ error: error.message });
});
```

请将 `expressError()` 注册在所有业务路由之后、应用最终错误处理器之前。进程退出时关闭 Provider：

```ts
await lo.shutdown();
```

## 配置

| 选项 | 默认值 | 说明 |
| --- | --- | --- |
| `dsn` | 必填 | `<origin>/p/<project>@<token>`。该 Token 为 `lo_...` 项目 API Token，仅能用于摄取；读取路由会返回 `403 ingest_only_token`。查询需使用 `lou_...` 用户 API Token 并携带 `X-LO-Project` 请求头，见[部署指南](../deployment.md)。 |
| `serviceName` | `process.title` | OpenTelemetry 服务名与日志字段。 |
| `serviceVersion` | 未设置 | OpenTelemetry 服务版本。 |
| `metrics`、`logs`、`traces` | 各自默认 `true` | 独立控制各信号的开关。 |
| `sampleRate` | `1` | 基于父 Span 的头部采样，范围 `0` 到 `1`。 |
| `compression` | `"none"` | OTLP/HTTP 载荷压缩方式（`""`、`"none"` 或 `"gzip"`）。留空或未设置时依次读取 `OTEL_EXPORTER_OTLP_<SIGNAL>_COMPRESSION` 与 `OTEL_EXPORTER_OTLP_COMPRESSION`，默认 `"none"`；显式配置优先于环境变量。 |
| `teeToStdout` | `true` | 同时在本地输出 Pino 和 Winston 日志。 |
| `exportIntervalMs` | `5000` | 指标与日志的导出间隔（毫秒）。 |
| `runtimeMetricsIntervalMs` | `1000` | 事件循环、堆内存与活跃句柄的采集间隔（毫秒）。 |
| `captureUnhandledErrors` | `false` | 上报未处理的 Promise 拒绝并观察未捕获异常。 |
| `profiling` | `true` | 采集并上传 V8 CPU 和采样堆 pprof 剖析文件。 |
| `profilingInterval` | `60000` | 剖析周期间隔（毫秒），每个周期包含 10 秒 CPU 采样窗口。 |
| `release`、`environment`、`tags` | 未设置 | 复制到错误事件的附加字段。 |

Express 中间件会显式提取 W3C `traceparent`，并记录请求数、耗时与活跃请求数。运行时指标通过 `perf_hooks` 和 unref 定时器采集。

## Pino 与 Winston

`pino` 和 `winston-transport` 均为可选的 peer dependency，仅在调用 `lo.pino()` 或 `lo.winston()` 时按需加载。只有在应用使用对应日志库时才需要安装。

```ts
const pinoLogger = lo.pino({ level: "info" });
```

```ts
import winston from "winston";

const winstonLogger = winston.createLogger({
  transports: [lo.winston()]
});
```

两种集成都会在存在活跃上下文时注入 `service.name`、`trace_id` 和 `span_id`，并导出 OTLP 日志。

## 错误上报

`expressError()` 会在当前活跃的请求 Span 上记录异常，并发送平台错误事件。需要上报已捕获错误时，可直接调用 `reportError`：

```ts
try {
  await chargeCard();
} catch (error) {
  await lo.reportError(error, { url: "/checkout" });
  throw error;
}
```

错误事件入队前会经过内置的 `sanitize` 流水线：默认剥离 URL 凭据、查询参数（`?...`）和片段（`#...`），截断过长的消息、标签和堆栈帧，并将单条事件控制在网关 200 KiB 预算内。有界 `DeliveryQueue` 按字节预算拆分批次，遇到永久性 4xx 拒绝时立即以 `server_rejected` 丢弃，遇到临时故障（`408`、`429`、`5xx`、超时、网络错误）时按 `Retry-After` 或 Full Jitter 指数退避重试。

设置 `captureUnhandledErrors: true` 会注册 `uncaughtExceptionMonitor` 监听器。在 Node 默认抛出行为下，它会同时观察 `uncaughtException` 和未处理的 Promise 拒绝，且不抑制 Node 正常的致命退出语义。建议优先显式处理错误。

## 持续性能剖析与会话回放

性能剖析默认开启，也可显式配置：

```ts
const lo = init({
  dsn,
  serviceName: "checkout-api",
  profiling: true,
  profilingInterval: 60_000
});
```

SDK 通过 Node inspector 会话采集数据，不会开启调试端口。每个周期以 1 ms 间隔采样 10 秒 CPU，并获取一次采样堆剖析，转换为 pprof 并 gzip 压缩后上传至 `/api/profiles`。上传失败时退避最长至 15 分钟；inspector 初始化失败时记录一次警告后关闭剖析。调用 `shutdown()` 会停止剖析器。如果进程无法承受剖析开销，或已有其他 inspector 剖析器占用会话，请设置 `profiling: false`。

会话回放（Session Replay）仅支持浏览器端。

可运行的完整代码见 [Express 示例](https://github.com/eimsound/lazy-observe/blob/main/sdk/node/examples/express/src/main.ts)。
