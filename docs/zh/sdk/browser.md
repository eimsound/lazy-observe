# Browser 与 React SDK

Browser SDK 负责采集 JavaScript 错误、Web Vitals、W3C 链路上下文以及隐私优先的会话回放（Session Replay）。React 适配器在此基础上补充 `ErrorBoundary` 与路由追踪。全局监听器和 `fetch`/`XHR` 包装仅在调用 `init()` 时安装，并在调用 `close()` 时恢复。

## 安装

相关包目前位于仓库的 pnpm workspace 中，尚未发布到包注册表。在 `sdk/browser` 目录下执行：

```sh
mise exec -- pnpm install --frozen-lockfile
mise exec -- pnpm build
```

workspace 内的示例会直接解析 `@lazy-observe/browser`、`@lazy-observe/react` 和 `@lazy-observe/bundler-plugin`。

## 初始化

```ts
import { init } from "@lazy-observe/browser";

const client = init({
  dsn: "http://127.0.0.1:3100/p/default@TOKEN",
  release: "2026.07.17",
  environment: "production"
});
```

宿主应用销毁时调用 `await client.close()`。再次调用 `init()` 会自动关闭前一个模块级客户端实例。

## 配置与能力开关

| 选项 | 默认值 | 说明 |
| --- | --- | --- |
| `dsn` | 必填 | `<origin>/p/<project>@<token>`。该 Token 为嵌入公开页面的 `lo_...` 项目 API Token，任何访客都可见。它仅能用于摄取，读取路由会返回 `403 ingest_only_token`。查询需使用 `lou_...` 用户 API Token 并携带 `X-LO-Project` 请求头，见[部署指南](../deployment.md)。 |
| `errors` | `true` | 采集窗口错误与未处理的 Promise 拒绝。 |
| `vitals` | `true` | 采集 LCP、CLS、INP、TTFB、FCP，并附带低基数的 `device_type`、`browser_family` 和 `os_family` 维度；传入对象形式 `{ country: "US" }` 可显式附加两位 ISO 国家/地区代码。 |
| `country` | 未设置 | 可选的显式两位 ISO 3166-1 alpha-2 国家/地区代码，附加到 Web Vitals。不会从浏览器语言或时区推断，可在运行时调用 `setCountry()` 更新。 |
| `fetchTracing` | `true` | 包装 `fetch` 和 `XHR`；对象形式可控制自动包装与白名单。 |
| `replay` | 启用 | 布尔值或会话回放配置对象。 |
| `release`、`environment` | 未设置 | 附加到错误事件的字段。 |
| `beforeSend` | 未设置 | 修改或丢弃错误事件的回调。 |
| `errorLimitPerMinute` | `60` | 单页每分钟错误风暴上限。 |
| `errorBatchSize` | `10` | 单批发送的错误事件条数。 |
| `errorFlushIntervalMs` | `5000` | 错误事件的周期性刷新间隔（毫秒）。 |

显式关闭部分能力的示例：

```ts
init({
  dsn,
  vitals: false,
  replay: false,
  fetchTracing: { autoPatch: false }
});
```

### Web Vitals 维度

Web Vitals 上报会附带归一化为固定枚举的低基数维度（`device_type`、`browser_family` 和 `os_family`），绝不将原始 User-Agent 字符串或 IP 地址写入指标标签。SDK 不会根据浏览器语言或时区猜测地理位置。省略 `country` 时，位于 `security.trusted_proxies` 后的网关可从边缘请求头（`CF-IPCountry`、`CloudFront-Viewer-Country`、`X-Vercel-IP-Country`、`X-Country-Code`）推导国家/地区；也可以通过 `init({ country: "US" })`、`init({ vitals: { country: "US" } })`、`client.setCountry("US")` 或 `setCountry("US")` 显式传入两位 ISO 3166-1 alpha-2 代码，传入 `undefined` 即可清除。

## 错误上报

```ts
const eventId = client.captureException(error, {
  level: "warning",
  tags: { operation: "checkout" }
});
```

模块级 `captureException()` 作用于最新初始化的客户端。`beforeSend` 可返回修改后的事件，或返回 `null`/`undefined` 将其丢弃。错误事件会携带浏览器与操作系统上下文、当前会话回放 ID、页面 URL，以及可用时的 SDK 活跃链路上下文。

## 遥测丢弃诊断

