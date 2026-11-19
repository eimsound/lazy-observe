# lazy-observe 领域术语表

lazy-observe 是面向单一团队的可观测平台，将公共访问、信号存储、告警评估和元数据演进统一在一个产品边界内。以下术语用于区分可能共用同一二进制、但运行时职责不同的角色和概念。

## 兼容性策略

**发布**与**受支持**是两个概念：发布是打 tag、出包；受支持是项目承诺升级路径（冻结数据库基线并提供只追加迁移）。v0.x 是公开预览：会发布，但不受支持，升级可能需要重建数据库，协议和 SDK 可能有不兼容变化。首个受支持版本的版本号和时间另行决定，不等于 v0.1.0。

项目尚处于首个受支持版本之前。当前协议、API、数据库结构（schema）和 SDK 的变更不保留旧版本兼容行为；废弃路径应直接删除，不保留别名或回退。每种数据库方言只有一个可变的 `001_baseline.sql`，不提供版本间升级链；数据库结构变更直接合入基线，开发数据库必须重建。开发阶段允许重建数据库，但不降低同一构建版本内的运行时持久性要求：正常或异常重启都不得静默丢失或损坏已提交数据。必须在首个受支持版本发布前冻结基线并引入只追加迁移（append-only migrations）。

## 当前运行时角色

**网关（Gateway）**：
lazy-observe 对外提供 UI 和 API 的公共角色，负责对写入和查询流量执行项目隔离。
_避免使用_：后端、告警引擎

**应用副本（Application replica）**：
一个正在运行的 lazy-observe 进程或 Pod。高可用部署中的应用副本共享一个可写的 PostgreSQL 元数据数据库。

**Issue AI 协调器（Issue AI coordinator）**：
主服务中控制持久 Issue AI 分析任务生命周期的运行时角色；一个任务的领取、执行权、上下文快照、尝试安排和最终结算都属于该角色。
_避免使用_：Issue AI Worker、AI Provider

**Issue AI 请求 Worker（Issue AI request worker）**：
执行一次已经确定输入和配置的模型请求并返回调用结果的运行时角色；它不拥有 Issue AI 分析任务，也不决定任务的领取、重试或终态。
_避免使用_：Issue AI 协调器、Issue AI 分析任务、AI Provider

**Issue AI 租约（Issue AI lease）**：
一段有期限且可续期的 Issue AI Job 执行权。租约过期后 Job 可以被重新接管，但原所有者不再有权推进状态或发布结果。
_避免使用_：数据库锁、永久任务所有权

**Issue AI 接管纪元（Issue AI claim epoch）**：
标识 Issue AI Job 当前执行权的单调序号。新接管使旧纪元失效，模型调用记录引用发起请求时的当前纪元。
_避免使用_：模型尝试次数、缓存 owner token

**托管组件（Managed component）**：
生命周期由 lazy-observe 拥有的存储或采集进程。All-in-One 的存储子进程及任何由 lazy-observe 启动的 vmagent 都属于托管组件；Standalone 的存储服务属于外部依赖。
_避免使用_：外部存储、应用副本

## 告警角色

以下术语描述 ADR 0001 确立的已实现架构；租约与成员过期所用的数据库时钟约束见 ADR 0011。

**告警引擎（Alert engine）**：
由 lazy-observe 拥有的领域边界，包含规则评估、告警状态、分组、抑制和通知。
_避免使用_：vmalert、Alertmanager

**规则所有者（Rule owner）**：
在一个所有权纪元内，唯一有权为某条规则提交状态和通知工作的告警引擎成员。
_避免使用_：集群主节点、主网关

## 持久化演进

**元数据基线（Metadata baseline）**：
首个受支持版本之前，用于创建新元数据数据库的当前完整 SQLite/PostgreSQL 数据库结构（schema）。
_避免使用_：Ent 自动迁移、Atlas 运行时迁移

## 产品体验

**核心可观测闭环（Core observability loop）**：
将指标、日志、链路、Issue、会话回放、持续剖析和应用性能连接成一条调查路径的产品体验。
_避免使用_：整个产品、生产就绪

**准生产功能质量（Pre-production feature quality）**：
核心可观测闭环具备可靠主流程、明确失败反馈、同一构建版本内的运行时持久性，以及可重复自动回归证据时达到的成熟度；它不承诺生产使用、跨版本升级或用户支持。
_避免使用_：生产就绪（Production readiness）、正式可用（General availability）

**错误调查旅程（Error investigation journey）**：
从捕获到的浏览器错误开始，经由其 Issue 进入相关会话回放、链路和日志证据的核心可观测闭环。

**性能调查旅程（Performance investigation journey）**：
从应用性能开始，经由缓慢或失败的端点进入相关链路、日志、持续剖析和指标证据的核心可观测闭环。

**日志探索工作区（Logs exploration workspace）**：
在所选项目中，由 LogsQL 查询、时间与过滤意图，以及历史结果或实时日志流共同组成的交互式调查；不包含纯视觉展示偏好和已保存查询。
_避免使用_：日志页面、仪表盘、已保存查询

**应用性能（Application Performance）**：
在同一产品入口中展示浏览器 Web Vitals 与后端端点延迟、吞吐量和错误率，并通过独立的前后端视图引导用户进入链路证据的观点明确型诊断体验。
_避免使用_：仪表盘、指标探索器

