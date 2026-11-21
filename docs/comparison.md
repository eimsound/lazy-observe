# 开源自托管可观测性平台对比：lazy-observe vs SigNoz / OpenObserve / Sentry / Traceway

本文面向正在评估开源自托管可观测性平台的工程与运维团队，将 **lazy-observe** 与 **SigNoz**、**OpenObserve**、**Sentry（Self-Hosted）** 和 **Traceway** 从架构取舍、部署复杂度、生态兼容、功能覆盖与适用边界五个维度做客观对照，帮助团队快速判断 lazy-observe 是否匹配自身场景。

> **说明与口径**
>
> - 本文只对比架构事实与工程取舍，不提供脱离硬件与负载条件的性能跑分，也不对竞品做贬低式评价。
> - lazy-observe 的所有能力声明均以本仓库当前主分支实现为准，并附对应文档与 ADR 链接；当前项目处于 **v0.x 公开预览期**，具体兼容面以 [版本兼容矩阵](version-matrix.md) 与 [CHANGELOG.md](../CHANGELOG.md) 为准。
> - 竞品的架构细节、功能范围与许可证可能随上游版本演进变化，最终选型前请**以各自上游仓库与官方文档为准**。

---

## 1. lazy-observe 的定位与核心差异点

lazy-observe 的目标不是成为超大规模多租户 SaaS 或替代 Grafana 的全能大屏系统，而是为中小规模及中大型自托管团队提供一套**单二进制起步、可平滑演进至高可用集群、与 Prometheus / Victoria 生态原生融合、并覆盖四信号与前后端应用诊断**的开源平台。以下五点是当前实现中经源码与发布门禁验证的核心差异：

### 1.1 单二进制免 CGO（`CGO_ENABLED=0`）与零外部依赖 All-in-One 启动

- **极简起步**：发布产物为纯 Go 静态编译的单二进制文件（`CGO_ENABLED=0`，支持 Linux `amd64` / `arm64`），内嵌编译后的 Web UI、SHA-256 校验清单以及四个固定版本的 Victoria 系可执行文件（`victoria-metrics-prod`、`victoria-logs-prod`、`victoria-traces-prod`、`vmagent-prod`）。元数据默认使用纯 Go SQLite 驱动（`modernc.org/sqlite`，WAL 模式），本地 Blob 存储落盘于 `<data_dir>/blobs/`。
- **单端口与进程托管**：试用或中小规模部署时，**不需要先拉起 ClickHouse + Kafka + ZooKeeper + PostgreSQL + Redis 多容器栈**；默认仅对外暴露 `:3100`（可选通过 `otlp_grpc.enabled: true` 开启 `:4317` 原生 OTLP/gRPC 摄入端口），受管子进程只监听本机回环地址，具备独立指数退避重启与两阶段优雅摘流/关停。
- **平滑切换到 HA 集群**：同一份二进制与同一套 `storage.SignalBackend` 抽象同时支持 **Standalone** 与 **Clustered-HA** 形态——当单机容量不足或需要多副本高可用时，可接入外部 VictoriaMetrics 集群（`vmselect` / `vminsert`）、分拆角色的 VictoriaLogs / VictoriaTraces、外部 PostgreSQL、Redis/Valkey（仅作共享会话与限流）及 S3 兼容对象存储。仓库内置 Helm Chart（含三副本集群配置）、Docker Compose 与 systemd 参考清单，部署与运维细节见 [部署文档](deployment.md) 与 [运维手册](operations.md)。

### 1.2 原生 Prometheus / MetricsQL 生态兼容，看板交还 Grafana / Perses