错误、Span、Web Vitals、会话回放分段和丢弃诊断均使用有界、协议感知的内存队列。遇到永久性 4xx 拒绝时，队列会立即按 `server_rejected` 丢弃；遇到 `408`、`429`、`5xx` 及临时故障时，按 `Retry-After` 或 Full Jitter 指数退避重试，强制执行单次尝试超时，并按字节预算拆分批次（Web Vitals 还会按每次最多 32 个不同页面切分批次）。尚未发送的 Web Vitals 按 `(name, page)` 去重并保留最新值；批次拆分在页面卸载时遵循 64 KiB 的 `keepalive` 上限（`visibilitychange` / `pagehide` 即使遇到进行中的异步重试也会同步发出 `keepalive` 请求），并与 200 KiB 的单条错误事件限制对齐。

错误事件入队前会经过内置的 `sanitize` 流水线，剥离 URL 凭据、查询参数与片段，并限制超大字段和堆栈帧。任何摄取失败都不会向页面抛出异常：每次丢弃都会递增 `client.diagnostics().droppedTelemetry`，输出一次本地 `console.warn`，并上报至 `/api/events/diagnostics`。

| `reason` | 含义 |
| --- | --- |
| `queue_full` | 有界队列已满，SDK 丢弃了新的遥测数据。 |
| `retry_exhausted` | 在耗尽配置的重试预算后仍未投递成功。 |
| `shutdown_budget` | 遥测数据在 `close()` 之后入队。 |
| `server_rejected` | 摄取端点返回 4xx（不包含属于临时错误的 408 和 429）。负载按原样不会被接受，因此不再重试。 |

上报的 `signal` 取值为 `error`、`trace`、`vitals`、`replay` 或 `diagnostic`。诊断上报自身失败时仅在本地计数，不会递归上报。

如果旧版服务端遇到不认识的 `signal` 或 `reason`，会对整批诊断请求返回 `422`，同批中服务端本已识别的计数（例如 `error` / `queue_full`）也会一起被拒。该批次会留在队列中，在诊断队列的有限投递预算内退避重试；预算耗尽后，SDK 丢弃整批数据，按批次条数增加 `droppedTelemetry`，并仅在本地记为 `diagnostic` / `retry_exhausted`，防止递归。SDK 依然静默降级，仅输出一次 `console.warn`，不抛出异常。

## Fetch 与 XHR 链路追踪

默认追踪同源请求。如需追踪跨域目标，请将其加入 `allowlist`，并在目标服务的 CORS 策略中放行 `traceparent` 请求头：

```ts
init({
  dsn,
  fetchTracing: {
    allowlist: ["https://api.example.com/", /^https:\/\/edge\d+\.example\.com\//]
  }
});
```

如需避免全局修改，可显式包装 `fetch`：

```ts
const client = init({ dsn, fetchTracing: { autoPatch: false } });
const tracedFetch = client.wrapFetch(window.fetch.bind(window));
```

请求头中若已存在 `traceparent`，包装器会原样保留，不再重复创建客户端 Span。若不存在 `traceparent`，包装器会生成已采样的 W3C 上下文用于链路传播和错误关联，并向 `/v1/traces` 导出浏览器客户端 Span。

## 会话回放

会话回放默认采用 10% 普通会话采样加上全量错误回溯。已采样的会话每 10 秒或每 500 KiB 刷新一次；未采样的会话在内存中保留 60 秒环形缓冲区，发生错误时将其上传。

```ts
init({
  dsn,
  replay: {
    sampleRate: 0.05,
    onErrorFullCapture: true,
    bufferMs: 60_000,
    blockSelector: "[data-private]"
  }
});
```

设置 `onErrorOnly: true` 可将普通采样率设为 0，同时保留错误回溯；设置 `replay: false` 可关闭录制。`rrweb` 是 `@lazy-observe/browser` 的可选 peer dependency，启用会话回放时请安装 `rrweb`。如果未安装 `rrweb`，回放模块仅记录一次警告并静默降级，不影响错误和链路采集。`rrweb` 默认遮蔽所有输入框并屏蔽 `[data-lo-block]` 元素；自定义 `blockSelector` 会替换该默认选择器。回放模块异步加载 `rrweb`，通过固定路径 `/api/replay/segments` 配合 `X-LO-Replay-Session` 与 `X-LO-Replay-Seq` 请求头上报分段以保持跨域预检可缓存，并在浏览器支持 `CompressionStream` 时启用 gzip 压缩。

## React

`@lazy-observe/react` 将 `@lazy-observe/browser` 和 `react` 声明为 peer dependency，由应用统一管理单个共享的 Browser SDK 实例。

```tsx
import { ErrorBoundary, useRouteTracking } from "@lazy-observe/react";

function RoutePage({ pathname }: { pathname: string }) {
  useRouteTracking(pathname);
  return <Page />;
}

<ErrorBoundary fallback={(error) => <p>{error.message}</p>}>
  <App />
</ErrorBoundary>
```

不传参调用 `useRouteTracking()` 会监听浏览器的前进、后退和 hash 变化；客户端路由跳转请传入当前 pathname。记录页面路径时会自动移除查询字符串和片段。