**持续剖析（Continuous profiling）**：
按固定节奏对运行中的服务进程采集调用栈，按资源类型（如 CPU、内存）汇总成剖析数据，用来回答「时间或资源花在哪些代码上」。
_避免使用_：性能监测、性能采样、应用性能

**外部仪表盘（External dashboard）**：
由 Grafana 或 Perses 等外部工具自由编排、以 MetricsQL/PromQL 面板构成的监控视图；本产品只提供 Prometheus 兼容查询接口，查询用用户 API Token（`lou_…`）加 `X-LO-Project` 请求头选择项目（项目 API Token 只能摄取，见 ADR 0010），不内置看板编排。
_避免使用_：应用性能、指标探索器

**因果证据链接（Causal evidence link）**：
通过传播的稳定标识符建立的跨信号关系，例如 trace ID、replay ID 或 error event ID。
_避免使用_：时间窗口匹配、可能相关

**上下文证据链接（Contextual evidence link）**：
依据共同服务和时间范围选择的跨信号关系，不宣称两条记录来自同一次请求。
_避免使用_：因果链接、精确匹配

## 凭据

**项目 API Token（Project API token）**：
属于某个项目、会被写进公开页面的摄取凭据；只能把遥测写入所属项目，不能查询，也不能上传 Source Map（ADR 0010、ADR 0018）。
_避免使用_：DSN token、ingest key、项目密钥

**用户 API Token（User API token）**：
代表某个用户身份的凭据，用于查询和 MCP，权限跟随该用户在项目里的角色（ADR 0010）。
_避免使用_：个人 Token、查询 Token

**Source Map 上传 Token（Source map upload token）**：
属于某个项目、只放在接入方构建流水线里的保密凭据；只能为所属项目上传 Source Map（ADR 0018）。
_避免使用_：发布 Token、CI Token、项目 API Token

## 错误跟踪

**错误事件（Error event）**：
项目内一次具有稳定 `event_id` 的已观测错误；同一 `event_id` 的重投表示同一事实，不同内容不得覆盖首次被持久接管的内容。
_避免使用_：Issue、投递尝试

**规范化发生时间（Normalized occurrence time）**：
错误事件携带的客户端发生时间经过服务端时钟漂移修正和合法性校验后的时间；产品错误率和趋势按该时间归属，摄取容量与拒绝率使用服务端接管时间，Worker 完成时间只用于处理延迟诊断。
_避免使用_：接管时间、处理完成时间

**Source Map 制品快照（Source Map artifact snapshot）**：
错误事件被持久接管时固定的不可变符号化输入，由内容寻址的 Source Map revision/digest 或显式的“无匹配制品”组成；Worker 不得在重试时改用后来上传、覆盖或删除后的当前制品。
_避免使用_：当前 Source Map、可变文件名映射

**Issue**：
项目内按指纹聚合错误事件形成的调查对象，拥有计数、首次与最近出现时间，以及 open、resolved、ignored 和回归语义。状态修改记录服务端 `status_changed_at`；只有在该时间之后被持久接管的错误事件才能使 resolved Issue 回归，ignored Issue 不自动回归。
_避免使用_：单次错误事件、告警通知

**Issue 生命周期事件（Issue lifecycle event）**：
错误事件使新 Issue 出现或使已解决 Issue 回归时产生的稳定持久事实，供告警引擎及可选下游消费；它不表示通知已经送达或 AI 分析已经完成。
_避免使用_：提交后回调、通知、AI 任务

**Issue AI 上下文快照（Issue AI context snapshot）**：
一次 Issue AI 运行在固定截止点内，从所属项目中与该 Issue 相关的数据聚合并裁剪出的输入集合，同时记录各类证据的纳入、省略及原因。其中内容仅是待分析数据、不具有指令权威；同一运行的重试共享该上下文，终态后的新运行拥有新的截止点。
_避免使用_：Issue AI 证据白名单、实时全量遥测、MCP 查询会话

**Issue AI 调用记录（Issue AI invocation record）**：
一次模型调用实际交换的应用层内容，包括指令、消息、上下文、公开模型参数与响应，以及解析结果；它不包含凭据或 HTTP 鉴权信息。
_避免使用_：重建 prompt、Provider HTTP dump、AI 配置

**Issue AI 分析任务（Issue AI analysis job）**：
由 `new_issue` 生命周期事件或有权限成员的手工意图触发、可独立恢复并拥有 pending、running、succeeded、failed 或 cancelled 状态的可选持久分析运行；它不属于错误 Inbox 的完成条件。同一 Issue 最多存在一个活动任务，自动触发和手工触发复用该任务，终态后的手工刷新才创建新运行。
_避免使用_：Issue 生命周期事件、错误 Inbox receipt

**Issue AI 自动分析策略（Issue AI automatic-analysis policy）**：
由 Admin 按项目管理、决定未来新 Issue 是否自动创建分析任务的显式选择，默认关闭并受全局 AI 总闸约束。它不改变已经接管的分析任务，也不代表对关闭期间 Issue 的补分析；全局 AI 可用时，有权限的成员仍可手工创建分析运行。
_避免使用_：全局 AI 启用、手工分析权限

## 数据投递

**持久接管（Durable acceptance）**：
Gateway 对平台异步工作的成功响应边界：有效载荷（payload）使用相同持久数据重启后仍然存在，并继续适用有界处理策略；它不表示数据已经可查询、不表示恰好处理一次，也不保证重试预算耗尽后仍能成功。
_避免使用_：已索引、已完全处理、保证投递