- **Victoria 专精存储（[ADR 0013](../.agents/docs/adr/0013-signal-storage-is-victoria-only.md)）**：指标、日志与链路存储层固定为 VictoriaMetrics / VictoriaLogs / VictoriaTraces，减少跨存储适配的语义折损与维护负担。
- **Prometheus 协议面兼容**：指标写入支持 OTLP/HTTP（`/v1/metrics`）、可选原生 OTLP/gRPC（默认关闭 `otlp_grpc.enabled: false`，开启后监听 `:4317`）、Prometheus remote-write 1.0（`/api/v1/write`）及内置 `vmagent` 主动抓取 Prometheus exporter；指标查询原生支持 PromQL / MetricsQL，网关直接暴露标准 Prometheus 查询端点（`/api/v1/query`、`/api/v1/query_range`、`/api/v1/series`、`/api/v1/labels`、`/api/v1/label/:name/values`）。
- **克制的产品边界（[ADR 0017](../.agents/docs/adr/0017-no-built-in-dashboards.md)）**：lazy-observe 内置固定诊断工作区（Overview、Application Performance、主机列表与详情、LLM 用量与成本、Metrics/Logs/Traces 查询工作区、Continuous Profiling），**刻意不自建可拖拽 Dashboard 编辑器，也不维护封闭的中间件模板库**。需要自定义监控大盘或复用存量 Grafana 看板时，可直接把 lazy-observe 接入 **Grafana** 或 **Perses**（指标端点开箱直连，日志与链路经轻量路径重写代理透传鉴权头，详见 [Grafana 与 Perses 对接指南](grafana-perses.md)）。

### 1.3 存储层硬性项目隔离与最小权限 Token 边界

- **网关侧强制隔离**：底层存储端口不对外暴露，所有读写必须经由网关校验并重写租户边界：
  - **VictoriaMetrics**：对 OTLP、Web Vitals 与 Prometheus remote-write 1.0（解包重写 protobuf 标签）强制覆盖 `project_id` 标签；查询侧剥离客户端传入的 `extra_filters` / `extra_label` 并注入可信过滤条件。
  - **VictoriaLogs / VictoriaTraces**：读写请求统一移除并覆盖上游 `AccountID` / `ProjectID` 原生租户头。
- **三类凭据严格分权（[ADR 0010](../.agents/docs/adr/0010-project-api-tokens-are-ingest-only.md)、[ADR 0018](../.agents/docs/adr/0018-source-map-upload-token.md)）**：
  - **项目 API Token（`lo_`）**：随 SDK DSN 下发（包含公开的浏览器页面），**只能用于遥测写入**；携带 `lo_` 访问任何读路由（含 `/mcp`、`/api/system/info`）一律返回 `403 ingest_only_token`。
  - **用户 API Token（`lou_`）**：绑定用户当前 RBAC 角色（`admin` / `member` / `viewer`），配合 `X-LO-Project` 请求头供 Grafana / Perses / MCP / CLI 使用，不能用于遥测写入。
  - **Source Map 上传 Token（`los_`）**：物理分表存储的项目级保密凭据，仅放进 CI Secret 用于 `POST /api/releases/:release/sourcemaps`；既避免公开页面里的 `lo_` Token 被用来篡改已发布版本的 Source Map，也避免将个人 `lou_` Token 绑定到 CI 流水线。用于任何其他路由返回 `403 upload_only_token`。

### 1.4 单主租约告警引擎、持久化 Outbox 与开箱即用国内 IM 渠道

