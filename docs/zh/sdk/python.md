# Python SDK（Django 与 DRF）

Python SDK 为 lazy-observe 提供显式的 Django 与 Django REST Framework（DRF）插桩，能力与 [Go/Fiber SDK](go.md) 对齐。导入 `lazy_observe` 不会安装钩子，不会改动全局 OpenTelemetry Provider，也不会调用 `set_meter_provider`、`set_tracer_provider` 或 `set_logger_provider`。

## 安装

本包支持 Python `>= 3.9`，目前位于仓库的 `sdk/python` 目录，尚未发布到包注册表。可在其他项目中从本地检出目录安装：

```sh
pip install /absolute/path/to/lazy-observe/sdk/python
# or, with uv:
uv pip install -e /absolute/path/to/lazy-observe/sdk/python
```

`django` 可选依赖组包含 Django 和 Django REST Framework（仅框架适配器和示例需要，核心 SDK 可独立运行）：

```sh
uv pip install -e "/abs/lazy-observe/sdk/python[django]"
```

## 初始化与关闭

```python
import lazy_observe

lo = lazy_observe.init({
    "dsn": "http://127.0.0.1:3100/p/default@TOKEN",
    "service_name": "checkout-api",
})
if lo.err():
    logging.error("lazy-observe initialization failed; telemetry is degraded", error=lo.err())
```

DSN 格式必须为 `http(s)://host[:port]/p/<slug>@<token>`，不能包含查询参数或片段，解析规则与 Go SDK 完全一致。Token 仅通过 `Authorization: Bearer <token>` 请求头发送。

`init` 会将该客户端注册为全局注册表的当前实例，供依赖注册表的集成组件（`lazy_observe.DjangoMiddleware`、`lazy_observe.drf.exception_handler`、`LazyObserveLogHandler`）解析。

进程退出前请调用关闭方法，以排空有界队列并刷新导出器：

```python
lo.shutdown(timeout=10.0)
```

## 配置

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `dsn` | 必填 | 网关地址、项目与项目 API Token。该 Token 为 `lo_...` 项目 API Token，仅能用于摄取；读取路由会返回 `403 ingest_only_token`。查询需使用 `lou_...` 用户 API Token 并携带 `X-LO-Project` 请求头，见[部署指南](../deployment.md)。 |
| `service_name` | 可执行文件名 | OpenTelemetry `service.name` 及日志字段。 |
| `metrics`、`logs`、`traces` | 全部启用 | 遵循下文信号矩阵规则。 |
| `sample_rate` | `1` | 基于父 Span 的头部采样，范围 `(0, 1]`。 |
| `compression` | `"none"` | OTLP/HTTP 载荷压缩方式（`""`、`"none"` 或 `"gzip"`）。留空时依次读取 `OTEL_EXPORTER_OTLP_<SIGNAL>_COMPRESSION` 与 `OTEL_EXPORTER_OTLP_COMPRESSION`，默认 `"none"`；显式配置优先于环境变量。 |
| `tee_to_stderr` | `true` | 同时通过日志处理器将记录输出到本地 stderr。 |
| `capture_db_statement` | `false` | 在数据库 Span 上附带 SQL 文本，绝不采集绑定参数。 |
| `capture_llm_content` | `false` | 采集 `gen_ai.prompt` 与 `gen_ai.completion`，截断至 4 KiB。 |
| `release`、`environment` | 空 | 附加到错误事件的部署元数据。 |
| `tags` | 空 | 初始化时深拷贝并附加到错误事件的应用标签，调用方后续修改不影响运行中的 SDK。 |
| `export_interval` | `5s` | 指标与日志的导出间隔。 |
| `delivery` | 零值默认 | 错误队列容量与重试预算，支持 `DeliveryConfig` 或嵌套 `dict`。 |
| `profiling` | `false` | **实验特性。** 显式开启持续性能剖析（见下文“持续性能剖析”一节）；仅在 Linux 且关闭 GIL 的自由线程 CPython 3.15 及以上版本采样。 |
| `profiling_interval` | `60`（秒） | 相邻两次 10 秒剖析窗口起始时间之间的间隔；`<= 0`、NaN 和无穷大会回退到 `60`；小于 `10` 时窗口连续运行。 |

当传给 `lazy_observe.init` 的 `dict` 或关键字参数包含无法识别的顶层键或 `delivery.*` 键时，SDK 会在 `lazy_observe` logger 上记录一次警告并列出被忽略的字段，不会静默吞掉拼写错误。

信号矩阵规则：`metrics`、`logs`、`traces` 全为 `false` 时三者全部启用；只要任一字段为 `true`，这三个值就构成显式启用集合。如需关闭全部遥测，不要调用 `init`。

