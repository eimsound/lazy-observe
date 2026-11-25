# 服务端与客户端 SDK 版本兼容矩阵

本文件是 lazy-observe 服务端与四种客户端 SDK（Go / Node / Browser / Python）之间版本兼容关系的事实来源（source of truth）。服务端发版、SDK 发版、协议变更都以此文档为准，并随发布持续更新。

## 1. 总则：为什么用声明式矩阵，而不是运行时握手

当前处于首次发布前（pre-release）阶段，业务量小、变更频繁，为“服务端与 SDK 协商协议版本”引入运行时握手（例如协议版本请求头加 SDK 上报自身版本再触发服务端行为分支）的成本高于收益：它要求两端同时上线协商逻辑、维护版本映射表、处理未知版本回退，而在 pre-release 阶段这些逻辑几乎每次发布都会被重写或删除。

因此现阶段采用声明式矩阵与 CI 强制的组合：

- 统一协议版本号：见“协议锚点”，由矩阵文档承载；
- 声明式矩阵文档：即本文件，服务端协议版本为锚点，`sdk/` 下各语言 README 引用本文件；
- 单仓强制：服务端与四个 SDK 位于同一仓库（`sdk/{go,node,browser,python}`，见 [ADR 0012](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0012-monorepo-sdks-and-github-hosting.md)），协议改动与本矩阵在同一个 PR 里修改，同一次 CI 同时覆盖两端（见第 4 节“维护规则”）；
- SDK 降级守则：私有 ingest 端点返回 404/4xx 时静默降级并上报 diagnostics，绝不对应用抛错。

进入正式可用（GA）后，矩阵将升级为强校验：服务端与 SDK 上线 `X-LO-Protocol-Version` 请求头与 SDK diagnostics 版本上报，CI 把“矩阵一致性”从人工维护的文档约定提升为自动化检查。第 6 节给出了完整迁移路径。

## 2. 协议锚点（Protocol Anchor）

当前 pre-release 的服务端协议基线为 **v0**（尚无握手、尚无版本协商字段）。服务端以二进制编译期注入的 `main.Version`（pre-release 为 `dev`，发布时由 `-ldflags` 注入语义化版本）配合此协议版本对外服务。服务端协议版本由本矩阵文档唯一记录，不写入任何请求字段。

### 2.1 当前受支持的协议面

