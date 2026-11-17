# lazy-observe 需求细化（v1.0）

> 状态：**全部开放问题已确认**（第 9 节保留决策记录）。本文档为 v1 需求基线；后续变更按增量修订。

## 0. 存储引擎选型

存储层采用 **VictoriaMetrics 生态**：

- **VictoriaMetrics**（指标存储，Apache-2.0）
- **VictoriaLogs**（日志存储，Apache-2.0）
- **VictoriaTraces**（链路存储，Apache-2.0，Jaeger 查询兼容）

选型理由：三个组件全部是 Go 编写、单二进制风格、License 允许闭源/商用打包（对比 Grafana Loki/Tempo/Mimir 的 AGPL），并且三者均原生支持 OTLP 摄入。

## 1. 项目定位与目标

一句话：**自托管的一体化可观测性平台，一个二进制起全套，SDK 开箱即用。**

项目所有者确认的三个目标（按此顺序理解全文一切取舍）：

1. **功能覆盖**：尽量覆盖夜莺（Nightingale）、Prometheus 系、VictoriaMetrics 系、Sentry 的功能面。
2. **易用性（核心差异化）**：客户端尽量开箱即用，把用户学习成本降到最低。
3. **性能**：优于 Sentry（可测基准见 1.2）。

注意"覆盖"不等于全部重造：Prometheus 生态靠**兼容**覆盖，VM 靠**内嵌**覆盖，自研火力集中在产品层（SDK、UI、关联、Issue、Replay、AI）。

### 1.1 功能覆盖矩阵

| 对手 | 其核心功能面 | 我们的覆盖方式 | 有意延后 / 不覆盖 |
|---|---|---|---|
| **夜莺 n9e** | 告警引擎、聚合降噪、主机监控、看图 | 自研：告警 + 聚合降噪（M1/M3）、主机监控（M3，基于 exporter 抓取 + 产品层 UI，见 3.13）、dashboard | 值班排班（on-call）、异构数据源单一面板（v2 插件） |
| **Prometheus 系** | metrics 存储、PromQL、exporter 生态、Alertmanager | 兼容：内嵌 VM 即 Prometheus 兼容存储；PromQL 查询、remote write、exporter 抓取全兼容；告警采用 Alertmanager 语义模型 | 不自造 exporter 生态——直接吃现有生态 |
| **VM 系** | metrics/logs/traces 高性能存储 | 内嵌：VM 三件套就是我们的存储引擎，不重造 | 存储性能不做超越目标（引擎即上限）；超大规模场景由 HA 模式对接 VM 集群版承接（M5，见 5.3） |
| **Sentry** | 错误追踪/Issue、Source Map、Replay、Performance、告警、Profiling | 自研：全部核心面（M2 重点），错误聚合质量专项对标；**Profiling 为核心功能**（Go M3 / Node M4，见 3.14） | 移动端 SDK、Crons 监控（后续再议；Crons 过渡期提供"指标打点 + 告警规则"官方 recipe） |

### 1.2 性能目标：优于 Sentry（可测基准）

Sentry 自托管的结构性负担是我们的机会：Python worker + Kafka + ClickHouse + Redis + 20 多个容器，官方最低配置要求 16GB 内存级别。"性能更好"落为四条可验证基准：

1. **部署资源**：空载内存 < 500MB、单二进制、单机可跑——对 Sentry 自托管是数量级优势。
2. **摄入吞吐**：单核 events/s 高于 Sentry（Go 网关直写 VM vs Python + Kafka 链路）；M2 建立公开 benchmark suite 给出实测数字。
3. **查询响应**：中等数据量下 Issue 列表 / 日志检索 / trace 查询 P95 < 1s。
4. **SDK 开销**：middleware 每请求额外开销微秒级；显式插桩无全局 hook 的运行时负担，开销可预测。

承诺方式：公开对比 benchmark 文档 + 性能回归测试进 CI（防止"发布时快，三个版本后慢"）。

### 1.3 由目标推导出的硬性需求（新增/升级）

1. **告警升级为 v1 必选**：要覆盖夜莺的核心功能面，告警（含聚合降噪）不能是 roadmap 尾巴，必须是第一梯队能力。
2. **主机/基础设施监控进入范围**（原方案缺口）：夜莺和 Prometheus 系用户默认期望机器监控。方案：**不自研采集端**，完全基于社区 exporter 生态——server 端抓取 node_exporter 及任意 Prometheus exporter（内嵌 vmagent 承担 + 抓取目标管理 UI），配套内置主机/中间件 dashboard；categraf/Telegraf 等推送型采集器经 remote write 接收（1.3-3 既有能力）天然接入。详见 3.13。
3. **Prometheus 生态兼容是战略级要求**：remote write 接收、exporter scrape、PromQL 查询兼容、Grafana 数据源兼容（允许用户继续用 Grafana 看我们的数据——降低迁移阻力的单向门）。
4. **国内市场适配**：UI 中英双语（i18n 架构从 M0 开始，不是后补）；通知渠道飞书/钉钉/企微优先于 Slack；离线部署友好（无外网依赖）。
5. **错误聚合质量对标 Sentry**：堆栈指纹算法是 Sentry 的护城河之一，需专项设计（而非简单 hash），列为 M2 的重点技术风险。