## Django

将 `lazy_observe.DjangoMiddleware` 加入 `MIDDLEWARE`。中间件会完成以下工作：

- 提取 W3C `traceparent` 并创建 SERVER Span，记录 `url.path`、`http.request.method`、`url.scheme`、`server.address` 和协议版本。
- 记录 `http.server.request.duration` 和 `http.server.active_requests` 指标。
- 在 `process_exception` 中捕获未处理的视图异常，以 `exc_info` 记录日志并将异常入队为平台错误事件以聚合 Issue，随后返回 `None`，保持 Django 原生 500 流程（`got_request_exception`、`DEBUG` 错误页、自定义 `handler500`）不变；在非 Django 环境的直接中间件调用中，设置 `config.re_raise_on_500 = True` 会重新抛出异常而不是返回 HTTP 500。
- 附加低基数的 `http.route`（路由解析结果，未匹配时为 `"unmatched"`）和 `http.response.status_code`，并在路由解析后更新 Span 名称。

```python
MIDDLEWARE = [
    "lazy_observe.DjangoMiddleware",
    "django.middleware.common.CommonMiddleware",
]
```

不抛出异常而直接写出 5xx 响应时，请求仍会出现在指标和链路中，但不会创建 Issue，这与 Fiber 保持一致。

### WSGI、ASGI 与流式响应支持边界

- **WSGI 与 ASGI。** `DjangoMiddleware` 和 `lo.django_middleware()` 声明了 `sync_capable = True` 与 `async_capable = True`。在 ASGI 下，当 Django 传入异步 `get_response` 时，中间件原生运行在事件循环上，不经过 `sync_to_async` 线程切换。
- **`StreamingHttpResponse`。** 中间件同时包装同步和异步的 `StreamingHttpResponse` 迭代器，使 SERVER Span、`http.server.request.duration` 和 `http.server.active_requests` 在流结束或响应关闭前保持打开。中间件返回响应对象时会立即解绑 OpenTelemetry 请求上下文，避免跨请求泄漏。
- **数据库包装器。** `lo.django_db()` 基于 Django 同步的 `connection.execute_wrapper`，适用于同步 ORM 与游标执行，包括在异步视图中通过 `sync_to_async` 执行的 ORM 查询。

## DRF

配置异常处理器，使解析为 5xx 的 API 异常携带请求上下文（方法、路由、状态码、去除查询参数的 URL）入队并聚合为 Issue：

```python
REST_FRAMEWORK = {
    "EXCEPTION_HANDLER": "lazy_observe.drf.exception_handler",
}
```

对于非 API 异常，处理器按 DRF 约定返回 `None`，由 `APIView.raise_uncaught_exception` 交给 Django 500 路径和中间件捕获一次。4xx API 错误（如参数校验失败或鉴权失败）不会生成 Issue。

## 日志

在 Django 的 `LOGGING` 中配置处理器，即可将日志导出为 OTLP 日志并附带 `service.name`；当发送上下文中存在活跃 Span 时，还会注入 `trace_id` 和 `span_id`。值不是 `str`、`int`、`float` 或 `bool` 的 `extra` 字段会被忽略。

```python
LOGGING = {
    "version": 1,
    "disable_existing_loggers": False,
    "handlers": {"lazy_observe": {"class": "lazy_observe.LazyObserveLogHandler"}},
    "loggers": {"": {"handlers": ["lazy_observe"], "level": "INFO"}},
}
```

除非显式关闭，处理器同时会将日志输出到 stderr。

## 数据库

使用 Django 显式的 `connection.execute_wrapper` 机制对代码块插桩，不修改任何驱动：

```python
with lo.django_db():
    ...  # ORM / cursor queries inside this block get DB CLIENT spans
```

Span 属性包含 `db.system.name`（由 `ENGINE` 推导）。仅当启用 `capture_db_statement` 时采集 SQL 文本，绝不采集绑定参数值。耗时记录在 `db.client.operation.duration` 指标中。

## LLM 调用

显式包装 `httpx` 传输层，不修改模型厂商 SDK：

```python
import httpx
client = httpx.Client(transport=lo.llm_transport())
# pass through http_client= to openai / anthropic SDKs
```

可识别的请求形态包括 OpenAI 兼容的 `/chat/completions`、Anthropic `/v1/messages` 以及 Ollama `/api/chat`。Span 记录 `gen_ai.operation.name`（`chat`）、`gen_ai.provider.name`（同时保留已弃用的 `gen_ai.system`）、请求与响应模型以及响应中的输入和输出 Token 数。服务端未返回用量时（例如未开启 `stream_options.include_usage` 的流式响应），会记录 `gen_ai.usage.missing=true`，不猜测数值。请先确认隐私合规要求再开启 `capture_llm_content`；开启后提示词与补全内容会被截断至 4 KiB，且由应用自行脱敏。

