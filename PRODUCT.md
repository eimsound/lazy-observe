# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Scope

本文件只覆盖 `web/` 下内嵌在二进制里的控制台。以下不在范围内：

- `docs/` 的 Zensical 文档站。
- 告警通知消息本身（飞书卡片、钉钉消息、邮件正文等）。
- 营销落地页，目前不存在。

## Users

lazy-observe 面向单一团队，不是多团队共用的租户平台。项目是同一个团队内部对应用或环境的划分。

- **主要用户：观测自己应用的开发者。** 自己接入 SDK、自己排障，需要在一屏里看到足够密的信息。
- **只用本平台做观测的小团队。** 没有另一套监控体系，开发者兼做运维，主机、抓取目标和告警也在这里看。
- **专职运维人员不是主要设计对象。** 他们多半使用外部仪表盘，本产品对他们的承诺是 Prometheus 兼容的查询接口。

账号角色有三种，界面按 `member` 设计：

- `member`：日常排障的开发者，默认角色。除只读外，可以写告警规则、改 Issue 状态、管理抓取目标、手工触发 AI 分析。
- `admin`：负责部署和维护这套平台的一两个人。另外管理用户、项目、通知渠道、AI 设置、审计和诊断。
- `viewer`：团队里只需要看的人，例如测试、产品、临时协作者。只读访问观测数据、项目、抓取目标、Issue 和告警事件，看不到告警配置（规则、静默、抑制规则、通知历史）与管理员页面，复用同一套界面去掉写操作与无权限入口，不为它单独设计页面。

部署场景：All-in-One（没有现成监控体系的团队、内网或私有化交付）、Standalone（已自己运维 Victoria 系存储的团队）、Clustered-HA（流量超出单机、需要高可用的单一团队）。

## Product Purpose

lazy-observe 是自托管的一体化可观测性平台：一个纯 Go 二进制起全套，SDK 开箱即用。它把指标、日志、链路、错误追踪、Web Vitals、会话回放、持续剖析、告警和 GenAI 用量与成本放进同一个按项目隔离的平台。

项目所有者确认的三个目标，按此顺序理解取舍：

1. **功能覆盖。** 尽量覆盖夜莺（Nightingale）、Prometheus 系、VictoriaMetrics 系和 Sentry 的功能面。
2. **易用性。** 这是核心差异化：客户端尽量开箱即用，把学习成本降到最低。
3. **性能。** 优于自托管 Sentry。

产品自托管、可离线，没有使用数据回传，所以控制台的成功用两条可观察的标准衡量：

1. 从启动二进制到在控制台看到第一条自己的数据，不超过五分钟。
2. 错误调查旅程和性能调查旅程能在控制台内走完，中途不需要手工复制 ID，也不需要离开控制台。

## Positioning

- **部署侧。** K3s 式单二进制，默认内嵌 VictoriaMetrics、VictoriaLogs、VictoriaTraces、`vmagent` 和 Web UI，零依赖启动；单机不够时，同一个二进制切到外接 Victoria 集群、PostgreSQL、Valkey/Redis 和 S3 的高可用拓扑。
- **接入侧。** SDK 是产品而不是协议库：每个框架一行 middleware 得到完整默认指标集，显式插桩，不做运行时 hook。
- **能力侧。** 各类信号连成核心可观测闭环：从一条证据能走到与它相关的其他证据。
- **生态侧。** 不另立山头：Prometheus 和 OpenTelemetry 生态完全兼容，存量用户平移进来。

## Operating Context

- 用户在排障时使用控制台，走错误调查旅程或性能调查旅程，在日志、指标、链路、会话回放和持续剖析之间取证。
- 查询语言是日常工具：PromQL/MetricsQL 有 CodeMirror 编辑器和与之双向同步的可视化构建器；LogsQL 用 CodeMirror 编辑器加结构化过滤和字段统计。
- 自由编排的监控视图不在控制台里做，用户把 lazy-observe 接成 Grafana 或 Perses 的数据源，即外部仪表盘。
- 外部 AI 助手通过只读 MCP 端点查询项目遥测。
- 内网和离线部署是常见环境。

## Capabilities and Constraints

控制台现有的固定诊断界面：Overview、Logs、Metrics、Traces、Issues、应用性能（Web Vitals 与后端端点）、Replays、Profiling、LLM 可观测性、基础设施（Hosts、抓取目标）、告警（规则、事件、静默、抑制规则、通知渠道、通知历史），以及设置（项目、用户、AI、审计、诊断）。

约束：

- **桌面优先。** 手机和电脑是同一套路由、同一套页面，做合理的响应式布局即可；不为手机单独设计页面，也不以手机为验收对象。
- **不内置仪表盘编排**（ADR 0017）。只提供固定诊断界面。
- **信号存储只用 Victoria 三件套**（ADR 0013）。不支持 Loki/LogQL 和 Tempo/TraceQL。
- **项目硬隔离。** 网关在每次读写上强制项目边界，界面始终处在某个项目的上下文里。
- **UI 中英双语，中文是源语言。** 新文案先写中文，英文在同一个 PR 内补齐，不接受只有一种语言的文案。
- **离线可用。** 控制台不能依赖外网资源。
- **AI 是可选的附加能力。** 管理员自带模型，默认关闭。没有 AI 时产品完整可用，AI 入口不占主导航和首屏。
- **代码或查询语言的输入框和文本展示优先用 CodeMirror。**
- **许可证。** MIT；生产和运行时依赖禁止 AGPL/GPL。
- **公开预览期（v0.x）。** 会发布但不受支持，协议和 SDK 在预览版本之间可能不兼容。

非目标：多团队租户管理、内置仪表盘编排、为专职运维做大规模主机管理能力、手机专属界面、无障碍合规。

领域术语以 `.agents/docs/GLOSSARY.md` 为准。

## Brand Commitments

- 产品名写作 `lazy-observe`，全小写带连字符。
- 文案技术化，简单、清晰、直接。
- 控制台的视觉权威是根目录 `DESIGN.md`（lazy-observe Console）。

## Evidence on Hand

- 产品说明：`README.md`、`README.zh-CN.md`。
- 需求基线：`.agents/docs/requirements.md`。
- 与 SigNoz、OpenObserve、Sentry、Traceway 的对比，以及何时不该选 lazy-observe：`docs/comparison.md`。
- 架构与决策：`.agents/docs/architecture.md`、`.agents/docs/adr/`。
- 可运行的全信号示例：`sdk/go/examples/fiber/`。
- 基准测试代码：`bench/`。

目前没有的东西，后续工作不得编造：客户名单、用户评价、案例研究、媒体报道、定价。

## Product Principles

1. **易用性优先于功能堆叠。** 学习成本是第一道门槛。
2. **界面靠结构说话。** 尽量仅呈现基础视觉层与核心交互组件。若脱离辅助文本后视觉层级无法自洽，考虑重新设计布局。
3. **证据要能走通。** 任何一条日志、链路、错误或回放，都应该能走到与它相关的其他证据。
4. **兼容而不重造。** 生态里已有的东西（PromQL、OTLP、exporter、Grafana）直接接入，自研只放在产品层。
5. **没有 AI 也完整。** AI 只做附加，不做前提。