| 协议面 | 端点 | 说明 |
| --- | --- | --- |
| OTLP / metrics (HTTP) | `POST /v1/metrics` | OpenTelemetry 协议，标准 OTLP/HTTP 载荷；支持可选 `Content-Encoding: gzip`（`x-gzip`），强制 52 MiB 解压体积上限并按解压字节计入 `ingest_rate_limit`，远程 VictoriaMetrics 上游走流式校验透传，All-in-One 本地回环与 `kind: prometheus` 后端统一池化解压为明文转发 |
| OTLP / logs (HTTP) | `POST /v1/logs` | 由网关转发至 VictoriaLogs `insert/opentelemetry/v1/logs`；支持可选 `Content-Encoding: gzip`（`x-gzip`），强制 52 MiB 解压上限并按解压字节限流，远程上游流式校验透传，本地回环池化解压为明文 |
| OTLP / traces (HTTP) | `POST /v1/traces` | 由网关转发至 VictoriaTraces `insert/opentelemetry/v1/traces`；支持可选 `Content-Encoding: gzip`（`x-gzip`），强制 52 MiB 解压上限并按解压字节限流，远程上游流式校验透传，本地回环池化解压为明文 |
| OTLP / traces (gRPC) | `opentelemetry.proto.collector.trace.v1.TraceService/Export` | 原生 OTLP/gRPC 摄入（默认地址 `:4317`，通过 `otlp_grpc.enabled` 启用，支持 gzip 与原生 TLS），转发至 VictoriaTraces `insert/opentelemetry/v1/traces` |
| OTLP / metrics (gRPC) | `opentelemetry.proto.collector.metrics.v1.MetricsService/Export` | 原生 OTLP/gRPC 摄入（默认地址 `:4317`），转发至 VictoriaMetrics `opentelemetry/v1/metrics` 并注入受信任 `extra_label=project_id=<slug>` |
| OTLP / logs (gRPC) | `opentelemetry.proto.collector.logs.v1.LogsService/Export` | 原生 OTLP/gRPC 摄入（默认地址 `:4317`），转发至 VictoriaLogs `insert/opentelemetry/v1/logs` |
| 私有 ingest | `POST /api/events/errors` | 错误事件上报 |
| 私有 ingest | `POST /api/events/vitals` | Web Vitals 上报；未知指标名跳过并计数（§5），响应含 `rejected`；支持可选低基数设备与地理维度字段（见下方说明） |
| 私有查询 | `GET /api/v1/performance/vitals` | 项目隔离的 Web Vitals 聚合与维度分解查询（`by_page`、`by_device`、`by_browser`、`by_os`、`by_country`），支持 `page`/`device_type`/`browser_family`/`os_family`/`country` 筛选 |
| 私有 ingest | `POST /api/events/diagnostics` | SDK 诊断信息上报；未知 `signal` / `reason` 跳过并计数（§5），响应含 `rejected` |
| 私有 ingest | `POST /api/replay/segments` | 会话回放分段上报；采用固定路径并通过 `X-LO-Replay-Session` 与 `X-LO-Replay-Seq` 请求头传递元数据（保留 `session_id` / `seq` query 兼容回退，同时存在且不一致时返回 `400`，CORS 放行这两个请求头）；对单层单 member 且无尾随数据的 `Content-Encoding: gzip`（`x-gzip`）负载强制 5 MiB 解压上限并在 `len(rawGzip) < len(decoded)` 且 `<= 5 MiB` 时跳过二次重压缩直接透传保存原始 gzip，同时保留基于解码明文字节的 `sha256` 去重与 `ingest_rate_limit` 计数 |
| 私有 ingest | `POST /api/profiles` | 持续剖析（pprof）上报 |
| 私有上传 | `POST /api/releases/:release/sourcemaps` | Source Map 制品上传；只接受 Source Map 上传 Token（`los_...`，§2.2），项目 API Token 与用户 API Token 返回 `403` `source_map_upload_token_required` |
| 自身运维指标 | `GET /metrics` | Prometheus 文本格式 0.0.4（`text/plain; version=0.0.4; charset=utf-8`）；通过 `metrics_endpoint.enabled` 显式开启（默认关闭），默认需管理员鉴权，可选静态 scrape Bearer Token 或显式 `allow_unauthenticated`（§2.2） |
| 控制台管理 | `GET`/`POST /api/alert-rules`、`PATCH`/`PUT /api/alert-rules/:id` | 项目隔离的原生告警规则增删改查（需会话 Cookie 或用户 API Token，拒绝项目 API Token）；规则可选携带按规则覆盖的 `evaluation_interval`（见下方说明） |

`POST /api/events/diagnostics` 的枚举值当前为：

- `signal`：`error` / `trace` / `vitals` / `replay` / `diagnostic`；
- `reason`：`queue_full` / `retry_exhausted` / `shutdown_budget` / `server_rejected`。

`POST /api/events/vitals` 每条指标项可选携带以下低基数维度字段（缺省完全向后兼容，协议版本仍为 **v0**）：

- `device_type`：仅限 `desktop` / `mobile` / `tablet` / `unknown`；客户端省略时服务端按 `User-Agent` 低基数回退归一，非白名单值一律归一为 `unknown`；
- `browser_family`：仅限 `chrome` / `edge` / `safari` / `firefox` / `opera` / `other`；客户端省略时服务端按 `User-Agent` 回退归一，非白名单值一律归一为 `other`；
- `os_family`：仅限 `windows` / `macos` / `ios` / `android` / `linux` / `chromeos` / `other`；客户端省略时服务端按 `User-Agent` 回退归一，非白名单值一律归一为 `other`；
- `country`：2 位大写 ASCII ISO 3166-1 alpha-2 国家码（如 `US`/`CN`，排除 `XX`/`T1`/`ZZ`/`A1`/`A2`/`O1` 等未知占位码，非法值归一为 `unknown`）。仅当请求来自 `security.trusted_proxies` 信任的代理时，优先提取标准边缘头（`CF-IPCountry`、`CloudFront-Viewer-Country`、`X-Vercel-IP-Country`、`X-Country-Code`）；无受信任边缘头时才采纳 SDK 显式传入的合法 2 位国家码，否则一律记为 `unknown`。服务端绝不将客户端真实 IP、原始 `User-Agent` 或自由文本写入指标标签。