- **内置原生评估器（[ADR 0001](../.agents/docs/adr/0001-nightingale-style-alert-engine.md)、[ADR 0011](../.agents/docs/adr/0011-sqlite-alert-lease-millisecond-clock.md)）**：不要求部署或维护 `vmalert` 与 Alertmanager。引擎在网关进程内统一评估 PromQL/MetricsQL 规则、LogsQL 阈值规则以及新 Issue / 回归等平台事件。多副本 HA 下，通过 Rendezvous 一致性哈希与带单调递增隔离纪元（fencing epoch）的 PostgreSQL 规则租约，保证每条规则同一时间最多只有一个合法提交者；租约过期判定统一基于数据库时钟（SQLite 下经毫秒时钟入口修正整秒截断）。
- **持久化通知 Outbox**：状态推进与通知入队在同一数据库事务内原子提交。分发 Worker 通过数据库行锁（PostgreSQL `FOR UPDATE SKIP LOCKED`）领取到期任务，支持过期 claim 接管、有界指数退避重试、死信保留及 HTTP `Idempotency-Key`，提供持久化至少一次投递（at-least-once）；即便 Redis 故障或副本重启，告警状态与未发送通知仍保持正确。
- **降噪与国内 IM 原生支持**：完整支持分组等待（`group_wait`）、重复间隔（`repeat_interval`）、风暴阈值汇总（`storm_threshold`）、静默（Silence）、抑制规则（Inhibition）与渠道限流。通知渠道开箱内置 **飞书（Feishu/Lark，交互卡片）**、**钉钉（DingTalk，Markdown）**、**企业微信（WeCom，Markdown）**、**Slack（Blocks）**、**Webhook（JSON + 自定义请求头 + `Idempotency-Key`）** 与 **SMTP / Email（STARTTLS / 隐式 TLS）**，并对飞书/钉钉/企业微信在 HTTP 200 下返回的业务错误码（`code` / `StatusCode` / `errcode`）做显式校验。

### 1.5 统一四信号 + Error Tracking / Session Replay / Continuous Profiling / 四端 SDK

- **信号与诊断一体化**：除 OTLP/HTTP（`/v1/metrics`、`/v1/logs`、`/v1/traces`）、可选原生 OTLP/gRPC（默认关闭 `otlp_grpc.enabled: false`，开启后监听 `:4317`，原生覆盖 Traces / Metrics / Logs）与 Prometheus remote-write 1.0 外，内置：
  - **Error Tracking**：基于持久化元数据 Inbox 接收错误事件（已接管事件防背压驱逐、重启可恢复，Inbox 满载时显式返回 `503`），按堆栈指纹归并为 Issue、识别回归，并通过 Source Map 还原前端报错源码位置；
  - **Browser RUM & Session Replay**：采集 LCP / CLS / INP / TTFB / FCP 五项 Web Vitals、关联 `fetch`/`XHR` 后端链路，并提供隐私优先的 DOM 会话回放（默认遮蔽所有输入值、支持 `[data-lo-block]` 子树屏蔽）；
  - **Continuous Profiling**：pprof 火焰图汇聚，覆盖 **Go（CPU / heap / goroutine）**、**Node.js（V8 CPU / sampled-heap）**，以及 **Python（experimental，仅在 Linux 无 GIL 的 free-threaded CPython ≥ 3.15t 上支持进程内 CPU 采样，详见 [ADR 0014](../.agents/docs/adr/0014-python-profiling-on-cpython-315-sampling.md)）**；
  - **GenAI 统计（默认常开）与只读 MCP**：基于 VictoriaTraces 中的 `gen_ai.*` Span 与内置模型定价表，默认常开统计业务应用的 LLM 调用、Token 用量与估算费用，无需开启 `Settings > AI` 或配置外部 LLM API Key；`/mcp` 与 `lazy-observe mcp` 提供项目隔离的只读 MCP 工具；
  - **可选平台 AI 助手（默认关闭）**：管理员可在 **Settings > AI** 按需配置 BYO LLM（支持 OpenAI、Anthropic、DeepSeek 与原生 Ollama），仅控制平台侧自然语言生成/解释 MetricsQL 与 LogsQL 以及新 Issue 异步跨信号分析。
- **四端显式接入 SDK**：同仓维护 [Go SDK](sdk/go.md)（Fiber v3、`slog`、`database/sql`）、[Node SDK](sdk/node.md)（Express、Pino、Winston）、[Browser / React SDK](sdk/browser.md)（含 `@lazy-observe/bundler-plugin` Vite 插件与 CLI）与 [Python SDK](sdk/python.md)（Django、DRF、`logging`、DB 与 LLM 埋点），不侵入全局 Monkey-Patch，并遵循统一的降级守则与 [版本兼容矩阵](version-matrix.md)。