### 1.4 差异化主张（总纲）

1. **部署侧**：K3s 式单二进制，不需要 Docker Compose 十个容器。
2. **接入侧**：SDK 是"产品"而不是"协议库"——每个框架一行 middleware 即得到完整默认指标集，行为显式可预测，拒绝运行时 hook。
3. **能力侧**：Metrics + Logs + Traces + Error/Stack + Session Replay + Performance + 主机监控 + AI 一站覆盖，共享同一套查询/关联体验（trace ↔ log ↔ error ↔ replay 互相跳转）。
4. **生态侧**：不另立山头——Prometheus/OTel 两大生态完全兼容，存量用户平移进来，而不是要求推倒重来。

## 2. 目标用户与场景

| 用户 | 场景 | 对应模式 |
|---|---|---|
| 中小团队 / 个人开发者 | 没有现成监控体系，想一条命令起全套 | All in One |
| 已有 Prometheus/VM 体系的团队 | 只想要统一 UI + SDK + Replay/错误追踪能力 | Standalone |
| 内网/私有化交付 | 不能用 SaaS，要求部署极简 | All in One |
| 大规模 / 平台工程团队 | 高可用要求、K8s 环境、流量超出单机承载 | HA 分布式（见 5.3） |

## 3. 功能需求

### 3.1 数据摄入（Ingest Gateway）

平台后端（Fiber）内置统一摄入网关，**对外只暴露一个端口**：

- **OTLP/HTTP + OTLP/gRPC**：metrics、logs、traces 的统一标准入口（转发/写入 VM 三件套）。
- **Prometheus 兼容**：remote write 接收；可选的 scrape 拉取（由内置 vmagent 能力承担）。
- **平台私有协议**：浏览器 SDK 的错误上报、Web Vitals、Session Replay 分片、Source Map 上传。
- 鉴权：per-project ingest token（`X-LO-Token` 或 OTLP header），支持吊销与轮换。
- 保护：请求体大小限制、限速、背压（存储不可用时的降级策略：丢弃 + 计数暴露）。

### 3.2 Metrics

- 存储：VictoriaMetrics（All in One 内嵌 / Standalone 外接，兼容任何 Prometheus remote-write 目标）。
- 查询：MetricsQL/PromQL 透传 + UI 查询构建器（面向不会写 PromQL 的用户）。
- SDK 默认指标集（零配置产出）：HTTP 请求速率/时延分布/状态码、Go runtime（GC、goroutine、内存）、DB 连接池、自定义指标 API（简单 counter/gauge/histogram 封装）。
- 内置仪表盘模板：每个 SDK 框架对应一套开箱即用 dashboard，不需要用户自建。

### 3.3 Logs

- 存储：VictoriaLogs；查询语言 LogsQL + UI 过滤器。
- 摄入：OTLP logs、SDK 直发（结构化 JSON）；日志文件采集不自研，用户用 vector/vlagent/OTel Collector 等社区采集器推到我们的 OTLP 入口（官方文档提供配方）。
- SDK 行为：提供 slog/zap/logrus handler（Go），一行接入；自动注入 trace_id/span_id 实现日志↔链路关联。
- UI：日志流、字段级过滤、直方图、从日志行一键跳到所属 trace。

### 3.4 Traces

- 存储：VictoriaTraces；摄入 OTLP。
- SDK：基于 OpenTelemetry SDK 封装，但**只启用显式插桩**——HTTP middleware、DB 驱动包装器（显式替换构造函数，非 patch）、outgoing HTTP client 包装器。
- UI：Trace 瀑布图、服务拓扑（v2）、按端点的时延分析、慢请求下钻。
- 采样：SDK 端头部采样（默认 100%，可配比率）；尾部采样 v2 再议。

### 3.5 错误追踪 / Stack（Sentry 对标能力）

- 捕获：服务端 panic/exception（middleware recover 捕获，非全局 hook）；浏览器 `window.onerror` / `unhandledrejection`。
- 聚合：按堆栈指纹分组为 Issue；计数、首次/最近出现、影响用户数。
- Source Map：浏览器错误支持 source map 上传与还原（构建插件：Vite/Webpack）。
- Issue 生命周期：open / resolved / ignored；回归自动重开。
- 关联：错误 → 所属 trace → 前端对应 session replay。

### 3.6 Session Replay

- 采集：浏览器 SDK 内嵌 rrweb（MIT），默认脱敏（input 打码、可配置 block 选择器）。
- 触发策略【默认已确认】：**采样 + 错误全录（Sentry 默认模式）**——正常会话按比率采样录制（默认采样率待定，如 10%），错误会话 100% 上传（环形缓冲，出错回溯上传）。全量录制 / 仅错误会话作为可配置选项保留。
- 存储：分片压缩后存对象存储或本地磁盘（All in One 默认本地磁盘 + 保留期清理）。
- 回放 UI：时间轴 + 同会话的错误/请求/Web Vitals 事件叠加标注。

