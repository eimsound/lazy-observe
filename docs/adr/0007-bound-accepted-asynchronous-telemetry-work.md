# 使用持久收件箱（durable Inbox）约束已接管的异步遥测工作

对于无法在一次请求内原子完成、且由平台负责的遥测工作，Gateway 必须在返回 `2xx` 前将其写入有容量上限的持久收件箱；首个适用场景是原始错误存储与 Issue 派生。Worker 领取条目，执行产品层幂等派生，并采用指数退避：总计最多尝试 20 次，从 1 秒开始，最长间隔为 5 分钟。由此，已被平台持久接管（Durable acceptance）的工作可以在使用相同数据目录重启后继续处理，同时无须在 OTLP 存储前再建立第二层队列。重试预算耗尽时，只有先通过诊断和指标暴露终态失败与丢弃事实，才可以删除 payload。

同步端点仍然只有在其所属存储确认写入后才能返回成功。收件箱达到容量上限时，应使用可重试的非成功响应拒绝新工作，绝不能驱逐已经接管的工作。本决策选择有界的至少一次处理（at-least-once），不选择无限保留、恰好一次投递（exactly-once）、跨存储同步写入，也不通过扫描 VictoriaLogs 重建 Issue。

错误摄取以单个错误事件作为独立收件箱条目，但同一 HTTP batch 的全部条目必须在一个元数据事务中全数接管或全数拒绝。`(project_id, event_id)` 是产品幂等身份：相同内容的重投复用首次结果；同一身份携带不同内容时保留首次接管的内容，拒绝冲突重投并记录不含 payload 的诊断。

持久接管同时记录 canonical payload digest、服务端 `accepted_at`、经过时钟漂移修正和合法性校验的 `occurred_at`，以及不可变的 Source Map 制品快照。制品快照必须指向内容寻址的 revision/digest，或者显式记录接管时没有匹配制品；Worker 重试不得读取按 release/filename 后来覆盖的当前 Source Map，后来上传制品也不追溯旧事件。仍被 pending work 引用的 Source Map blob 不得由 retention 清理。

Worker 使用该快照把规范化、符号化的原始错误事件写入 VictoriaLogs，再在一个元数据事务中幂等更新 Issue、记录稳定的 Issue 生命周期事件并完成收件箱 receipt。Issue 状态修改记录服务端 `status_changed_at`；resolved Issue 只会被 `accepted_at` 严格晚于该状态时间的事件重新打开，较早已接管但延迟处理的 backlog 仍更新幂等计数而不产生 regression，ignored Issue 不自动回归。

完成后的 receipt 删除 payload，但从 `completed_at` 起按全局 Logs retention 保留 `(project_id, event_id)`、canonical digest 和结果；该窗口内相同重投复用首次结果、冲突重投被拒绝，清理后同一 event ID 可以作为新事件使用。VictoriaLogs 与元数据没有共同事务，因此故障恢复可能留下同一错误事件的多份物理日志；所有面向用户或其他产品行为的投影必须在 receipt 窗口内按 `(project_id, event_id)` 去重。

产品错误率和趋势按规范化 `occurred_at` 归属；摄取流量、容量和拒绝率按 `accepted_at` 归属；Worker `processed_at` 只用于延迟和积压诊断。告警引擎从持久 Issue 生命周期事件进入既有状态与 notification Outbox 流水线。任何可选下游功能都不属于错误收件箱的完成条件。

服务端重试领域共用一个纯退避时长计算函数。它接收从零开始的重试索引、初始值、上限、位于 `[0,1]` 的抖动样本，以及可选的已解析服务端延迟及其独立上限；只执行溢出安全的时长计算，不访问随机源或时钟、不休眠、不解析 HTTP 请求头，也不拥有任何调用方的预算、领取或持久化状态。各领域共用算法，但分别传入策略并拥有自己的状态。
