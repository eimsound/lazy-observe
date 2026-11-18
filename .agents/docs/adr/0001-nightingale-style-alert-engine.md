---
status: accepted
---

# 使用 Nightingale 风格的单所有者（single-owner）告警引擎

已接受的目标是由 lazy-observe 自身拥有告警引擎：每条规则只选择一个所有者，并且只有持有当前隔离纪元（fencing epoch）的所有者才能持久化告警状态或创建持久通知工作。本方案以 Nightingale 的规则分片模型作为架构参考，同时补足其进程内状态和队列的缺口；不会把 Nightingale 作为服务或代码依赖引入。

该迁移已通过 N5b 完成。当前主分支在进程内评估定时规则，通过元数据数据库协调所有权与通知工作，并且不再启动或嵌入 `vmalert`。不可变的 v1.0 需求基线继续作为历史背景；本 ADR 只取代其中关于 `vmalert` 和重复评估的实现选择，不改变告警、分组、静默、抑制、高可用或通知目标。

## 决策

- 告警引擎由同一个 lazy-observe 二进制拥有。All-in-One 和单节点 Standalone 在进程内运行一个引擎候选者；高可用部署中的每个应用副本都运行一个候选者。Rendezvous 分配与带隔离令牌的数据库租约（fenced database lease）共同保证：应用副本数量变化时，同一规则仍然最多只有一个能够提交结果的评估者。
- 引擎实例在 PostgreSQL 中发布成员身份。系统对 `(project_id, signal, rule_id)` 做确定性一致性哈希，以选择候选所有者；该设计沿用 Nightingale 的分片形态和当前 `AlertRule` 模型。如果以后引入用户自定义数据源，其稳定键再扩展到所有权键中；当前不假设不存在的 `datasource_id`。
- 候选者还必须获取并续租该规则的 PostgreSQL 租约。每次获取都会推进隔离纪元；只有携带当前纪元的写入才能更新告警状态或将通知加入队列。故障转移时，旧所有者可以完成进行中的存储查询，但其过期纪元无法提交结果。一致性哈希负责均衡工作，带隔离令牌的租约负责保证副作用不会发生脑裂。
- 规则定义以及故障转移所需的 pending、firing、recovered、repeat、分组和抑制状态都保存在元数据数据库中。旧租约过期后，新所有者从持久状态继续，而不是把每条规则都视为新规则。
- 一次评估事务同时推进状态并插入通知发件箱（notification Outbox）记录。分发 Worker 使用数据库锁领取到期记录，按退避策略重试，并为每次逻辑投递使用稳定的幂等键。因此外部渠道得到持久的至少一次投递（at-least-once）；对于不支持幂等键的接收方，不承诺恰好一次（exactly-once）。
- Redis 可以加速缓存、限流或唤醒，但 Redis 丢失后，成员身份、所有权、告警状态和未发送通知仍必须保持正确。
- 引擎通过现有的项目隔离存储边界评估 MetricsQL/PromQL 和 LogsQL 规则，并自行实现项目中受 Alertmanager 启发的分组、静默与抑制语义。目标生产路径不要求部署 `vmalert` 或 Prometheus Alertmanager。
- 由 Gateway 产生的平台事件（例如新 Issue 和回归）不需要定时规则所有权。它们使用稳定事件 ID 进入同一条持久告警状态与发件箱流水线，使所有告警类型共享分组、抑制、重试和投递历史，而不是再生长出第二套通知器。
- SQLite 只支持单引擎拓扑。多副本告警要求 PostgreSQL，因为规则租约、隔离纪元和发件箱属于共享状态。

## 迁移方案

切换分阶段进行：先增加持久状态和发件箱表；然后让新引擎以影子模式运行但不发送通知；再把结果与 `vmalert` 比较；之后按规则分组逐批切换到单所有者引擎；最后删除 `vmalert` 的监督、嵌入产物、内部 webhook 兼容路径，以及仅用于重复评估正确性的 Redis 键。任何阶段都不允许两个引擎为同一组规则发送通知。