### 3.7 Performance Monitoring

- 前端：Web Vitals（LCP/CLS/INP/TTFB/FCP）自动采集，按页面/地理/设备聚合；资源加载瀑布（v2）。
- 后端：从 traces 派生端点级性能视图（p50/p95/p99、吞吐、错误率），无需单独插桩。
- 前后端贯通：浏览器 fetch/XHR 注入 `traceparent`，一次用户操作串起前端 span 与后端 trace。

### 3.8 告警【已确认：v1 必选，基础能力提前至 M1】

- 基于 MetricsQL/LogsQL 的阈值告警（复用 vmalert 能力）+ 平台事件告警（新 Issue、错误率突增）。
- 通知渠道：飞书/钉钉/企业微信/Webhook/邮件优先，Slack 次之（国内市场优先，见 1.3-4）。
- 覆盖基线：告警能力（含 3.12 的聚合降噪）需覆盖夜莺核心功能面，是 v1 必选而非可选（见 1.3-1）。

### 3.9 平台管理

- 项目（Project）为隔离单元：token、数据、dashboard 按项目划分。
- 用户与权限：v1 单团队 + 角色（admin/member/viewer）；SSO v2。
- **多租户：明确不做（应用内不实现）**。租户隔离下沉到部署层——每租户独立实例（单二进制空载 <500MB + M5 Helm Chart，实例级隔离成本极低），隔离强度高于应用内方案且代码零负担。此为 Grafana Cloud（per-stack）验证过的路线，Grafana OSS 的应用内 Organizations 功能已被官方事实弃养。前提纪律：Project 隔离必须实现干净（所有数据访问强制走 project 维度），若未来公有 SaaS 场景出现，在其之上再评估控制面方案。
- 数据保留：按信号类型全局配置（如 metrics 90d / logs 14d / traces 7d / replay 3d），自动清理；不做按项目差异化（见 5.5）。

### 3.10 AI 集成 —— 平台侧 AI 能力（AI for Observability）

参考市面主流实践（Datadog Bits AI SRE、Sentry Seer、Grafana Assistant、Dynatrace Davis、Elastic AI Assistant），按投入产出排序分四层：

1. **MCP Server（低成本高价值，优先）**：将平台的查询能力（metrics/logs/traces/issues/replay 元数据）以 MCP 协议暴露，用户的 AI agent（Claude Code、Cursor 等）可直接查询和调查监控数据。2026 年已是可观测性平台标配（Datadog/Grafana/Sentry 均有官方 MCP Server）。
2. **AI Assistant / 自然语言查询**：自然语言 → MetricsQL/LogsQL/过滤条件；图表与异常的自然语言解释；面向不会写查询语言的用户，与 UI 查询构建器互补。
3. **Issue / 告警 AI 分析**：新 Issue 自动生成摘要与疑似根因（关联同 trace 的日志、指标突变、最近部署）；对标 Sentry Seer 的调查能力（PR 自动修复不在范围——平台不接管用户代码库）。
4. **异常检测与智能告警（后置）**：先做统计学基线（同比/环比/离群），ML 异常检测 v2 再议；agentic RCA（告警触发即自动跨信号调查，Datadog Bits AI SRE 模式）为长期方向。

**架构与原则（硬性约束）**：
- **BYO LLM**：用户自带 API key（OpenAI/Anthropic 兼容接口 + 本地 Ollama），平台不内置、不捆绑任何模型。
- **隐私默认**：自托管产品的底线——监控数据只有在用户显式配置外部 LLM 后才可能离开本机；所有 AI 功能默认关闭、可整体禁用。
- **只读旁路**：AI 层只坐在只读查询 API 之上，不进数据写入路径；AI 不可用不影响任何核心监控功能。

### 3.11 AI 应用可观测性（Observability for AI / LLM Monitoring）

监控"用户的 AI 应用"，对标 Langfuse/LangSmith，差异化在于与基础设施监控同平台（一个 trace 里同时看到 HTTP、DB 和 LLM 调用）：

- **遵循 OpenTelemetry GenAI 语义约定**（`gen_ai.*` 属性：模型、input/output tokens 等）。该约定 2026 年仍处 experimental 但已是事实标准（Datadog/New Relic/Dynatrace 均原生支持），跟随而非自造。
- **LLM 调用追踪**：作为 trace 中的 span 呈现，UI 提供 LLM 专属视图（对话内容、工具调用、agent 步骤）。
- **Token 用量与成本**：按项目/模型/端点聚合的用量与成本仪表盘，内置主流模型价格表（可覆盖配置）。
- **Prompt/Completion 采集**：默认不采集内容（只采集元数据与 token 数），显式开启后支持脱敏规则。
- **SDK 形态**：显式包装 OpenAI/Anthropic SDK 客户端（构造函数包装，符合 4.1 的 no-monkey-patch 原则）；已用 OTel GenAI 插桩的应用零改动直接接入（OTLP 直收）。