`GET /api/v1/performance/vitals`（需会话 Cookie 或用户 API Token，拒绝项目 API Token 并强制项目隔离）返回 `{ start, end, step_seconds, filters, summary, total_samples, by_page, by_device, by_browser, by_os, by_country, truncated }`，历史无维度标签的序列在聚合与筛选时自动归入 `unknown` / `other`。

告警规则的 `evaluation_interval`（可选字段，缺省完全向后兼容，协议版本仍为 **v0**）：

- 请求：Prometheus 时长字符串，如 `30s`、`1m30s`。`PATCH`/`PUT` 省略该键保持原值，传 `""` 或 `null` 恢复为信号默认值；服务端按规范形式存储（`60s` 存为 `1m`）。
- 取值范围：须为 `5s`（评估主循环 tick）的整数倍，`metrics` 规则 `5s`–`1h`，`logs` 规则 `30s`–`1h`；越界值，以及切换信号后已存覆盖值超出新信号范围的更新，返回 `400`。
- 响应：`evaluation_interval` 为已存的覆盖值（`""` 表示信号默认），只读的 `effective_evaluation_interval` 为评估器实际使用的间隔（未覆盖时 `metrics` 为 `5s`，`logs` 为 `1m`）。

服务端不认识的新增枚举值不会导致整批拒绝，详见 §2.3 与 §5。

后端三端 SDK（Go / Node / Python）的 OTLP/HTTP 载荷压缩默认保持 `"none"`，可通过显式 `Compression` / `compression` 配置或标准 `OTEL_EXPORTER_OTLP_COMPRESSION`、`OTEL_EXPORTER_OTLP_<SIGNAL>_COMPRESSION` 环境变量按信号启用 `"gzip"`（优先级：显式配置 > 信号级环境变量 > 全局环境变量 > `"none"`）。网关对未压缩、池化解压与流式透传三种路径统一强制 52 MiB 解压体上限，并统一按解压后的明文字节数计入 `ingest_rate_limit`。

#### 告警通知载荷

当服务端配置了 `external_url`（`LO_EXTERNAL_URL`）时，出站告警通知会在发送阶段派生绝对控制台链接（未配置 `external_url` 时省略所有链接字段，载荷与既有 **v0** 完全一致；新增可选字段向后兼容，协议版本保持 **v0**）：

- **Webhook JSON**：顶层可选字符串字段 `url` 与每条告警可选字符串字段 `alerts[].url`（未配置 `external_url` 时省略；风暴与渠道测试通知不填 `alerts[].url`）。Issue 告警既有的相对路径字段 `annotations.link` 与 `annotations.issue` 保持不变。
- **Email（`smtp`）**：邮件正文为缩进 JSON 载荷，编码时不做 HTML 转义（`&`、`<`、`>` 保持原样）。存在顶层 `url` 时，正文首行为 `View in lazy-observe: <url>`，随后接 `\r\n\r\n` 与 JSON。
- **控制台深链查询参数**：控制台链接基于 `<external_url>` 构造，任意控制台路由均支持 `project=<slug>`，`/alerts` 页面还支持 `fingerprint`、`rule` 与 `status` 查询参数（例如 `/alerts?fingerprint=<fp>&project=<slug>`、`/alerts?project=<slug>&rule=<rule>`、`/alerts?project=<slug>&status=firing`、`/issues/<id>?project=<slug>`、`/issues?project=<slug>` 与 `/alerts/channels`）。当前登录用户无法打开链接中的 `project`（已删除、已改名或无权限）时，控制台显示错误页，不会回落到其他项目。

### 2.2 DSN 格式

用户 DSN 格式为：

```
http(s)://<host>/p/<project-slug>@<token>
```

其中 `<token>` 是 `lo_...` 前缀的项目 API Token，SDK 将其解析为项目标识与鉴权凭据，但只通过 `Authorization: Bearer <token>` 请求头发送，不得把完整 DSN 作为请求明文。使用原生 OTLP/gRPC（默认 `:4317`）时，同样通过 gRPC incoming metadata `authorization: Bearer <token>`（scheme 大小写无关）携带项目 API Token：缺失、格式错误或无效/已吊销 Token 返回 `Unauthenticated`；用户 API Token（`lou_...`）与 Source Map 上传 Token（`los_...`）返回 `PermissionDenied`；认证存储或上游存储故障返回 `Unavailable`（错误信息脱敏）。