## 实施顺序

1. 先增加所有权内核，不改变运行时行为：规范化规则键、稳定的引擎成员 ID、确定性哈希，以及证明成员顺序无关性和成员加入/离开时有限迁移量的黄金测试。
2. 增加成员身份、规则租约/隔离、持久告警状态和通知发件箱表。通过基于数据库时间的测试实现租约获取、续租、过期和带隔离令牌的写入。
3. 将现有渠道分发移到发件箱 Worker 后。使用 PostgreSQL 行锁领取工作，持久化尝试次数和下次重试时间，并携带稳定的投递幂等键。在迁移定时规则评估前，先让平台事件进入该路径。
4. 以影子模式实现原生评估器。它可以查询存储并持久化比较结果，但不得修改权威告警状态或发送通知。用录制 fixture 和高可用浸泡测试比较它与 `vmalert` 的 pending/firing/recovered 转换和分组结果。
5. 将明确的规则分组切换到原生引擎。每组规则只能有一个通知权威；必须先启用租约与隔离，再禁用对应的 `vmalert` 规则。回滚时反向切换所有权，绝不能双发。
6. 只有所有规则组都通过故障转移、Redis 丢失、进程终止和通知重试门禁后，才能删除 `vmalert`、其嵌入产物、内部 webhook 兼容路径。All-in-One 与高可用 Helm 角色必须在同一个版本中同步更新。

N3 版本先把步骤 1 实现为纯 [`ownership`](../../../internal/alert/ownership) 包。它验证当前 `(project_id, signal, rule_id)` 领域键，使用稳定成员 ID 和带领域分隔的 Rendezvous 哈希，并通过黄金测试和成员迁移测试固定分配契约。该包刻意不包含数据库或运行时集成，因此在后续带隔离租约的阶段完成前，`vmalert` 行为保持不变。

持久协调基础提供活跃成员心跳、基于数据库时间的过期、原子的逐规则租约获取/续租、租约过期或所有者变化后单调递增的纪元，以及受当前未过期纪元保护的 pending/firing/recovered 状态写入。PostgreSQL 测试覆盖候选者并发和过期所有者拒绝；在当时的历史阶段，还没有评估器使用这些表。

持久队列原语使用稳定幂等键抑制重复逻辑工作。PostgreSQL Worker 通过 `FOR UPDATE SKIP LOCKED` 领取互不重叠的到期记录；Worker 崩溃后，过期 claim 可以恢复；失败投递经过有界重试后进入保留的死信状态。在 N4b2 之前，平台事件和定时告警路径都不会写入该队列。

N4b2 版本把经过分组、静默和抑制的平台事件通知创建迁移到持久队列。每个 Gateway 使用唯一 claim owner 启动一个 Worker；Worker 通过元数据数据库竞争，在配置 Redis 时复用共享渠道限流器，按有界指数退避重试失败发送，并保留已恢复通知的优先级。HTTP 投递使用发件箱键作为 `Idempotency-Key`。投递仍是至少一次，因为接收方接受请求后、数据库提交完成前，Worker 仍可能停止；因此恰好一次的效果要求接收方支持该幂等键。定时指标/日志规则和风暴摘要在完成带隔离的 N5 规则组切换前仍沿用旧路径。

N4c 版本在每个 Gateway 中启动一个原生评估器候选者，并以影子模式运行。活跃成员使用 N3 Rendezvous 契约为每条规则提名一个查询所有者；该候选者必须持有 N4a 带隔离令牌的租约，才能写入候选的 pending/firing/recovered 状态。指标规则使用现有 Prometheus/VictoriaMetrics 项目过滤边界；LogsQL 规则使用原生 VictoriaLogs 租户请求头和兼容 Prometheus 的 `stats_query` 响应。影子比较表为每条规则保留最新的原生结果与 `vmalert` firing fingerprint 对比。影子评估不会加入通知队列，也不会调用渠道发送器，因此在该历史阶段，`vmalert` 仍是定时规则的唯一通知权威。Gateway 内的影子 Worker 在 N5 成为生产评估器候选者；规则所有权与隔离机制，而非单独部署的角色，将规则权威与普通 API 请求处理区分开。