### 3.12 监控聚合 = 告警聚合与降噪【含义已确认】

项目所有者已确认："监控聚合"特指**告警聚合与降噪**。目标：告警风暴中用户收到的不是 500 条通知，而是一条带上下文的聚合摘要。语义模型参考 Alertmanager（业界验证过的分组/静默/抑制模型），产品体验对标夜莺告警管理及 PagerDuty/BigPanda 的事件关联：

- **分组（Grouping）**：按标签/服务/规则/时间窗将告警合并为告警组，一个组只发一条通知；组内新增告警更新通知而非新发。
- **去重（Dedup）**：同指纹告警在窗口内只计数不重发。
- **静默（Silence）**：基于标签匹配器的静默规则 + 计划维护窗口（到期自动恢复）。
- **抑制（Inhibition）**：高级别告警触发时抑制其派生告警（例：主机宕机抑制该主机上所有服务级告警）。
- **风暴抑制与根因合并**：短时间大量同源告警合并为一条"风暴摘要"通知；通知渠道级限流。
- **AI 关联解释（衔接 3.10）**：聚合后的告警组作为整体交给 AI 分析——跨信号关联（同时段指标突变、错误日志、trace 异常、最近部署），输出疑似共同根因的自然语言摘要，附在通知中。

节奏：M1 随告警基础落地分组与静默（最小可用），M3 补全抑制/风暴合并，AI 解释在 M4 随 3.10 落地。

> 备注：原先列出的另外两种理解——多数据源聚合、第三方系统单一面板——不属于本需求。前者作为 Standalone 模式的自然延伸保留在 M4 规划中，后者仍列为 v2 插件生态方向。

### 3.13 主机 / 基础设施监控【由覆盖目标推导，见 1.3-2】

- **不自研采集端（已确认）**：主机与中间件监控完全基于社区 exporter 生态。主机指标 = node_exporter；中间件指标 = 对应社区 exporter（mysqld_exporter、redis_exporter、postgres_exporter、nginx exporter、kafka_exporter 等）。我们的开发面全部在产品层，零采集逻辑。
- **抓取与接入**：server 端抓取任意 Prometheus exporter（内嵌 vmagent 承担），配 **抓取目标管理 UI**（添加主机/exporter 端点、抓取间隔、健康状态一目了然）；categraf/Telegraf 等推送型采集器走 remote write 接收。不为任何特定采集器做专项适配。
- **受限网络 recipe**：server 无法反向连通主机（NAT/防火墙）的场景，官方文档提供"主机侧 vmagent 采集本机 exporter → remote write 推送"的标准配方，不需要我们发明任何东西。
- **主机视图 UI**：机器列表（从 node_exporter 指标派生）、单机详情（预置系统 dashboard）、主机分组与标签；针对 node_exporter 与常见中间件 exporter 的内置 dashboard——指标名即社区标准，用户存量 Grafana dashboard 与告警规则同样可用。
- **自研 agent：不做**。采集端完全交给社区生态（拉：exporter；推：vmagent/categraf 等），本产品不发布任何采集端产物。

### 3.14 持续剖析（Profiling）【已确认：核心功能，不延后】

- **定位**：性能监控（3.7）的最后一层下钻——端点指标告诉你"哪个接口慢"，trace 告诉你"慢在哪个环节"，profiling 告诉你"CPU/内存烧在哪一行代码"。
- **SDK 采集**（遵循 4.1 显式原则，随 `Init` 可开关）：
  - **Go（M3 首发）**：基于 runtime/pprof 的持续采样（CPU、heap、goroutine），采样开销低且生态最成熟，从它切入风险最低。
  - **Node（M4）**：V8 CPU profiler 周期采样。
  - 浏览器：JS Self-Profiling API 目前仅 Chromium 支持，列为 v2 观察项，不进当前计划。
- **存储与查询（本功能的核心风险）**：profiling 是唯一没有 VM 引擎可内嵌的信号。Grafana Pyroscope 为 AGPL 不可用；**v1 务实路线**：profile 以压缩 pprof blob 存储（复用 replay 的本地磁盘/S3 存储抽象），SQLite 存索引（服务/类型/时间窗），UI 按时间窗合并 pprof 渲染火焰图。Parca（Apache-2.0）内嵌可行性列为 M3 前调研项。OTel profiles 信号 2026-03 进入公开 Alpha（OTLP 中仍为 Development 状态，官方明确不建议生产使用，生产级后端尚未出现），**不作为 v1 依赖**；因 pprof 与 OTLP Profiles 可无损双向转换，现行 pprof 存储路线在其 GA 后可加转换层平滑对接（届时还可直收 OTel eBPF profiler 的全系统采集数据）。
- **关联**：trace span 与同服务同时间窗的 profile 互跳（"这个慢请求发生时 CPU 在烧什么"），延续关联跳转的体验主线（第 6 节）。

## 4. 客户端 SDK

### 4.1 设计原则（硬性约束）