---

## 2. 平台横向对比矩阵

> 下表概括各项目在自托管场景下的典型形态；各竞品具体版本与功能边界**以各自上游仓库为准**。

| 对比维度 | lazy-observe | SigNoz | OpenObserve | Sentry（Self-Hosted） | Traceway |
| --- | --- | --- | --- | --- | --- |
| **架构与存储引擎** | Go（`CGO_ENABLED=0`）单网关 + VictoriaMetrics / VictoriaLogs / VictoriaTraces / vmagent；元数据 SQLite / PostgreSQL；Blob 本地 / S3；可选 Redis/Valkey | Go Query Service + 定制 OTel Collector + ClickHouse + SQLite/PostgreSQL 元数据 | Rust 单体或分角色集群（Router/Ingester/Compactor/Querier）；对象存储优先（S3/本地 Parquet 等）+ SQLite（单机）/ PostgreSQL + NATS（集群） | Python / Rust（Relay）+ Snuba + ClickHouse + Apache Kafka + ZooKeeper + PostgreSQL + Redis + Symbolicator 等多服务流水线 | Go 后端 + ClickHouse（生产遥测存储）+ PostgreSQL（元数据）；提供面向本地/小规模的 SQLite 轻量模式 |
| **最小部署形态** | 单二进制零外部依赖 All-in-One（默认 SQLite + 嵌入四组件），默认仅暴露 `:3100`（可选开启 `:4317` 原生 OTLP/gRPC）；可平滑切换到外部 Victoria 集群 HA | 通常以 Docker Compose 或 Helm 运行多容器栈（ClickHouse + Keeper/ZooKeeper + OTel Collector + SigNoz） | 单二进制 + 本地磁盘/SQLite 可快速启动；HA 集群拆分为多角色并需 PostgreSQL + NATS + S3 | 官方自托管为重型 Docker Compose 多容器集群，对内存与运维资源要求较高 | 轻量模式可用 SQLite 快速体验；生产形态通常搭配 ClickHouse + PostgreSQL |
| **查询语言与 Grafana / Perses 生态兼容** | 指标原生 PromQL / MetricsQL（直连 Grafana / Perses Prometheus 数据源）；日志 LogsQL；链路 Jaeger API（经轻量代理可对接，见 [对接指南](grafana-perses.md)）；不自建拖拽大屏（[ADR 0017](../.agents/docs/adr/0017-no-built-in-dashboards.md)） | ClickHouse Query Builder、PromQL、ClickHouse SQL；内置可编辑 Dashboard，定位为一体化替代 UI，非官方 Grafana 数据源插件 | SQL 与 PromQL；内置完整 Dashboard 编辑器与报表能力 | 自有 Discover / SnQL 查询语法与内置视图，不面向 Prometheus / Grafana 数据源生态 | OTLP/HTTP 原生接入，内置自有查询与看板组件；并非围绕 PromQL / Grafana 数据源生态构建 |
| **Error Tracking / Replay / Profiling** | 异常指纹聚合 + Source Map + 回归检测；Web Vitals + 隐私优先 DOM Session Replay；Go / Node / Python（3.15t experimental）pprof 火焰图；内置 LLM 观测与只读 MCP | 基于 OTel Span 的异常追踪与聚合；偏后端 APM，不以原生 DOM Session Replay 与内置火焰图为主线（以各自上游仓库为准） | 覆盖 Logs/Metrics/Traces、RUM（含 Session Replay）与 OTLP Continuous Profiling；非 Sentry 式独立 Error Workflow 主线（以各自上游仓库为准） | 行业标杆级 Error Tracking、多端原生 SDK（含移动端）、Session Replay、Profiling、Cron 监控与 Release 健康度 | 一体化覆盖三信号、异常聚合与 Source Map、RUM/Session Replay（含 Web 与 Flutter）、LLM 观测与实验性 Go pprof 火焰图 |
| **告警降噪与国内 IM（飞书 / 钉钉 / 企微）** | 内置单主租约 + Fencing + 持久化 Outbox；支持分组等待、静默、抑制、风暴汇总、限流；**原生内置飞书、钉钉、企业微信**、Slack、Webhook、SMTP/Email | 基于定制 Alertmanager，支持 Slack、PagerDuty、OpsGenie、Teams、Email、Webhook；飞书/钉钉/企微通常走通用 Webhook | 支持 Slack、Email 与自定义 Webhook 模板；飞书/钉钉/企微通常经自定义 Webhook 模板配置 | 丰富的 Issue/Metric 告警与海外协作集成（Slack/PagerDuty/Jira/Webhook 等）；飞书/钉钉/企微通常需 Webhook 或第三方集成 | 内置 Slack、Email、Webhook、GitHub、Telegram、Pushover 等；飞书/钉钉/企微通常走通用 Webhook |
| **项目 / 租户隔离与凭据边界** | 单团队多项目数据分区；网关强制重写 `project_id` 及 `AccountID`/`ProjectID`；`lo_`（仅摄取）/ `lou_`（用户读写）/ `los_`（仅上传 Source Map）三类分权 Token | 支持多用户与 RBAC，部分 SSO/高级企业治理归入 Enterprise（`ee/`）范围（以各自上游仓库为准） | 原生支持多 Organization 与流级隔离，企业版提供更细粒度 RBAC/SSO（以各自上游仓库为准） | 成熟的多组织、多团队、多项目与细粒度权限模型 | 支持组织/项目级隔离（具体模型以各自上游仓库为准） |
| **许可证** | **MIT**（内嵌 Victoria 系组件为 Apache-2.0；生产与运行时不含 AGPL/GPL 依赖，见 [LICENSE](../LICENSE)） | 核心 **MIT** + `ee/` 企业版专有许可（Open-Core，以各自上游仓库为准） | **AGPL-3.0**（另有企业版条款，以各自上游仓库为准） | **FSL-1.1-Apache-2.0**（源码可用，限制商业托管竞争，满 2 年转 Apache-2.0；SDK 多为 MIT/Apache-2.0，以各自上游仓库为准） | **MIT**（无 BSL/Open-Core 限制，以各自上游仓库为准） |