异步客户端可使用 `lo.llm_transport(client_kind="async")` 获取 `httpx.AsyncBaseTransport`。

## 错误上报

在中间件和 DRF 之外，可显式上报已捕获的错误：

```python
event_id = lo.capture_exception(exc, tags={"http.route": "/checkout"})
```

错误事件携带外层优先顺序的 Python 堆栈（与 Go 一致）、`release`、`environment`、`tags`、`service.name` 以及当前活跃的 `trace_id` 和 `span_id`。中间件会补充请求方法、路由、状态码和去除查询参数的请求 URL。SDK 绝不采集请求体、请求头、Cookie、鉴权凭据或查询字符串。

发送走非阻塞有界队列，每批最多 20 条事件，每 100 ms 刷新一次。

## 采样

`sample_rate` 配置基于父 Span 的头部采样（`ParentBased` 包装 `TraceIdRatioBased`）。链路与日志直接使用 SDK 原生有界批处理器（队列容量 2048，导出批次 512），不额外叠加重复队列。

## 运行时指标

启用指标后，SDK 会采集三项运行时指标：

- `process.runtime.python.threads`：来自 `threading.active_count()`。
- `process.runtime.python.memory.rss`：在 Linux 上读取 `/proc/self/statm` 的实时常驻内存（RSS），在其他 Unix 平台回退至 `resource.getrusage`，不可用时（如 Windows）返回 `0`。
- `process.runtime.python.gc.count`：来自 `gc.get_stats()` 的累计 GC 周期数。

## 持续性能剖析

> **实验特性，默认关闭。** Python SDK 的持续性能剖析依赖未公开文档的 CPython 3.15 内部接口，仅在 Linux 平台、无 GIL 运行的自由线程版本（`python3.15t`）下生效。在其他任何环境中开启都只会输出一条警告且不采集样本，绝不影响应用本身。

持续性能剖析按固定周期采样当前进程的调用栈，并将 CPU 剖析数据（`type=cpu`，gzip 压缩的 pprof）上传至平台的 `POST /api/profiles`。传输契约与 Go SDK 相同，无需改动服务端或协议。

### 开启方式

```python
lo = lazy_observe.init({
    "dsn": "http://127.0.0.1:3100/p/default@TOKEN",
    "service_name": "checkout-api",
    "profiling": True,          # default False
    "profiling_interval": 60,   # seconds between window starts; default 60
})
```

`init` 会立即返回，可用性探测和采样循环运行在名为 `lazy_observe-profiling` 的守护线程中。调用 `lo.shutdown(timeout)` 会立即停止采样；若当前窗口已运行至少 5 秒，会在关闭预算内上传，不足 5 秒则直接丢弃。支持 gunicorn、uWSGI 等 prefork 服务器：每个 worker 进程在 `fork` 后都会重启独立的采样器并开启新窗口，避免预加载阶段主进程采集的样本被各个 worker 重复上传。

### 支持范围

仅当以下条件**全部**满足时才会采样：

- 运行平台为 Linux；
- 解释器为 CPython 3.15 及以上版本，且为自由线程构建（`python3.15t`，`Py_GIL_DISABLED`）；
- 运行时 `sys._is_gil_enabled()` 返回 `False`。

除此之外的环境（包括 CPython 3.9 至 3.14、标准 3.15 GIL 构建、重新开启了 GIL 的 3.15t、macOS 和 Windows）都只会记录一条警告（详见下文“警告码”一节）而不采样。SDK 的其余功能仍然支持 CPython 3.9 及以上版本。

之所以要求关闭 GIL，是因为进程内采样器必须持有 GIL 才能读取调用栈。在 GIL 构建中，采样器只能在线程等待或触发强制切换（默认 5 ms）时观察到该线程。对于典型的 Web 请求这类短 CPU 突发，样本往往落在 `select`、`recv` 等等待调用上，而不是实际消耗 CPU 的代码。误导性的火焰图不如不画，因此 SDK 在 GIL 构建中拒绝采样。要支持 GIL 构建需要进程外采样器，目前尚未实现。

运行期间 GIL 也可能被重新启用，例如导入了未声明支持自由线程的 C 扩展。SDK 在每个采样窗口开始前都会重新检查，一旦发现 GIL 开启就永久停止采样。

### 采样内容