1. **禁止 monkey patch / 全局运行时 hook**。一切插桩显式化：middleware、handler、包装构造函数。
2. **默认即完整**：接入 = 引包 + 一行 middleware + 一个 DSN 配置，即产出 3.2–3.7 的全部默认数据。
3. **配置尽量收敛（设计追求，非硬性）**：默认形态是一个 `Init(config)` + 一个 DSN（携带 endpoint + token + project），但具体形式跟随目标框架自身的惯例与约束灵活适配——真正的目标是**最小配置复杂度**，不是形式上的单一入口。
4. **可裁剪**：每类信号可独立开关（`metrics: false` 等）。
5. **传输统一走 OTLP**（错误/replay 走私有协议），SDK 本质是"预配置好的 OTel + 平台扩展"。

### 4.2 覆盖矩阵【已确认收窄】

P0 只做三个目标，后续覆盖矩阵暂不规划：

| 端 | 目标 | 能力 |
|---|---|---|
| Go 服务端 | **Fiber** | middleware + slog handler + DB 包装（metrics/logs/traces/panic 捕获） |
| Node 服务端 | **Express** | middleware + logger 接入，与 Go 能力对齐 |
| 浏览器 | 框架无关核心 + **React** 适配层 | 错误、Web Vitals、fetch 追踪（traceparent 注入）、Session Replay；React 层：ErrorBoundary、路由级性能 |

架构约束：SDK 必须做**核心层与框架适配层分离**（Go core / JS core 承载全部逻辑，Fiber/Express/React 只是薄适配），保证未来扩框架是低成本追加而非重写——但何时扩、扩什么，现在不规划。

### 4.3 接入体验基准（验收样例）

```go
lo := lazyobserve.Init(lazyobserve.Config{DSN: "http://host:3100/p/myapp@token"})
app.Use(lo.Fiber())          // HTTP 指标+trace+panic 捕获
slog.SetDefault(lo.Slog())   // 日志接入+trace 关联
```

```ts
import { init } from "@lazy-observe/browser";
init({ dsn: "...", replay: { onErrorOnly: true } });
```

## 5. 架构

### 5.1 All in One 模式

```
┌─ lazy-observe (单二进制) ──────────────────────────┐
│  Fiber 后端: API / Ingest Gateway / 静态前端资源    │
│  嵌入进程内: VictoriaMetrics │ VictoriaLogs │ VictoriaTraces │ vmalert │
│  本地存储: ./data/{vm,vlogs,vtraces,replay,meta}   │
│  元数据: SQLite (项目/用户/Issue/告警规则)          │
└────────────────────────────────────────────────────┘
```

- **嵌入方式【已定案：路线 B，M0/P1 实测】**：官方 release 二进制 go:embed 进主二进制 + 运行时解出、子进程监督（启动顺序、健康检查、退出联动、优雅停机——均已 PoC 验证）。
  - 路线 A（库方式 import）实测不可行：三组件入口均为 main 程序不可 import；内部包两两组合触发 `flag redefined` panic（`storageDataPath`/`retentionPeriod`）；VictoriaLogs 与 VictoriaTraces 独立 module 经 MVS 产生版本漂移。
  - 实测数据（VM v1.147.0 / VLogs v1.52.0 / VTraces v0.9.4，darwin-arm64）：三 payload 共 64MB，嵌入后二进制估算 ~72MB（目标 <150MB）；全套空载 RSS ~73MiB（目标 <500MB）。
  - 内嵌组件版本随平台版本锁定，升级即整体更换（与第 7 节平滑升级承诺一致）。
- 前端构建产物 `go:embed` 进二进制。
- 目标：`./lazy-observe server` 一条命令可用；二进制体积目标 < 150MB；空载内存目标 < 500MB。

### 5.2 Standalone 模式【兼容范围已确认：VM 系 + Grafana 系】

- 只跑 Fiber 后端 + 前端 + SQLite 元数据。
- 存储通过配置外接，兼容范围与实现成本分三档：
  - **Metrics：任意 Prometheus 兼容查询端点**（VictoriaMetrics / Grafana Mimir / Prometheus 本体 / Thanos）——四者共享同一套 PromQL HTTP API，一个实现全覆盖，成本近零。
  - **Logs：VictoriaLogs + Grafana Loki**。Loki 走 LogQL API，与 VictoriaLogs 的 LogsQL 语义不同，需要在 storage 接口层做**独立的 LogQL 查询适配器**——这是本决策新增的最大工作项。
  - **Traces：VictoriaTraces + Grafana Tempo**。VictoriaTraces 本身实现 Jaeger 查询 API，我们的 trace 查询层以 Jaeger API 为基准；Tempo 侧优先通过其 Jaeger 兼容查询端点（tempo-query）接入（近零成本），Tempo 原生 API/TraceQL 适配器视需求再评估。
  - **明确不做**：Elastic 系、独立 Jaeger 后端官方支持（后者因走 Jaeger API 基准大概率天然可用，但不承诺、不测试）。