### 典型场景速览

- **更适合选择 lazy-observe**：希望用最小运维开销自托管统一可观测性（单二进制起步、后期平滑切 Victoria 集群 + PG + S3）；已有 Prometheus / Grafana / Perses 习惯；看重 MIT 宽松许可、开箱即用的飞书/钉钉/企业微信告警以及严格分权的摄取/查询/Source Map Token 边界。
- **更适合选择 SigNoz**：团队已全面采用 OpenTelemetry，希望一套内置可编辑大屏、以 ClickHouse 为核心的后端 APM 与三信号分析平台。
- **更适合选择 OpenObserve**：日志与指标体量很大、希望直接以 S3 对象存储 + Parquet 极致压低长期存储成本，且团队接受 AGPL-3.0 许可证与自带全套大屏的形态。
- **更适合选择 Sentry（Self-Hosted）**：核心诉求是最成熟的 Error Tracking、移动端原生 SDK（iOS/Android/Flutter 等）、Release 健康度与 Cron 监控，且团队具备维护多容器重型自托管栈的资源。
- **更适合选择 Traceway**：偏好 MIT 许可与 Go + ClickHouse 架构，且需要内置自有大屏或 Flutter 移动端会话回放等开箱特性。

---

## 3. 何时不要选 lazy-observe

开源选型中最容易踩坑的是忽视产品的显式边界。如果您的团队存在以下任一硬性诉求，**不建议现阶段选择 lazy-observe**：

