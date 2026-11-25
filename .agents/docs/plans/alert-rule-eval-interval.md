# 告警规则评估间隔

来源：Project #4 卡片「添加告警设置间隔时间」（`PVTI_lADOBIRX_M4BtBM8zhLyEYg`）。

## 目标与完成条件

- 目标：原生告警规则可单独设置评估间隔；留空沿用按信号的默认值（metrics 5s、logs 1m）。
- 完成条件：`mise run test`、`mise run test-meta-pg`、`mise run web`、`mise run e2e-ha` 全绿；
  `POST /api/alert-rules` 带 `evaluation_interval` 后评估器按该间隔调度。

In scope：schema/双基线、meta 持久化、服务校验、评估器调度、网关 DTO、Web 表单与列表、`docs/version-matrix.md`。
Out of scope：全局默认间隔配置键、`group_wait` 改由 outbox 延迟投递、存量表达式提示文字收口（DESIGN.md Known Gaps）。

## 决策

两条备选由 Claude 与 Orchestrator (Neon) 各自独立提出后逐条对比，结论一致的部分直接采用。

| 维度 | 采用 | 拒绝及理由 |
| --- | --- | --- |
| 粒度 | 仅规则级可选覆盖，默认值仍在 `rule_evaluator.go` | 全局 per-signal 配置键：HA 下各副本配置不一致会让评估节奏随 owner 漂移；`member` 也改不了服务端配置。以后需要时只需接线，不动 schema/API |
| 边界 | 5s 整数倍；metrics 5s–1h，logs 30s–1h | 任意整秒：7s 会被 5s tick 悄悄变成 10s；上界 24h：首次 firing 通知要等下一轮评估，最坏延迟过大 |
| 存储 | `evaluation_interval TEXT NOT NULL DEFAULT ''`，存规范化 Prometheus 时长 | NULL 列：全链路多一层指针；整数秒：与同表 `for`、`scrape_targets.interval` 的单位体系分裂；始终存具体值：切换信号会撞下界、无法区分默认与覆盖 |
| PATCH 语义 | 省略键 = 不变，`""`/`null` = 恢复默认（网关 `optionalString`） | 直接 `*string`：JSON `null` 会被静默当成「不变」；哨兵值 `default`/`0s`：易混淆 |
| 响应 | `evaluation_interval` + 只读 `effective_evaluation_interval`（与评估器同源） | 只返回覆盖值：列表须在前端复制默认值才能显示实际间隔 |
| 调度 | 周期起点采样 `cycleNow`；`lastDue + ruleInterval(fresh)` 每 tick 按重载规则重算；`min(tick/10, 250ms)` 容差；首次 firing 未通知时按 `firing_since + group_wait` 追加一次唤醒 | 内存 `next` 固定到期时间：其他副本改间隔后最长卡 1h；在 `UpdateAlertRule` 推进纪元清缓存：浪费在途查询并污染租约丢失指标 |
| UI | 表单第 2 行「信号类型｜评估间隔｜持续时间」三等分；placeholder「默认 5s/1m」随信号联动；提交前本地校验（格式/范围两条报错）；列表新增「间隔」列，默认值弱化并以 Tooltip 标注 | 字段下方说明或常驻提示（DESIGN.md 禁止）；预设下拉（限制自定义值） |

## 执行日志

- 2026-11-25 后端由协作者在独立 worktree 实现（`8f9326c`），前端与文档在本分支实现；合并后 golines 无改动
- 2026-11-25 `mise run vet` → 通过
- 2026-11-25 `mise run test` → 通过（`internal/alert`、`internal/gateway`、`internal/meta` 均 ok）
- 2026-11-25 `mise run lint` → 通过（0 issues，alert lease clock 检查通过）
- 2026-11-25 `mise run fmt-check` → 通过
- 2026-11-25 `mise run ent-generate-check` → 通过
- 2026-11-25 `mise run migration-check` → 通过
- 2026-11-25 `mise run test-meta-pg` → 通过
- 2026-11-25 `mise run e2e-contract-check` → 通过
- 2026-11-25 `mise run web` → 通过（vitest 300/300，Playwright 48/48，JS 许可证检查通过）
- 2026-11-25 `mise run e2e-ha` → 通过（19 条 PASS，含「两个评估候选只查询一次原生规则并只提交一条通知」）
- 2026-11-25 rebase 到 `origin/main`（`71e3ccb`，仅 CI/文档改动）后压成单个提交