- License 说明：兼容 Grafana 系仅指作为**查询 API 客户端**对接用户已部署的 Loki/Tempo/Mimir，不引入其 AGPL 代码，无 License 传染问题。
- 节奏：VM 系适配随 Standalone 模式在 M3 落地；Loki/Tempo 适配器作为独立工作项排在 M3 后段或 M4，不阻塞 Standalone 首发。
- 实现要求：后端所有存储访问经由 **storage 接口层**，All in One 与 Standalone 走同一接口的不同实现（内嵌地址 vs 外部地址），保证两模式不分叉。
- Replay 与错误聚合无外部标准可依赖，两种模式均由平台自身存储。

### 5.3 HA / 分布式部署模式【新增需求】

定位：满足高可用与大规模场景（K8s 环境）。形态上是 **Standalone 模式的多副本演进**——同一个二进制的第三种部署形态，不是独立代码分支；延续 K3s 式故事：从单机单二进制到集群，用户不换产品。

- **架构：平台层无状态多副本**（LB 后水平扩展），一切状态外置：
  - 元数据 → **PostgreSQL**（此模式必选，即 5.4 的可选项在此转正）；
  - 共享状态/缓存 → **Redis/Valkey**（session、限流计数、Issue 指纹热映射、通知去重）；
  - 时序信号 → **VM 集群版**（VictoriaMetrics cluster / VictoriaLogs cluster；VictoriaTraces 集群能力需在 M5 前验证）或任意 5.2 兼容端点；
  - Replay 分片 → **S3 兼容对象存储**（此模式必选）。
- **告警 HA**：vmalert 双副本重复评估 + 聚合降噪层按告警指纹去重；通知发送经分布式锁/租约保证单发不重发。
- **K8s 交付**：官方 Helm Chart、健康/就绪探针、优雅停机（摄入排空）、HPA 兼容（平台层无状态即可）；Operator 列为 v2 方向。
- **分布式演进约束（从 M1 起生效的设计纪律，避免 M5 变成重写）**：
  1. 平台进程不得持有不可重建的本地状态（All in One 的嵌入存储是显式例外）；
  2. 有状态点（session、限流、攒批计数、replay 分片组装）一律走接口抽象，进程内实现与 Redis 实现同接口（衔接 5.4 缓存纪律）；
  3. Replay 存储从 M2 落地时即抽象为本地磁盘 / S3 两个实现；
  4. Issue 聚合的攒批写入设计需兼容多副本（按指纹分片路由或共享存储原子累加，M5 定案）。

### 5.4 元数据数据库与缓存选型【已确认】

**元数据库：默认 SQLite + 可选外接 PostgreSQL。**

- **默认：SQLite**，零配置——产品故事和 90% 用户走这条路。驱动用 **modernc.org/sqlite**（纯 Go，避免 CGO 破坏四平台交叉编译），WAL 模式。
- **可选：外接 PostgreSQL**（配置一个连接串切换）——给有 DBA 运维习惯、想统一备份策略、或未来跑多副本的团队。Gitea/Grafana 验证过的成熟模式，随 M3 落地。
- 选型逻辑：元数据负载（低频关系型 + Issue 聚合）用不上外部数据库的并发/HA 优势，默认路径不应为零收益付部署成本；嵌入式候选中只有 SQLite 提供 SQL/事务/索引；SQLite 数据即 `./data/meta` 下单文件，天然满足"目录备份即恢复"。
- **ORM【已确认】：ent**（entgo.io）。schema-as-code + 类型安全生成代码；基于 `database/sql`，SQLite 走 modernc 纯 Go 驱动注册（不破坏无 CGO 约束），SQLite/PostgreSQL 双方言由 ent 统一承载——"两个数据库一套代码"主要由 ent 兜底，repository 接口层（与 5.2 storage 接口层同思路）保留但可以更薄。注意事项：CI 必须对两种方言都跑测试（方言差异不能只靠 ent 承诺）；schema 迁移用 ent 的版本化迁移（Atlas），不用运行时 auto-migration 裸奔——自托管产品的升级路径要可控可回滚。
- **写入边界（硬性纪律，SQLite 单写者模型决定，PG 模式同样遵守）**：
  - 进元数据库：项目、用户、token、告警/静默规则、告警事件与通知历史、dashboard 配置、保留策略、**Issue 聚合行**（指纹、状态、计数、首末次出现）、replay 分片索引。
  - 不进元数据库：错误事件原始数据（高频 append，存 VictoriaLogs 作为结构化日志流）、replay 分片本体（磁盘/对象存储）、一切时序信号（VM 三件套）。
  - Issue 计数等高频更新在内存攒批、周期 flush，禁止逐事件写库。

**缓存：v1 只做进程内缓存，不引入外部缓存依赖。**

- 进程内缓存覆盖热点路径：ingest token/项目鉴权查找（每请求命中，写时失效，禁止逐请求查库）、Issue 指纹→Issue ID 热映射、限流计数器、查询结果短 TTL 缓存。
- **Redis/Valkey 不进单节点形态**：其增量价值仅在跨副本共享状态，单节点下收益为零而配置/测试成本翻倍；与 HA 分布式模式绑定（见 5.3），随 M5 落地。
- 设计纪律：session、限流计数等有状态点走接口抽象，将来追加 Redis 实现是低成本扩展而非重构。