- 仅采集 CPU 时间（`mode=cpu`），覆盖进程内所有线程，频率为 100 Hz，排除 SDK 自身的采样线程。
- 每隔 `profiling_interval` 秒（默认 60）录制一个固定 10 秒的窗口，编码后入队上传。`profiling_interval` 小于 10 时窗口连续执行。
- 上传使用容量为 6 的有界内存投递队列并支持重试。遇到非临时 4xx（408 和 429 除外）或体积超过 10 MiB 的剖析文件时不重试。失败的采样 tick 与丢弃的上传会计入 `Diagnostics().dropped_telemetry`，`signal="profile"`，原因码包括 `sample_failed`、`queue_full`、`retry_exhausted`、`shutdown_budget` 和 `server_rejected`。
- 平台展示的火焰图是所选时间范围内该服务**所有进程与实例的汇总**。窗口不附带 pid 或主机标签，无法按进程或实例拆分查看。

### 实测开销

在 CPython 3.15.2t、100 Hz、栈深 30、单次运行 4 秒、每组 3 轮的条件下，于共享机器（负载 1.3 至 4.5）测得以下数据：

| 忙碌线程 + 空闲线程 | 采样窗口运行期间的采样线程 CPU 占用 |
| --- | ---: |
| 1 + 0 | 单个核心的 1.75% |
| 1 + 15 | 单个核心的 3.60% |
| 1 + 63 | 单个核心的 9.03% |
| 4 + 0 | 单个核心的 2.77% |

- 开销随线程数增长，因为每个 tick 都需要为每个线程读取一次 `/proc/<pid>/task/<tid>/stat`。
- 开销仅发生在 10 秒采样窗口内，在默认 60 秒间隔下，平均分摊开销约为上表数值的六分之一。
- 导入采样器会使 RSS 增加 15 至 19 MiB。
- 忙碌线程的吞吐量三轮平均变化不超过 3.2%，单轮变化约 5%，与未开启剖析时基线本身的轮间波动（最高 4.8%）相当，无法与测量噪声区分。
- 开启剖析会使 `init` 增加约 9 ms 耗时，因为探测在后台线程执行。

以上数据仅代表单台机器上单一工作负载的测量结果，不构成保证。在生产环境大范围启用前，请先针对自身服务实测。

### 不支持 Greenlet

当线程运行在非主 greenlet 中时，CPython 采样器无法回溯调用栈，采样会报 `Incomplete sample: did not reach base frame`。无论采用哪种采样模式、无论 3.15 还是 3.15t 均如此。SDK 不会自动检测 greenlet，请自行判断是否开启。

- **gevent 与 eventlet。** 几乎所有 tick 都会失败，失败率超过 50%，剖析器会以 `sample_failure_rate` 自动关闭。
- **局部使用 greenlet 的库（如 SQLAlchemy asyncio）。** 失败率低于关闭阈值（实测 8% 至 29%），剖析器会保持开启，但 greenlet 内部消耗的 CPU **不会出现在火焰图中**。实测中占真实 CPU 36% 至 44% 的查询路径，在样本里仅占 1% 至 5%。请将这类服务的火焰图视为不完整结果。

### 警告码

无法运行剖析时，SDK 会通过 `lazy_observe` logger 输出一条警告，每个进程对每个警告码最多打印一次（`fork` 出的子进程可能再次警告一次）：

```
lazy_observe continuous profiling is off: reason=<code> (<detail>); see <link to this section>
```

请按 `code` 匹配，不要匹配 `detail` 文本。发出警告后不再采样或上传，且已停止的采样器在同一进程内不会重启，因为触发原因不会自行消失。

| 警告码 | 触发时机 | 处理方式 |
| --- | --- | --- |
| `python_too_old` | CPython 版本低于 3.15（3.9 至 3.14），不存在 `profiling.sampling`。 | 改用 `python3.15t` 运行，或保持 `profiling` 关闭。 |
| `unsupported_platform` | 非 Linux 平台（macOS、Windows）。 | 在 Linux 上运行需要剖析的服务。 |
| `gil_build` | 常规带 GIL 构建，包括标准 3.15。 | 改用自由线程构建 `python3.15t`。 |
| `gil_enabled` | 自由线程构建，但因 `PYTHON_GIL=1`、`-X gil=1` 或不支持自由线程的 C 扩展导致 `sys._is_gil_enabled()` 为 `True`。窗口间隙 GIL 被重新开启并停止采样时也会报此码。 | 移除对应启动参数或重新启用 GIL 的扩展。 |
| `probe_failed` | 启动探测发现当前解释器的 `profiling.sampling` 与 SDK 预期不符：导入失败、`sample()` 签名或 CPU 模式常量变更，或 0.1 秒试采失败。因接口未公开文档，常见于 CPython 升级后。 | 提交 `detail` 文本，在 SDK 适配该版本前保持 `profiling` 关闭。 |
| `too_many_failures` | 连续 3 个窗口失败（合并计入采样和编码错误）。 | 查看 `lazy_observe` logger 的 debug 日志排查原因。 |
| `sample_failure_rate` | 单个窗口内超过 50% 的采样 tick 失败（至少达到 100 次采样尝试才判定，100 Hz 下约 1 秒）。常见于 gevent 和 eventlet。 | 不要将性能剖析与基于 greenlet 的并发模型混用。 |
| `internal_error` | 其他未预期错误，例如剖析线程启动失败或未分类的停止原因，`detail` 中附带异常信息。 | 提交 `detail` 文本以便排查。 |