N5a 版本引入明确的逐规则通知权威。规则组切换必须经过 `vmalert -> paused -> native`（或反向）：进入 `paused` 时，先撤销带隔离令牌的租约并重新加载规则文件，再激活目标引擎。一次原生评估同时锁定规则权威和租约，然后在一个事务中推进持久状态、更新用户可见的告警事件并插入渠道发件箱工作。原生分组遵守 group-wait、repeat 时间、已恢复优先级、静默和抑制。N5b 迁移剩余规则组并删除旧运行时时，规则 API 仍保留该过渡字段。

N5b 版本完成到 `native` 评估的迁移。API 与 Ent schema 不再接受 `vmalert`；规则文件生成、重新加载信号、内部数据源/webhook 路由、仅供重复评估使用的 Redis 键、监督逻辑以及四个平台的 `vmalert-prod` 嵌入产物均已删除。发布产物现在只包含 VictoriaMetrics、VictoriaLogs、VictoriaTraces 和 vmagent。Redis 仅作为共享 session 和限流的可选组件；告警所有权、带隔离保护的状态及未发送通知在共享 PostgreSQL 元数据数据库中仍保持正确。在发布前阶段，依据 ADR 0003，所有这些表都直接体现在当前单一数据库基线中。

## 后果

故障转移时间受成员和租约过期时间限制，因此这些间隔成为明确的可用性参数。该设计消除了稳定状态下的重复高可用查询，并使通知工作可恢复。故障转移窗口内可以存在重叠查询，但隔离机制必须阻止重复推进状态和重复创建通知。该设计新增了租约、隔离、状态机和发件箱持久化，因此需要故障注入和多副本测试。

## 参考资料

- Nightingale 的[官方架构指南](https://n9e.github.io/zh/docs/prologue/architecture/)说明，告警规则会被自动分配，使一条规则只在一个实例上运行，并在实例故障后由另一个实例接管。
- Nightingale 会为每个数据源重建一致性哈希环，并在 [`alert/naming/hashring.go`](https://github.com/ccfos/nightingale/blob/6bc533764a7a0f85d85b324790c1c78224f7acde/alert/naming/hashring.go#L12-L62) 中判断一条规则是否属于当前节点。
- Nightingale 在 [`alert/naming/heartbeat.go`](https://github.com/ccfos/nightingale/blob/6bc533764a7a0f85d85b324790c1c78224f7acde/alert/naming/heartbeat.go#L70-L191) 中记录引擎心跳、把 30 秒内出现的实例视为活跃，并在成员变化时重建哈希环。
- 它的评估和通知流水线分别位于 [`alert/eval`](https://github.com/ccfos/nightingale/tree/6bc533764a7a0f85d85b324790c1c78224f7acde/alert/eval) 与 [`alert/dispatch`](https://github.com/ccfos/nightingale/tree/6bc533764a7a0f85d85b324790c1c78224f7acde/alert/dispatch)。
- Nightingale 会恢复当前 firing 事件，但事件队列仍保存在进程内，参见 [`alert/process/process.go`](https://github.com/ccfos/nightingale/blob/6bc533764a7a0f85d85b324790c1c78224f7acde/alert/process/process.go#L560-L604) 与 [`alert/queue/queue.go`](https://github.com/ccfos/nightingale/blob/6bc533764a7a0f85d85b324790c1c78224f7acde/alert/queue/queue.go#L10-L17)；上文的持久状态/发件箱要求是 lazy-observe 的加固措施。
- 当前 lazy-observe 规则所有权键以 [`AlertRule`](../../../internal/db/schema/alertrule.go) 为依据；它包含 `project_id`、`signal` 和 Ent 规则 ID，但没有数据源 ID。