### 5.5 项目数据隔离模型（Project → 存储层的强隔离）

**原则：VM 三件套永不直接暴露**（All in One 端口只绑 localhost / 进程内；HA 模式走内网），一切读写经网关收口，隔离在网关这个唯一强制点执行。写入侧由 ingest token 解析项目，查询侧由会话校验项目成员资格，然后按引擎盖章：

- **VictoriaLogs / VictoriaTraces**：使用原生多租户能力——摄入与查询均带 `AccountID`/`ProjectID` header，平台 project 一对一映射 ProjectID，存储层物理隔离。**已实测确认**（M0/P2，VLogs v1.52.0、VTraces v0.9.4：两引擎同一套 header，摄入/查询/跨租户互查全部隔离正确）。
- **VictoriaMetrics 单机版**（无原生租户）：标签级隔离（vmauth 模式）——写入侧网关注入 `project_id` 标签（**覆盖**客户端同名标签，不可伪造）；查询侧网关对每个请求附加服务端 `extra_filters`，VM 将其应用于查询中所有序列选择器。**已实测确认**（M0/P2，VM v1.147.0：正则、`label_replace`、`or` 双臂、显式他项目选择器四类对抗用例均无法逃逸）。
- **网关强制纪律（M0/P2 实测发现，安全关键）**：VM 的 `extra_filters[]` 重复出现时为 **OR 语义**——网关转发查询前必须**剥离客户端请求中的一切保留参数**（`extra_filters`、`extra_filters[]`、`extra_label`）后再注入唯一可信过滤器；对 VLogs/VTraces 查询代理同理必须剥离/覆盖客户端自带的 `AccountID`/`ProjectID` header。此剥离逻辑必须有专项测试。
- **HA 模式（M5）**：VM 集群版原生租户（accountID），从标签方案平滑升级。
- 平台自有数据（Issue、replay 索引等）在元数据库天然带 project 外键；replay 分片按项目分目录/前缀。

**保留策略【已确认】**：**保留期按信号类型全局配置，不做按项目差异化**（VM 单机与 VictoriaLogs 的保留期本就是全局的，按租户 retention filter 属 VM 企业版；不为此开发定时删除任务）。按项目保留若未来有真实需求，随 M5 集群版再评估。

### 5.6 配置

- 单一 `config.yaml` + 环境变量覆盖 + 合理默认值（零配置可启动）。
- `lazy-observe server`（All in One）/ `lazy-observe server --standalone`；HA 分布式即 standalone 形态多副本运行 + 外置状态配置（PG/Redis/S3/VM 集群），不新增第三种命令形态。

## 6. 前端

- 技术栈：TanStack Start / Query / Table / Router / Form，shadcn/ui，Tailwind。
- 国际化：中英双语，i18n 架构从 M0 引入（后补 i18n 是灾难，见 1.3-4）。
- **布局风格【已确认】**：**侧栏 + 顶栏采用 GitLab 风格**——左侧固定主导航侧栏（可折叠为图标栏、分组导航、支持置顶常用项），顶栏承载面包屑、全局搜索/命令面板（⌘K）、项目切换器、用户菜单；**其余一切组件与页面均为 shadcn 风格**（基于 shadcn/ui，不混入第二套组件语言）。
- **主题系统【已确认，含主题切换器】**：全部视觉参数收敛为 CSS 变量（色板、radius、字号、间距、图表色系），图表库吃同一套变量（候选：ECharts 或 visx，需评估大数据量时序渲染性能）。提供 **Shadcn UI Kit 式主题切换器**（参考 shadcnuikit.com/dashboard/default）：面板内实时切换——多套预设主题色板、亮/暗/跟随系统、圆角档位、布局密度/侧栏变体——即时生效并按用户持久化；自定义主题即覆盖 CSS 变量集，无需构建。
- 核心页面：总览、Metrics 浏览器、日志流、Trace 查询/瀑布、Issues、Replay 播放器、性能（前端 Vitals + 后端端点）、项目/设置/告警。
- 关联跳转是体验核心：任何一条数据都能跳到同 trace_id / session_id 的其他信号。

## 7. 非功能需求

- **性能**：单节点承载 ~50k samples/s 指标、~10k logs/s（VM 单机能力范围内，需压测确认网关不成为瓶颈）；HA 模式下平台层摄入吞吐随副本数近线性扩展（M5 建立水平扩展压测基准）。
- **可用性**：单节点形态接受进程级重启恢复（数据不丢由 WAL/存储层保证）；HA 模式目标为平台层无单点、滚动升级不中断摄入。
- **安全**：ingest token 鉴权、UI 登录（用户名密码 + session）、可选 TLS、Replay 默认脱敏。
- **运维性**：自身也要可观测（self-monitoring 默认开启，吃自己的狗粮）；数据目录整体备份即可恢复；平滑升级（内嵌组件版本随平台版本锁定）。
- **发布**：linux/darwin × amd64/arm64 二进制 + Docker 镜像 + Homebrew/一键安装脚本；M5 起提供官方 Helm Chart。

