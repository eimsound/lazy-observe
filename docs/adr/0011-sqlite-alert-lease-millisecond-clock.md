# SQLite 下告警租约统一走毫秒时钟入口

SQLite 的 `CURRENT_TIMESTAMP` 和 `datetime()` 只有整秒精度：`datetime('now', '+1.000000 seconds')` 在 `hh:mm:23.9` 求值得到 `hh:mm:24`，而不是 `hh:mm:24.9`。告警引擎的成员身份和规则租约（ADR 0001）用数据库时间判定过期，于是 SQLite 下每个 TTL 实际只有 `TTL - frac(获取时刻)`，短 TTL 的租约可能在一次评估中途就过期，提交被隔离纪元拒绝（`ErrStaleRuleFence`）。PostgreSQL 的 `CURRENT_TIMESTAMP` 是微秒精度，没有这个问题。我们决定：凡是读写 `alert_engine_members`、`alert_rule_leases` 且用到数据库当前时间的查询，都只通过 `internal/meta/alert_engine.go` 里的两个入口取时钟：

- `(*Store).alertLeaseExpiry(ttl)`：生成到期时间表达式和参数。SQLite 用 `strftime('%Y-%m-%d %H:%M:%f', 'now', ?)`，PostgreSQL 用 `CURRENT_TIMESTAMP + (? * INTERVAL '1 millisecond')`。
- `(*Store).alertClockSQL(query)`：代替 `bind(s.dialect, query)`。查询里照常写 `CURRENT_TIMESTAMP`，SQLite 下它被替换成 `sqliteAlertClock`（`strftime('%Y-%m-%d %H:%M:%f', 'now')`），PostgreSQL 下原样保留。

不要在这两张表的查询里手写 `datetime('now', ...)`，也不要把带 `CURRENT_TIMESTAMP` 的查询直接交给 `bind`。其它文件里校验租约的查询也在此列，例如 `internal/meta/native_alert.go` 中 `CommitNativeAlertEvaluation` 的租约锁查询。

## 备选方案

- **SQL 层统一毫秒时钟（采用）**：过期判定仍以数据库时间为准，与 ADR 0001「基于数据库时间的过期」一致；不改 schema，两种方言共用同一份查询文本。代价是 SQLite 下这两张表的时间列写成带毫秒的文本，与其它表的整秒文本格式不同。
- **由 Go 侧传入 `time.Now()` 作为时钟（拒绝）**：多副本时各进程时钟不一致，会破坏租约互斥；SQLite 虽然只支持单引擎拓扑，但两种方言各走一套时钟来源会让同一份租约逻辑出现两种语义。
- **规定 TTL 不得小于若干秒来掩盖截断（拒绝）**：误差仍在，只是变得更难复现；故障转移时间和测试用的短 TTL 都会被这条隐性下限绑住。

## 后果

- SQLite 里整秒文本和毫秒文本按字符串比较：`hh:mm:24` 排在 `hh:mm:24.000` 之前，所以旧的整秒行只会被判定为不晚于原先的时刻过期，不会被延长。
- 这两张表以外的时间列仍是整秒精度，包括 `alert_rules.updated_at`、`alert_events.updated_at`，以及 `notification_outbox` 的 `available_at`/`claim_until`（`internal/meta/notification_outbox.go` 仍用 `datetime('now', ?)`）。发件箱的 claim 时长因此在 SQLite 下最多短一秒；它靠幂等键保证至少一次投递，本 ADR 不改变它。要给别的表引入亚秒 TTL 时，先扩展这里的入口，不要各写各的 `strftime`。
- 新增一种基于数据库时间的租约或成员表时，把它纳入同一入口，并在本 ADR 的表清单里补上。
