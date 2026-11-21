# 四端 SDK 防坑参考（`sdk/`）

适用于 `sdk/go`、`sdk/node`、`sdk/browser`、`sdk/python` 的改动与审查（同时遵守 `sdk/AGENTS.md` 中的协议与兼容性约束）。

## 1. DSN 与凭据零泄漏（`#185`、`#187`、`#189`）

- `parseDSN` 等解析失败路径只返回结构化脱敏错误，剥离原始 URL 与 token，避免通过 Go `*url.Error`、Node `cause.input` 或异常消息将明文项目 API Token 打印进宿主日志。
- Python `DSN`/`Config` 的 `__repr__`/`__str__` 与四端示例代码统一隐藏或占位展示 token；Python 传输层使用独立 `httpx.Client(follow_redirects=False)` 隔离全局 `urllib` 状态并拒绝 3xx 重定向（防降级 POST 或跨源泄露 `Authorization`）；Go profiling 等子模块自建带超时 `http.Client`，不复用全局 `http.DefaultClient`。

## 2. 4xx 丢弃契约、`Retry-After` 与单事件 200KB 预算（`#185`、`#187`、`#189`）

- 除 `408`、`429` 外，其余 `4xx` 全部按 `server_rejected` 立即丢弃并记入自诊断计数，不进入退避重试；可重试错误采用带随机 jitter 的指数退避，并遵守服务端 `Retry-After`（默认上限 30s）。
- 在入队或序列化前严格执行单事件 200KB 预算（防止异常文本与堆栈双份序列化膨胀），超限事件先裁剪字段或独立丢弃，杜绝单条 413 毒化整批正常事件。
- Browser 侧约束 `fetch(..., { keepalive: true })` 载荷低于 64KB 浏览器上限；Node 侧使用异步压缩避免 `gzipSync` 阻塞事件循环。

## 3. `in_app` 堆栈归属与异常类型保真（`#185`、`#187`、`#189`）

- `in_app` 判定明确排除标准库、第三方依赖与框架帧（Go 框架/第三方包、Python stdlib/`site-packages`/Django/DRF），确保服务端 `Fingerprint` 聚合与 `Culprit` 定位落在接入方业务代码。
- 保留真实异常类型名（如 Python `DjangoMiddleware` 上报真实异常类名，不硬编码 `exception_type="panic"`）。
- 前端与 Node 栈解析覆盖 Firefox `fn@file:line`、无列号 `:lineno` 与 `window.onerror` 文件名/行号，对 URL 解码异常做保护防 `URIError`；`stripSourceMaps` 不因未设 DSN 跳过剥离，动态路由文件名（如 Next.js `[id]`）规范化编码上传；Python `_pprof` 清洗非法 UTF-8 surrogate。

## 4. 路由低基数与传输封装边界（`#185`、`#189`）

- 未匹配路由的 HTTP 请求统一归类为低基数模板（不回退到原始 `request.path`），并在路由解析完成后同步刷新 span 名称与 `http.route`。
- Go `LLMTransport` 等拦截器读取请求体时设置体积上限且完整恢复 `req.Body` 不篡改调用方数据；`lo.Fiber()` 覆盖 4xx、未匹配路由与自定义 `ErrorHandler`；`slog` `WithGroup` 保持 `trace_id` 层级语义清晰；避免热路径高频调用 `runtime.ReadMemStats` 引发 STW。

## 5. `DeliveryQueue` 并发、关机分片与 `shutdownCtx`（`#185`、`#187`、`#189`）

- 初始化时校验队列参数（如 `BatchSize <= Capacity` 防退化为 1）；入队与关闭在同一锁下判断关闭状态，消除 `enqueue`/`close` 竞态。
- 关机排空（如 `_drain_and_close`）按 `batch_size` 分片发送，避免将全部积压合并为超大请求被 413 拒绝；投递成功后清空 `lastError`，防止历史瞬态失败污染最终 `shutdown()` 结果。
- Go 等语言在文档、示例与实现中，统一在进程收到退出信号准备关机时才创建带超时的 `shutdownCtx`，避免在进程启动时提前创建导致长寿命服务退出时立即超时丢数据。

## 6. Browser Replay 序列持久化与生命周期清理（`#187`）

- 从 `sessionStorage` 复用 `session_id` 时同步持久化并递增 `nextSequence`，防止刷新或新开页将 `nextSequence` 归零覆盖 `0.json.gz` 损坏回放。
- 默认采样与 `onErrorOnly` 模式均配置 `checkoutEveryNms` 周期性输出全量 DOM 快照，避免 60 秒窗口过后回放永久黑屏。
- 异步加载 `rrweb` 与 `vitals` 采集器时做好 `close()` 竞态保护：异步回调落盘前检查已关闭状态，关闭时完整移除已注册事件监听器。

## 7. 框架静默降级与跨平台/无 GIL 兼容（`#189`、`#190`）

- SDK 初始化或运行时错误一律静默降级并释放已创建的 OTel 线程与资源，绝不导致宿主请求 500 或吞掉 Django 原生 500 异常处理管道；中间件兼容 `DisallowedHost`、任意 `init()` 调用时机、嵌套 `delivery` 配置与防重复上报，异常 DB 查询同样记录耗时与错误指标。
- `LazyObserveLogHandler` 等日志处理器挂载至 root logger 时过滤 SDK 内部 logger 并加防重入标记，阻断自反馈放大环路，同时保留 `exc_info` 堆栈。
- 避免在顶层无条件导入 Unix 专属模块（如 Python `import resource`），按平台懒加载保障 Windows 正常导入；针对 Python 3.15t 等无 GIL 环境，对 fork 钩子注册与全局采集状态加锁保护。