浏览器端性能剖析和 React `withProfiler` 尚未实现。

## Source Map

上传 Source Map 必须使用 **Source Map 上传 Token（`los_...`）**，不能使用浏览器端使用的项目 API Token。`@lazy-observe/bundler-plugin` 会在上传前于本地校验 `los_` 前缀，直接拒绝 `lo_...` 和 `lou_...` Token（且不在报错中回显 Token）；服务端上传端点也会对项目 API Token 返回 `403 source_map_upload_token_required`。上传 Token 只能向所属项目上传 Source Map，不能摄取或读取任何数据。

1. 具备管理员权限的成员（与管理项目 API Token 的全局角色相同）打开 Settings，依次进入 Projects、Manage tokens、Source map upload tokens，创建 Token 并复制仅展示一次的 DSN（`<origin>/p/<project>@los_...`）。
2. 将该 DSN 存入 CI 密钥（例如 `LO_DSN`），只向构建任务开放。不要将其写入页面、打包产物或提交到代码仓库。若发生泄漏，在同一弹窗中吊销即可。
3. 浏览器端的 `init({ dsn })` 继续使用普通项目 DSN（`lo_...`）。

`@lazy-observe/bundler-plugin` 负责上传构建产出的 Source Map。它仅提供 ESM 格式，无运行时依赖，要求 Node.js `>= 22.15`。Vite 项目使用插件，其他构建工具使用 `lazy-observe-sourcemaps` CLI，同一项目中不要同时使用两者。

```ts
import { defineConfig } from "vite";
import { vite as lazyObserve } from "@lazy-observe/bundler-plugin";

export default defineConfig({
  build: { sourcemap: true },
  plugins: [lazyObserve({
    dsn: process.env.LO_DSN,
    release: process.env.RELEASE
  })]
});
```

插件选项如下：

| 选项 | 默认值 | 说明 |
| --- | --- | --- |
| `dsn` | 未设置 | 携带 Source Map 上传 Token（`los_...`）的网关 DSN，通常来自 CI 密钥。缺失或为空时，构建会打印警告、跳过上传并正常通过。 |
| `release` | 7 位 Git SHA | 上传和堆栈还原使用的版本标识。仅在上传时需要：未配置 DSN 时，缺少 `.git` 目录不会导致构建失败；配置了 DSN 且未指定 `release` 又没有 Git 仓库时，`vite build` 会报错 `unable to infer release from git; set release explicitly`。 |
| `upload` | `true` | 构建完成后上传产出的 `.map` 文件。 |
| `stripSourceMaps` | `false` | 上传成功后，或未配置 DSN 跳过上传时，删除本地 `.map` 文件；已配置上传但失败时保留本地 `.map` 文件。 |

Vite 插件会注入 `__LAZY_OBSERVE_RELEASE__`，将其传入浏览器端初始化配置即可：

```ts
declare const __LAZY_OBSERVE_RELEASE__: string | undefined;
init({ dsn, release: __LAZY_OBSERVE_RELEASE__ });
```

当无法推断版本且当前不需要版本（未配置 DSN、`upload: false` 或 `vite dev`）时，`__LAZY_OBSERVE_RELEASE__` 会定义为 `undefined`，上报的事件将不带 `release` 字段。

### 其他构建工具与 Next.js

```sh
lazy-observe-sourcemaps upload --dir <directory> [--dsn <dsn>] [--release <release>] [--strip]
```

`--dsn` 默认读取 `$LO_DSN`。`--release` 默认先读取 `$LO_RELEASE`，其次取 7 位 Git SHA；配置了 DSN 且未指定版本又没有 Git 仓库时以状态码 1 退出，未配置 DSN 时不会尝试推断版本。Browser SDK 不会自动从构建产物中读取版本号，需将同一版本号传入 `init({ release })`。Next.js 16 默认使用 Turbopack 构建，请使用 CLI 方式：在 `next.config` 中设置 `productionBrowserSourceMaps: true`，并在 `postbuild` 脚本中运行 `lazy-observe-sourcemaps upload --dir .next/static --strip`。完整示例见 [bundler-plugin README](https://github.com/eimsound/lazy-observe/blob/main/sdk/browser/packages/bundler-plugin/README.md)。

在 CI 中请通过密钥（如 `LO_DSN`）提供上传 Token DSN。未设置 DSN 时（例如本地构建），插件和 CLI 会打印警告、跳过上传并保持成功；设置 DSN 后若上传失败，Vite 构建会失败，CLI 会返回非零退出码。

服务端按压缩文件的 basename 匹配 Source Map，因此输出目录树中的 basename 必须唯一，插件和 CLI 会拒绝重复的 basename。