项目 API Token 只能摄取（[ADR 0010](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0010-project-api-tokens-are-ingest-only.md)）：SDK DSN 里的 Token 只用于 §2.1 列出的摄取端点。所有读路由（含 `/mcp`、`/api/system/info`）收到项目 Token 时返回 `403`，响应体 `code` 为 `ingest_only_token`；读操作只接受会话 Cookie（新签发会话令牌使用 `lss_...` 前缀，与 `los_...` 上传令牌隔离，存量会话按摘要平滑兼容），或用户 API Token（`lou_...`）加 `X-LO-Project` 请求头选择项目。

Source Map 上传（[ADR 0018](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0018-source-map-upload-token.md)）使用独立的项目级保密凭据 Source Map 上传 Token（`los_...`）。DSN 形状不变（`http(s)://<host>/p/<project-slug>@<los_...>`），bundler-plugin 与 `lazy-observe-sourcemaps` CLI 照常解析 DSN，只是其中放的是上传 Token；它只放进 CI secret，不下发给浏览器。admin（全局角色，与项目 API Token 的管理权限相同）在 Web UI“设置 → 项目 → 管理令牌”创建与吊销（`GET|POST /api/projects/:id/source-map-upload-tokens`、`DELETE /api/projects/:id/source-map-upload-tokens/:tokenID`，均需 admin；值只在创建响应里返回一次，创建与吊销写审计 `source_map_upload_token.create` / `source_map_upload_token.revoke`）。鉴权规则如下：

- `POST /api/releases/:release/sourcemaps` 只接受 `los_` Token，且只能写入它所属的项目。项目 API Token（`lo_`）与用户 API Token（`lou_`）返回 `403`，`code` 为 `source_map_upload_token_required`；缺失、未知或已吊销的 Token 返回 `401`。
- `los_` Token 用于其他任何路由（摄取、查询、`/mcp`、管理 API）一律返回 `403`，`code` 为 `upload_only_token`。
- 吊销后该 Token 在本副本立即失效，其他副本最迟在 30 秒认证缓存过期后失效（与项目 API Token 相同）。

MCP（`lazy-observe mcp --dsn`）的 DSN 形状与上面相同，但 `<token>` 必须是 `lou_...` 前缀的用户 API Token（在 Web UI“设置 → 个人 API Token”创建）；`<project-slug>` 通过 `X-LO-Project` 请求头发送。`lazy-observe mcp` 在启动时拒绝非 `lou_` 的 Token，不会等到第一次工具调用才收到 `403`。

#### 统一摄入限速与背压契约

网关支持通过 `ingest_rate_limit` 配置按项目（`project.ID`）执行统一的令牌桶限速（覆盖全部 HTTP 摄入端点与原生 OTLP/gRPC 服务，默认关闭，开启后按 `requests_per_second`、`burst` 及可选 `bytes_per_second` 约束，并在 `max_tracked_projects` 内有界回收）：

- **HTTP 摄入路由**：超限时在转发至上游存储前拒绝，返回 `429 Too Many Requests`、`Retry-After: <正整数秒>`（下限 `1` 秒、上限 `300` 秒）与脱敏 JSON 响应体 `{"code":"ingest_rate_limited","error":"project ingest rate limit exceeded"}`。
- **OTLP/gRPC 摄入服务**：复用同一限流器实例，超限时返回 gRPC `codes.ResourceExhausted` 并附带 `google.rpc.RetryInfo`（`retry_delay` 为正整数秒）。
- 不同项目的令牌桶严格隔离，单个项目触发限速不影响其他项目。

#### 自身 `/metrics` 端点鉴权与低基数契约

`GET /metrics` 通过 `metrics_endpoint.enabled` 控制（默认 `false`，关闭时返回 `404` 且不落入前端 SPA 回退）。启用时按 fail-closed 策略鉴权：