## 8. 里程碑

- **M0 — 可行性骨架（最高风险先行）**：VM 三件套嵌入单二进制的 PoC（路线 A/B 定案）、**项目隔离机制验证（VLogs/VTraces 租户 header + VM 标签注入/extra_filters，见 5.5）**、Fiber 网关透传 OTLP、SQLite 元数据、前端壳 + 一个能查询指标的页面。
- **M1 — Go 全链路 + 告警基础**：Go SDK（Fiber + slog + DB 包装）、Metrics/Logs/Traces 查询 UI、trace↔log 关联、项目与 token 管理、默认 dashboard、**告警基础（阈值规则 + 分组/静默最小可用 + 飞书/钉钉/企微/Webhook 通知）**（告警提前进 M1，由覆盖目标 1.3-1 决定）。
- **M2 — 前端可观测性 + Node**：浏览器 SDK（错误、Vitals、fetch 追踪 + React 适配层）、Express SDK、Issue 聚合与 Source Map（堆栈指纹算法为本期重点技术风险，对标 Sentry）、Session Replay 采集+回放、前后端 trace 贯通。
- **M3 — 完整产品**：**主机/中间件监控（exporter 抓取 + 抓取目标管理 UI + 主机视图与内置 dashboard，见 3.13）**、告警聚合降噪补全（抑制/风暴合并/渠道限流）、Standalone 模式（首发覆盖 VM 系 + Prometheus 兼容 metrics；Loki/Tempo 适配器为后续独立工作项，见 5.2）、**Go 持续剖析（采集 + 火焰图 + trace 关联，见 3.14）**、保留策略与清理、主题系统完善、**MCP Server**。
- **M4 — AI 与聚合深化**：AI Assistant（自然语言查询）、Issue AI 摘要与根因提示、LLM 应用可观测性（GenAI 语义约定 + token/成本仪表盘 + LLM trace 视图）、**Node profiling（见 3.14）**、多数据源聚合。ML 异常检测、agentic RCA、第三方系统聚合插件列为 v2 方向。
- **M5 — HA/分布式与 K8s**：平台层无状态化收尾与多副本部署、PostgreSQL/Redis/S3 全链路转正、VM 集群版对接（含 VictoriaTraces 集群能力验证）、告警评估与通知 HA（去重 + 单发保证）、官方 Helm Chart 与 K8s 交付（探针/优雅停机/HPA）、水平扩展压测基准。前提是 M1–M2 遵守 5.3 的分布式演进约束，此期为"转正"而非重构。

## 9. 决策记录（原开放问题，已全部确认）

1. ~~存储引擎选型？~~ **已确认：VictoriaMetrics 三件套**（见 0）。
2. ~~项目性质：开源还是商用/闭源？~~ **已定：延后，项目初期以产品为主**。无技术阻塞——当前依赖全为 Apache-2.0/MIT，两条路都不锁死；唯一纪律是**初期不引入 AGPL/GPL 依赖**，保持决策自由。
3. ~~告警是否必须进 v1？~~ **已定：进 v1 且提前到 M1**（要赢夜莺，告警是底线能力，见 1.2-1）。
4. ~~Standalone 模式需要兼容的既有系统清单？~~ **已定（修正版）：VM 系 + Grafana 系**——Metrics 走 Prometheus 兼容 API（天然覆盖 Mimir/Prometheus/Thanos），Logs 增加 Loki 适配器，Traces 经 Jaeger API 基准覆盖 Tempo；不做 Elastic 系（见 5.2）。
5. ~~后端 SDK 首发语言？~~ **已定：P0 = Go/Fiber + Node/Express + 浏览器核心/React，后续矩阵暂不规划**（见 4.2）。
6. ~~Session Replay 默认策略？~~ **已定：采样 + 错误全录（Sentry 默认模式）**，见 3.6。
7. ~~多用户/团队权限在 v1 的最小范围？~~ **已定：单团队 + admin/member/viewer 三角色**（实例即团队，与多租户部署层隔离决策一致，见 3.9）；SSO v2。
8. ~~产品名是否定为 lazy-observe？~~ **已确认：lazy-observe**。
9. ~~"监控聚合"的确切含义？~~ **已定：特指告警聚合与降噪**（分组/去重/静默/抑制/风暴合并 + AI 关联解释），见 3.12。
10. ~~AI 集成的优先方向？~~ **已定：维持现排序**——MCP Server 最先（M3），AI Assistant / Issue AI 分析 / LLM 应用监控进 M4，ML 异常检测等 v2。
11. ~~LLM 接入策略？~~ **已定：BYO API key**（OpenAI/Anthropic 兼容接口 + 本地 Ollama），平台不内置不捆绑任何模型，AI 功能默认关闭；完全离线场景由 Ollama 路径覆盖（见 3.10）。