上述任何情况都不会改变应用行为：SDK 绝不会向业务代码抛异常，失败的采样器只会关闭自身。

## 投递可靠性

错误事件投递采用内存有界队列，非阻塞且保证至少一次（at-least-once）投递。默认配置如下：

| 字段 | 默认值 |
| --- | ---: |
| 错误队列容量 | 100 条事件 |
| 最大尝试次数 | 3 |
| 初始重试延迟 | 100ms |
| 最大重试延迟 | 1s |

队列满时丢弃新事件并记录本地警告；重试预算耗尽后丢弃该批次。`Diagnostics().dropped_telemetry` 暴露进程本地的丢弃总数，启用指标的客户端还会导出带 `signal` 和 `reason` 属性的 `lazy_observe_sdk_dropped_telemetry` 指标。

丢弃的错误事件以 `signal="error"` 计数，原因码为 `queue_full`、`retry_exhausted`、`shutdown_budget` 和 `server_rejected`（覆盖关闭预算耗尽时仍在排队或等待重试的事件、关闭后捕获的事件、被非临时 4xx 拒绝的批次以及超过 200 KiB 单事件上限的事件），与 Go SDK 一致。关闭预算耗尽后，队列不再发起任何新请求，无论是积压批次的首次尝试，还是退避刚结束的重试，这些批次都会记为 `shutdown_budget`，已经在途的请求则允许完成。SDK 不提供磁盘缓冲（disk spool）。

## 与 Go SDK 的能力对照

| Go SDK | Python SDK |
| --- | --- |
| `lazyobserve.Init(Config)` | `lazy_observe.init(cfg)` |
| `lo.Err()` / 降级实例 | `lo.err()` / `lo.degraded` |
| `signalMatrix` 全 `false` 默认全开 | `metrics`/`logs`/`traces` 规则一致 |
| `lo.Fiber()` | `lazy_observe.DjangoMiddleware` |
| Fiber 5xx 捕获 | `lazy_observe.drf.exception_handler` |
| `lo.Slog()` 关联处理器 | `LazyObserveLogHandler` |
| `lo.WrapDB(driver, dsn)` | `lo.django_db()` 上下文管理器 |
| `lo.LLMTransport(base)` | `lo.llm_transport(base=...)` |
| `CaptureException(ctx, err)` | `lo.capture_exception(exc, ...)` |
| `process.runtime.go.*` | `process.runtime.python.*` |
| pprof CPU、heap 与 goroutine | 实验特性，默认关闭且仅采集 CPU（`profiling=True`），仅在 Linux 且关闭 GIL 的自由线程 CPython 3.15 及以上版本生效（见上文“持续性能剖析”一节） |
| `lo.Shutdown(ctx)` | `lo.shutdown(timeout)` |
| `Diagnostics().DroppedTelemetry` | `Diagnostics().dropped_telemetry` |
| `django_db` / `LLM` 内容开关 | `capture_db_statement` / `capture_llm_content` |

## 直接依赖与许可证

| 依赖 | 用途 | 许可证 |
| --- | --- | --- |
| `opentelemetry-api` / `-sdk` | API 与 SDK（`>=1.30,<2.0`） | Apache-2.0 |
| `opentelemetry-exporter-otlp-proto-http` | OTLP/HTTP 导出器（`>=1.30,<2.0`） | Apache-2.0 |
| `httpx` | 内部遥测 HTTP 传输与 LLM 传输层包装（`>=0.24,<1.0`） | BSD-3-Clause |
| `Django` | 中间件与数据库包装器（可选） | BSD-3-Clause |
| `djangorestframework` | DRF 异常处理器（可选） | BSD-3-Clause |
| `protobuf` | OTLP protobuf（由 `opentelemetry-exporter-otlp-proto-http` 固定） | BSD-3-Clause |