- 配置了 `metrics_endpoint.bearer_token`：只接受匹配的 `Authorization: Bearer <token>`（常数时间校验），且禁止与 `allow_unauthenticated: true` 同时配置；其余请求返回 `401`。
- 未配置 `bearer_token` 且 `allow_unauthenticated: false`（启用后的默认策略）：要求已认证的 `admin` 会话或 `admin` 用户 API Token（`lou_...`）；匿名返回 `401`，项目 API Token 返回 `403` `ingest_only_token`，上传 Token 返回 `403` `upload_only_token`，非管理员角色返回 `403`。
- 未配置 `bearer_token` 且显式设置 `allow_unauthenticated: true`：允许无鉴权抓取（适用于内网或 Pod 采集场景）。
- 指标标签仅使用有限静态枚举（如 `surface`、`status_class`、`protocol`、`signal`、`outcome`），绝不暴露 `project_id`、`trace_id`、动态 URL 或凭据文本。

### 2.3 协议版本变更规则

- 向后不兼容的变更（端点路径或语义变更、请求或响应字段删除或语义变化、鉴权方式变化、DSN 格式变化）视为升级服务端协议版本（例如 v0 升级至 v1）。
- 每一次协议版本升级必须先更新本矩阵文档（协议面清单、矩阵表、受影响 SDK 版本），随后才能发版。
- 新增端点或新增可选响应字段视为向后兼容，在协议面清单中追加说明，不提升协议版本。
- 新增请求侧枚举值属向后兼容：服务端跳过未知值并计数，同批的已知值照常写入，不整批拒绝。`POST /api/events/vitals` 的指标名与 `POST /api/events/diagnostics` 的 `signal` / `reason` 都已实现这一点，响应用 `accepted` / `rejected` 计数。结构性错误（缺字段、空 `signal` / `reason`、非法时间戳、计数非正、`service` 超长等）仍整批返回 `422`，不做部分写入。

## 3. 兼容矩阵

| 服务端版本 | Go SDK | Node SDK | Browser SDK | Python SDK | 兼容状态 |
| --- | --- | --- | --- | --- | --- |
| `dev`（pre-release，规划首个发布 v0.1.0） | 0.1.0 | 0.1.0 | 0.1.0 | 0.1.0 | 已验证 |

说明：

- 当前矩阵只有一行：服务端 pre-release 基线（`main.Version=dev`，规划首个发布 `v0.1.0`）与四个 SDK 包（均位于本仓库 `sdk/{go,node,browser,python}`，当前版本全部 0.1.0）。
- “已验证”代表该组合已在同一仓库提交上通过服务端构建、e2e 与对应 SDK 的测试（验证步骤见第 4 节）。
- 本矩阵以本文档为准，随每次服务端或 SDK 发版持续更新；矩阵中未列出的组合（例如某个 SDK 发版后、服务端尚未重验证的中间态）在验证完成前一律视为未承诺。

### 3.1 SDK 运行时要求

SDK 运行时下限属于对外兼容面，变更需与 SDK 代码在同一个 PR 里修改本节。

| SDK | 运行时要求 | 依据 |
| --- | --- | --- |
| Node SDK（`@lazy-observe/node`） | Node.js `>=22.15`（`package.json` 的 `engines.node`） | OpenTelemetry JS 3.x / OTLP exporter 0.301.x 的 `engines.node` 均为 `>=22.15.0`；仓库开发与 CI 工具链为 Node 26.13.1 |

## 4. 维护规则（谁在什么时候改什么）

### 4.1 服务端发版

服务端 release 流程（`.github/workflows/release.yml`，`v*` tag 触发）构建的是同一提交上 `sdk/go` 的源码：主模块通过 `go.mod` 的 `replace github.com/eimsound/lazy-observe/sdk/go => ./sdk/go` 引用 Go SDK，不再需要单独固定 SDK 版本。

1. 发布前该提交的 CI（含 e2e）必须为绿。
2. 发布版本号确定后，更新本矩阵表中对应服务端版本行（新增一行并标注同期 SDK 版本），先更新矩阵，再发布。

### 4.2 SDK 发版

SDK 以带前缀的 tag 从本仓库发布：Go 用 `sdk/go/vX.Y.Z`（Go 嵌套模块约定），Node / Browser / Python 分别用 `sdk-node-vX.Y.Z`、`sdk-browser-vX.Y.Z`、`sdk-python-vX.Y.Z`。

1. 打 tag 的提交上，服务端构建、e2e 与该 SDK 的测试必须为绿（同一次 CI 覆盖）。
2. 更新本矩阵表中对应 SDK 列。
3. 再打 tag 并对外发布。

### 4.3 协议变更