1. **需要成熟的多租户商业 SaaS，或面向超大规模 PB 级宽表的 ClickHouse OLAP 即席分析**
   - lazy-observe 的定位是**单团队自托管系统**（以项目作为数据分区，而非面向外部租户的商业 SaaS 控制面；无关联组织之间的强隔离建议独立部署实例）。
   - 如果业务核心依赖超大规模列式 OLAP 聚合或 ClickHouse 自定义 SQL 报表分析，基于 ClickHouse 或对象存储数据湖的方案（如 SigNoz、OpenObserve）会更契合。

2. **必须在平台内直接拖拽编辑大盘，而不愿接入 Grafana 或 Perses（[ADR 0017](../.agents/docs/adr/0017-no-built-in-dashboards.md)）**
   - lazy-observe 仅保留固定的内置诊断页面，**明确不提供可编辑 Dashboard 引擎，也不内置主机/中间件 Dashboard 模板**。
   - 如果团队希望“零外部工具、所有自定义大盘都在一个 UI 里拖拽完成”，建议直接选用内置完整 Dashboard 系统的产品，或在评估期同步规划好 Grafana / Perses 对接。

3. **强依赖移动端原生 SDK（iOS / Android / Flutter）或 Cron 定时任务监控**
   - 当前仓库仅提供 **Go（Fiber v3）、Node.js（Express）、Browser/React、Python（Django/DRF）** 四端 SDK，尚未提供 iOS / Android / Flutter 原生 SDK，也未内置专门的 Cron / Heartbeat 监控能力。
   - 此外，Python 持续剖析目前为 **experimental**，受限于 CPython GIL 采样盲区，仅在 Linux 无 GIL 的 free-threaded CPython ≥ 3.15t 上生效（见 [ADR 0014](../.agents/docs/adr/0014-python-profiling-on-cpython-315-sampling.md)）；原生 OTLP/gRPC（`:4317`）默认关闭（`otlp_grpc.enabled: false`）需显式开启，且仅覆盖 Traces / Metrics / Logs 三信号（Error / Vitals / Replay / Profiles 等平台私有上报仍走 HTTP）。

4. **要求 GA 级跨大版本平滑数据库升级保障或商业 SLA（[ADR 0003](../.agents/docs/adr/0003-pre-release-single-database-baseline.md)）**
   - lazy-observe 当前处于 **v0.x 公开预览期**（首个受支持版本尚未发布）：SQLite 与 PostgreSQL 各自仅维护一份可变的 `001_baseline.sql` 元数据基线，**不承诺跨不兼容 schema 变更的原地升级或降级迁移**（升级可能要求重建元数据数据库，见 [运维手册](operations.md)），协议面与 SDK 也可能发生不兼容调整（见 [版本兼容矩阵](version-matrix.md)）。
   - 同时，本项目为 MIT 社区开源项目，不提供商业 SLA 或 7×24 商业支持合同。

5. **必须复用非 Victoria 系的存量信号存储（如现有 Loki / Tempo / Jaeger / Mimir / Thanos，[ADR 0013](../.agents/docs/adr/0013-signal-storage-is-victoria-only.md)）**
   - lazy-observe 的指标、日志与链路存储层**按设计固定为 Victoria 系**（VictoriaMetrics、VictoriaLogs、VictoriaTraces），Loki/LogQL 与 Tempo/TraceQL 适配器明确不在路线图范围内；`storage.metrics.kind: prometheus` 虽在代码中保留，但不属于官方支持与发布验证范围。
   - 这也意味着链路存储的上限与演进节奏与上游 VictoriaTraces（目前仍处 v0.x 阶段）强绑定，无法通过切换至 Tempo 或 Jaeger 后端来规避。

---

## 相关文档

- [部署文档（All-in-One、Standalone 与 Helm HA）](deployment.md)
- [运维手册（备份、恢复、升级、回滚与应急操作）](operations.md)
- [Grafana 与 Perses 对接指南](grafana-perses.md)
- [服务端与客户端 SDK 版本兼容矩阵](version-matrix.md)
- [变更日志](../CHANGELOG.md)
