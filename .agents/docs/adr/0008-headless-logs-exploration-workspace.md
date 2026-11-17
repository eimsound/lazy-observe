# 保持 Logs exploration workspace 无 React 依赖

Logs 的历史查询与 Live tail 共享 Project、LogsQL、过滤条件、执行切换和取消规则，因此由同一个 deep Logs exploration workspace 作为两种互斥模式拥有；该 module 只包含探索行为核心，不吸收列宽、抽屉或文本渲染等视觉偏好。它采用无 React 依赖的 TypeScript interface，通过状态快照和行为命令连接薄 React adapter，而不实现为直接依赖 React Query 与 Router 的自定义 hook；这样虽然需要明确的内部 seam，却能用现有 `node:test` 穿过同一 interface 验证异步时序，避免规则重新散落到 hooks。

有限历史查询继续由 React adapter 使用 React Query 执行，但查询的启用条件、`QueryPlan`、execution identity 和结果接纳规则归 workspace 所有，迟到的旧 identity 结果不得覆盖当前快照。Live stream 不进入 React Query；workspace 通过注入的 stream 与 clock port 拥有连接、取消、重连、去重、暂停和有界缓冲生命周期。

Logs URL 是可分享探索意图的规范序列化形式，而不是运行结果存储。workspace 拥有其行为字段的解析、规范化、替换和外部导航接纳规则；薄 Router adapter 只传递 location change 与执行 replace，视觉层拥有的参数作为未知参数原样保留。浏览器外部导航替换当前意图并取消过期工作，workspace 自己产生的 URL 更新不得形成重复执行循环。

切换 Project 时保留 LogsQL、时间范围、过滤条件和 Historical/Live 模式意图，取消旧 Project 的有限查询或 Live stream，清空结果与瞬态状态，并在新 Project 下按当前执行意图重新执行；查询历史按 Project 隔离。Reset 只重置 workspace 的行为状态：LogsQL `*`、相对一小时、无过滤、limit 50、Historical、关闭自动刷新、未执行并清空结果；Reset 不自动查询，且保留查询历史、收藏和纯视觉偏好，同时规范化 URL 并保留视觉层参数。

workspace 的公开 interface 采用状态机形态，只提供只读快照、订阅、类型化事件输入和销毁；它不暴露逐字段 setter，也不把 historical、Live 与 URL 拆成需要 caller 重新组合的多个 controller。React adapter 的用户意图以及 React Query 的带 identity 结果都作为可判定的事件进入同一 interface。

取消有限历史查询只终止当前 execution identity，不停止 Live。Live 只会因退出 Live 模式、Project 或外部 location 改变、workspace 销毁或用户显式停止而关闭。用户显式停止 Live 时不在后台接收或缓冲；滚动离开底部只关闭纯视觉层的自动跟随，不影响网络流或 workspace 生命周期。

Live 只对网络错误、超时、HTTP 429 和 5xx 自动重连；无效查询、401 和 403 是 terminal error，直到探索意图改变或用户显式重试。自动重连使用带 jitter 的指数退避，从约一秒开始、上限 30 秒，在仍处于 Live 模式时不限制次数，并在成功连接后重置退避。

每次有限历史查询开始执行时，包括首次执行、查询条件变化、手动刷新和自动刷新，workspace 都立即清空上一批结果并进入 loading 状态；界面用明确的 loading 元素呈现该状态，不在新执行期间继续展示旧数据。取消只终止当前 execution identity，不恢复已清空的上一批结果；迟到结果仍按 identity 拒绝。

一次有限历史执行中的主日志列表与命中数、统计信息、时间范围等辅助结果分别记录状态和接纳结果。主列表成功即可展示日志，辅助失败只影响对应区域并允许局部重试，不能把已经成功的主列表降级为整体失败。

Historical 与 Live 不保留供模式切换时恢复的旧结果缓存。切换模式立即清空当前可见结果；进入 Historical 创建新的 execution identity 并展示 loading，进入 Live 则重新连接并从当前时间对应的配置窗口接收数据。

LogsQL 编辑器中的内容是 draft。仅修改 draft 不清空或重跑当前 Historical 结果；结果始终对应最近一次 executed snapshot。点击 Run 或切换 Historical/Live 模式才把当前 draft 固化为新的 executed snapshot，清空旧结果，并创建新的 execution 或 Live connection identity。

自动刷新 tick 到达而当前有限历史 execution 仍在 loading 时，workspace 跳过该 tick，不取消当前执行、不排队，也不并行创建 execution；当前执行完成后再恢复正常刷新周期。

当用户修改 LogsQL draft 时，workspace 立即把 URL 中的查询更新为 draft，并把执行标记规范化为 `run=false`；刷新或分享该 URL 会恢复 draft，但不会自动执行。用户显式点击 Run，或携带新 draft 切换模式时才写入 Project-scoped 查询历史；自动刷新、辅助结果重试、Live 自动重连以及停止后恢复不重复写入历史。

显式停止 Live 会关闭网络流但保留已接收行供检查。恢复 Live 时，workspace 清空旧行、进入 connecting 状态，并按当前时间和配置窗口重新连接；恢复前后的行不混合，也不显示空档提示。

外部 URL 中非法的结构参数按既定默认值和上下限规范化，并由 Router adapter replace 为规范 URL；LogsQL 文本作为 draft 原样保留，不在 URL 解析阶段预判语法，只在执行后接纳查询错误。

有限历史主查询失败时不展示旧结果。Retry 使用失败的 executed snapshot 创建新的 execution identity，清空错误并进入 loading；修改 draft 不触发重试，只有 Run 才执行新 draft。辅助结果的 Retry 只重试对应结果槽。

Live 始终只保留最新 `liveLimit` 行；超过上限时丢弃最旧行并累计 dropped 数。停止时保留当前行和计数供检查，恢复时把行、dropped 计数和去重状态一起清零，再建立新连接。