协议面（端点、字段、DSN、鉴权）发生任何变化，必须先更新本矩阵文档（第 2 节协议面清单与第 3 节矩阵），再发版；如属向后不兼容变更，同步升级协议版本号。

## 5. SDK 降级守则

本守则与 `sdk/AGENTS.md` 的“版本兼容与降级守则”章节保持一致。

当 SDK 向服务端上报数据、而服务端出现以下任一情况时：

- 私有 ingest 端点返回 404 / 4xx（端点不存在、被移除、鉴权被拒等）；
- 响应中出现未知字段（SDK 不认识的字段）；
- 请求中出现服务端无法识别的字段或枚举值（向 SDK 回吐错误）；

SDK 必须遵守以下四条：

1. **静默降级**：丢弃该次负载；对可重试场景（例如回放分段、profile 分片）可缓存后退避重试；
2. **diagnostics 上报**：把降级原因（HTTP 状态码、端点、SDK 版本）写入自身 diagnostics 缓冲，随下一次 diagnostics 上报一并发出（若 diagnostics 端点同样不可用，则只保留进程内计数，绝不阻塞）；
3. **绝不抛错**：降级不得向应用代码抛出异常、不得中断业务线程、不得改变上层业务行为；
4. **向后兼容新服务端**：SDK 必须忽略响应中一切未知字段（向前兼容），仅解析自己认识的结构。

本守则只针对应用运行时里的 SDK。`POST /api/releases/:release/sourcemaps` 是私有上传端点（§2.1），调用方是构建期工具（bundler-plugin 的 Vite 插件与 `lazy-observe-sourcemaps` CLI），不适用静默降级：配置了 DSN 而上传失败（含 4xx）时 Vite 构建失败、CLI 非零退出；未配置 DSN 时警告并跳过。

服务端侧的现状：`POST /api/events/vitals`（指标名）与 `POST /api/events/diagnostics`（`signal` / `reason`）都实现了对未知枚举值的宽容。它们遇到本服务端不认识的值时不整批拒绝，而是跳过该条、在响应的 `rejected` 字段里计数，并写入同批的已知值（`accepted` 计数）；实现了部分接受的服务端不会因为某个新枚举值就连带丢掉同一批里的已知值。

**新 SDK 配旧服务端的代价**：browser SDK 的 `vitals` signal 与 `server_rejected` reason 是本轮新增的枚举值。尚未实现部分接受的旧服务端会把含这两个值的 diagnostics 批次整批返回 `422`，后果不止是丢新增的那几条，同批里服务端本来认识的诊断（例如 `error` signal / `queue_full` reason）也会一并被拒。SDK 侧的处理是：该批留在 `diagnostics` 队列里按有限重试预算（`maxAttempts`，指数退避）反复重投，预算耗尽后整批丢弃；丢弃时 `droppedTelemetry` 按该批条数累加，并记一条 signal 为 `diagnostic`、reason 为 `retry_exhausted` 的本地丢弃（该丢弃不再递归上报 diagnostics），全程只输出 `console.warn`，不抛错、不影响业务。在新服务端上则只会跳过未知值并计数，同批已知诊断照常写入，`rejected > 0` 不是降级原因。

无论哪个端点，结构性错误（缺 `page`、空指标名、非法时间戳、空 `signal` / `reason`、`service` 超长、`dropped` 非正）都按整批 `422` 处理，不做部分写入。

## 6. GA 前路线图

进入 GA 前，将“声明式矩阵”升级为运行时强校验：

1. **协议版本头**：服务端与 SDK 上线 `X-LO-Protocol-Version` 请求头，SDK 上报其实现的协议版本，服务端据此决定是否继续接受该 SDK 的数据（未知或不兼容版本走降级守则，不拒绝服务、不改写业务）。
2. **diagnostics 版本上报**：SDK diagnostics 负载携带 runtime 与 SDK 版本信息，服务端侧可观测各版本 SDK 的分布与降级率。
3. **矩阵 CI 强校验**：发布 CI 增加脚本检查“矩阵中的 SDK 版本与实际发布版本一致”，把矩阵一致性升级为自动化门禁（当前为文档约定）。可先在服务端 CI 加一条只读的“矩阵一致性预演”检查任务验证可行性。
4. 强校验上线并运行平稳后，再评估是否移除声明式矩阵，或仅保留其作为历史版本参考表。
