# Issue AI 持久分析实施计划

> 状态：后续实施，不属于核心可观测性闭环预生产质量门禁。

## 目标与来源

- 源规格：[Issue AI 持久分析 PRD](../specs/issue-ai-analysis.md)
- 架构决策：[ADR 0009](../adr/0009-run-issue-ai-as-durable-jobs.md)
- 领域词汇：[GLOSSARY.md](../GLOSSARY.md)
- 前置能力：核心可观测性闭环预生产质量轮（计划已完成，存于私有归档）产出的持久 Issue 生命周期事实、稳定错误身份和跨信号关联。
- 最终状态：自动和手工 Issue AI 意图都进入可恢复的持久任务；HTTP 触发不等待模型；固定上下文、准确调用记录、预算、租约、接管纪元、取消、流式历史和权限可通过公开接口验证；AI 成败不影响 Issue、告警或错误摄取。
- 范围外：把 Issue AI 纳入核心预生产质量验收、Standalone 独立 Worker、多应用副本容量语义、MCP、写工具、货币成本、任务级 token 汇总、WebSocket、旧同步接口或旧发布前数据库兼容路径。

## 仓库证据

- `internal/gateway/issue_ai_analysis.go` 当前使用容量 128 的进程内 channel；`HandleIssueChange` 入队，`run`/`process` 直接调用模型，队满、进程退出和调用失败只降级为日志。
- 同一文件的 `postIssueAIAnalysis` 当前同步执行 `generateAndStoreIssueAI`；`issueAIContextBudget`、超时、项目限流和上下文裁剪是编译期常量，尚无持久任务、尝试、租约或调用历史。
- `internal/gateway/issue_ai_analysis_test.go` 已覆盖输出解析、24 KiB 裁剪、项目限流、自动分析不阻塞摄取和同步生成行为，可作为迁移外部契约的起点。
- `internal/meta/ai_settings.go`、`internal/db/schema/aisetting.go`、`internal/gateway/ai_settings.go` 和 `web/src/routes/ai-settings-page.tsx` 已提供单行 AI 设置、管理 API 和设置 UI。
- `internal/meta/issues.go` 与 `internal/meta/issues_test.go` 已提供双方言 Issue 行为；`internal/meta/notification_outbox.go` 及其测试展示了 SQLite/PostgreSQL 的持久领取、过期恢复和条件结算模式，但通知领域的表和预算不能复用。
- 旧告警路径遗留的 `LeaseLocker`/Redis lease 已因无生产调用且缺少续租语义而删除；Issue AI 必须按任务接管语义新建 owner-safe 续租 seam 与真实内存 TTL 测试实现。
- `internal/ai/provider.go`、`client.go` 和 `stream.go` 已封装 Provider 调用与流；`web/src/components/ai/issue-ai-analysis-card.tsx`、`web/src/lib/api/issues.ts` 和 Issue 页面已提供展示入口。
- Ent schema 位于 `internal/db/schema`；双方言可变基线位于 `internal/meta/migrations/001_baseline.sql` 与 `internal/meta/migrations/postgres/001_baseline.sql`；生成的 `internal/db/ent` 必须保持不被跟踪。
- 已核实的门禁包括 `mise run test`、`test-meta-pg`、`migration-check`、`ent-generate-check`、`fmt-check`、`vet`、`lint`、`web` 和 `e2e`。
- 制定本计划时只运行了只读检索、文件阅读和文档检查，没有运行 Go、Web 或 E2E 测试。

## 实施策略

- **推荐路线：深 `issueai` 模块 + 元数据持久任务 + 单次请求 Worker。** 新模块拥有任务策略、上下文、尝试、重试、租约和结算；Gateway 只处理 HTTP/权限/响应映射，metadata implementation 拥有 Ent、事务和方言，`internal/ai` 只拥有 Provider 协议。该路线使 AI 可以独立删除或停用而不破坏 Issue/告警主链，并让模型请求与持久状态分别在稳定接缝测试。
- **备选路线：扩展现有 `issueAIService` channel 并把队列落盘。** 改动较少，但 Gateway 仍同时拥有调度、上下文、Provider、重试和 HTTP，手工/自动路径继续分叉；租约和条件结算会进一步加深宽 Gateway，因此拒绝。
- **备选路线：复用错误 Inbox 或通知 Outbox。** 可减少表数量，但错误 Inbox 的 20 次预算、payload 清理和 `2xx` 语义，以及通知 Outbox 的渠道/死信语义，都与模型成本、调用审计、固定上下文、取消和最近成功投影不同；只复用双方言事务与领取测试模式，不共用表或领域接口。

## 已确认的测试接缝

- Gateway HTTP：证明手工请求权限、限流、审计、`Idempotency-Key`、统一 `202`、`Location`、状态读取以及全局/项目开关。
- `issueai` 行为接口：使用真实 SQLite metadata adapter、假时钟、可阻塞假 Provider 和内存租约证明任务、上下文、预算、领取、取消与结算；不模拟 Ent builder。
- metadata 行为：复用 `forEachDialect` 证明单活动任务、条件纪元写、原子快照、尝试事件、历史清理和双方言错误映射。
- `internal/ai` 单次调用接口：证明 DeepSeek 请求映射、流事件、Provider 错误事实和响应上限，不读取任务或执行重试。
- Issue UI 与 SSE：证明最近成功结果、当前运行、流式重放、终态诊断、权限和秘密排除。
- 真实产品：在完成前述契约后，使用真实 All-in-One、Issue/Trace/log fixture、嵌入式 Web 和 Chromium 验证完整手工与自动路径；该旅程是本后续功能自己的门禁，不加入核心预生产计划。

## 依赖顺序

1. 持久任务和异步 HTTP（步骤 1）建立所有后续工作的权威身份与状态边界。
2. 固定上下文与调用记录（步骤 2）依赖任务身份；Provider 设置与错误策略（步骤 3）依赖可审计尝试。
3. 单活动运行、自动策略和权限（步骤 4）依赖步骤 1–3 的统一任务入口。
4. 租约与接管纪元（步骤 5）依赖稳定任务/尝试事务；流式历史和结算背压（步骤 6）依赖步骤 3、5。
5. 取消、保留期和诊断（步骤 7）依赖完整终态与运行历史；最终旅程（步骤 8）依赖全部行为。

## 实施步骤

- [ ] 1. 用持久任务替代进程内队列和同步手工调用
  - 交付：自动 `new_issue` 与手工 POST 都创建或复用 `pending` 任务；手工接口验证 `Idempotency-Key` 后统一返回带状态资源的 `202`，请求内不调用模型；任务失败不改变 Issue、告警或错误 Inbox 完成。
  - 阻塞于：核心预生产计划已经提供持久 `new_issue` 生命周期事实。
  - 涉及位置：新增 `internal/issueai` 行为模块；`internal/gateway/issue_ai_analysis.go`/tests；`internal/meta` Issue AI behavior；`internal/db/schema` 新任务/尝试/幂等键 schema；双方言 `001_baseline.sql`；不编辑 `internal/db/ent`。
  - Red：增加 Gateway 契约，使用阻塞假 Provider 断言 POST 在 Provider 返回前已经得到 `202`、`Location` 和稳定 `job_id`；缺少幂等键被拒绝；重启 metadata 后任务仍为 `pending`。运行 `mise exec -- env CGO_ENABLED=0 go test ./internal/gateway ./internal/issueai ./internal/meta -run 'TestIssueAI.*(Accepted|Job|Idempotency)' -count=1`，预期当前同步路径和无持久表而失败。
  - Green：以扩展→迁移→收缩增加行为接口和双方言基线，先让假执行器领取一个任务并保存终态；迁移自动/手工调用方后删除 channel、同步模型快路径和旧编排。发布前不增加兼容 adapter。
  - 验证：运行聚焦测试、`mise run test-meta-pg`、`mise run migration-check`、`mise run ent-generate-check`。
  - 验收：进程退出不丢失已接管任务；删除 AI 模块不会使 Issue 摄取或告警测试失败。

- [ ] 2. 固定完整上下文并保存准确调用记录
  - 交付：任务首次执行时原子保存 `context_cutoff_at`、完整不可变上下文、digest 和 omission manifest；每次尝试在请求前保存准确 invocation，在响应后保存公开结果、usage、Provider 请求 ID 和解析结果；重试复用快照。
  - 阻塞于：步骤 1。
  - 涉及位置：`internal/issueai` 上下文聚合器、`internal/meta` 快照/调用记录 behavior、现有 `issueAIEvents`/Trace/Logs/Metrics 查询适配、Issue AI 读取 API/UI。
  - Red：构造超过 24 KiB 的同项目混合证据，断言固定优先级、逐类计数/字节/原因、重试不吸收 cutoff 后事件、提交失败不留下半份快照；断言凭据和传输头不在任何读取结果中。运行聚焦 Go/Web 契约，预期当前即时字符串上下文和单结果字段失败。
  - Green：把聚合器选中的数据确定性序列化并裁剪；以当前任务条件事务一次性写入快照；请求前后追加精确调用记录，保留原始 `usage` 与可空规范化维度，不计算任务汇总或货币成本。
  - 验证：运行 `mise exec -- env CGO_ENABLED=0 go test ./internal/issueai ./internal/meta ./internal/gateway -run 'TestIssueAI.*(Context|Invocation|Usage)' -count=1`、`mise run test-meta-pg`、`mise run web`。
  - 验收：同一任务所有重试使用完全相同的证据快照；读者看到的调用与实际请求一致且不含秘密。

- [ ] 3. 建立类型化 Provider 设置、尝试预算和错误分类
  - 交付：设置页管理重试 `0–5`、上下文预算 4 KiB–1 MiB、Issue AI 专属最大输出/思考/连接与总超时；每个真实请求消耗一次尝试，按规格分类重试或终止；设置在任务/尝试边界正确快照或热读取。
  - 阻塞于：步骤 1–2。
  - 涉及位置：`internal/db/schema/aisetting.go`、`internal/meta/ai_settings.go`、`internal/gateway/ai_settings.go`、`internal/ai/{provider,client,stream}.go`、`web/src/routes/ai-settings-page.tsx`、相关契约/i18n。
  - Red：以表格测试覆盖所有数值边界、默认值、`max_tokens` 省略、DeepSeek `thinking.type`/`reasoning_effort`、408/429/5xx、`insufficient_system_resource`、`length`、`content_filter`、意外工具调用、8 MiB 响应和结构化修复预算。运行 AI settings、Provider 与任务聚焦测试，预期硬编码常量和当前两次解析循环失败。
  - Green：让 Provider 通过共享扩展接口声明强类型字段和请求映射；协调器在尝试开始时读取一次配置并创建持久尝试；复用纯退避计算器，但由任务拥有预算、时钟、等待和状态。
  - 验证：运行 `mise exec -- env CGO_ENABLED=0 go test ./internal/ai ./internal/issueai ./internal/gateway ./internal/meta -run 'Test.*(AI|Provider|Backoff)' -count=1`、`mise run web`。
  - 验收：每个 HTTP 模型请求都有且只有一个持久尝试；配置错误不重试，任何修复调用不绕过预算。

- [ ] 4. 统一单活动运行、权限、自动策略和最近成功发布
  - 交付：重复生命周期、自动与手工意图合并到同一活动任务；终态刷新新建运行；只在新运行成功时替换最近发布分析；项目默认不自动外发，权限、双重限流与审计符合规格。
  - 阻塞于：步骤 1–3。
  - 涉及位置：`internal/issueai` 创建/合并策略、metadata 唯一约束、Gateway GET/POST 与项目设置 API、`web/src/components/ai/issue-ai-analysis-card.tsx`、项目设置卡、i18n。
  - Red：并发投递相同 `new_issue`、相同/不同幂等键和刷新请求，断言最多一个活动任务；覆盖 Viewer/Member/Admin、系统操作者、全局开关、默认关闭项目策略、关闭后继续、重开不回填以及失败刷新保留旧结果。
  - Green：把所有入口汇聚到一个原子 create-or-coalesce behavior；用独立 latest-success pointer 发布结果；Gateway 只映射领域结果，不编排事务顺序。
  - 验证：运行 Gateway/metadata 聚焦测试、`mise run test-meta-pg`、`mise run web`。
  - 验收：项目未选择加入时没有自动模型请求；手工权限不受项目自动开关影响；失败刷新不清空最近成功结果。

- [ ] 5. 用可续租租约和接管纪元恢复任务并拒绝迟到结果
  - 交付：协调器按 FIFO 键集分页读取候选，依次获取缓存租约和数据库条件接管；每次接管递增 `claim_epoch`，续租覆盖上下文、Provider、解析与结算；重启、TTL 到期、Redis/数据库故障和旧所有者返回均有明确结果。
  - 阻塞于：步骤 1–4。
  - 涉及位置：`internal/state` Lease contract/内存/Redis、`internal/issueai` 协调器、metadata claim/settle behavior、Gateway 启动配置与 wiring。
  - Red：用假时钟和可阻塞 Provider 证明 owner-safe 获取/续租/释放、内存 TTL、Redis 原子比较、候选→缓存→数据库顺序、纪元递增、遗留尝试 `lease_lost`、旧纪元写拒绝、30 秒兜底扫描、并发 `1–64` 边界和运行期 Redis fail-closed。
  - Green：扩展 `Lease` 的续租能力并修正内存 TTL；实现主服务协调器和不感知任务的单次请求 Worker；数据库保持权威，缓存只提供活性协调；提交后才唤醒扫描和释放租约。
  - 验证：运行 `mise exec -- env CGO_ENABLED=0 go test ./internal/state ./internal/issueai ./internal/meta ./internal/gateway -run 'Test.*(Lease|Claim|IssueAI)' -count=1`、Redis 契约、`mise run test-meta-pg`。
  - 验收：旧所有者在任何接管时序下都不能发布；领取/上下文准备不消耗尝试，已经发出的未知请求一定消耗尝试。

- [ ] 6. 持久化流事件并在结算故障时施加背压
  - 交付：DeepSeek streaming 的 reasoning/content 按 250 ms 或 16 KiB 批次持久化；SSE 先重放再实时发送；流或结算写库失败时停止拉取/分发并只重试原事务，不重复调用 Provider。
  - 阻塞于：步骤 3、5。
  - 涉及位置：`internal/ai/stream.go`、请求 Worker stream contract、`internal/issueai` 批次/结算、metadata attempt events、Gateway SSE、Issue AI card。
  - Red：注入数据块事务失败、Provider 中断、数据库结算失败、关闭和租约丢失；断言 `(attempt_id, seq)` 连续幂等、`after_seq` 重放、未提交批次上限、请求槽释放、进程级背压以及没有第二次模型请求。
  - Green：协调器拥有批次提交、输出契约与结算；请求 Worker 只产出 Provider 数据块。第一次结算失败打开无配置背压门，全部保留事务完成后关闭并立即扫描。
  - 验证：运行流/metadata/Gateway/Web 聚焦测试、`mise run test-meta-pg`、`mise run web`。
  - 验收：只有完整成功且通过输出契约的流可以成为当前分析；中断流已提交内容只作为诊断保留。

- [ ] 7. 完成取消、历史清理和读者/运维诊断
  - 交付：全局关闭持久取消任务并拒绝迟到发布；保留期热更新清理运行材料但保留最近成功投影；产品详情公开语义状态，内部诊断不泄露所有者令牌或凭据。
  - 阻塞于：步骤 4–6。
  - 涉及位置：AI manager 热更新、Janitor、metadata cleanup、Issue AI API/UI、内部指标和结构化日志。
  - Red：覆盖关闭与结算竞态、重新开启不复活、空值继承日志保留期、缩短保留期清理已有历史、最近成功投影保留、秘密扫描以及 `claim_epoch`/副本身份不进入产品契约。
  - Green：用条件终态事务完成取消；Janitor 每轮读取当前有效保留期；把产品语义与内部协调诊断映射分开。
  - 验证：运行 AI settings/Janitor/Gateway/Web 聚焦测试、`mise run test-meta-pg`、`mise run web`。
  - 验收：取消后任何迟到响应都不能发布；历史到期不删除仍展示的最近成功分析。

- [ ] 8. 建立独立的 Issue AI 真实产品门禁
  - 交付：真实 All-in-One 中创建含 Trace/log/符号化栈的 Issue，手工和自动路径分别完成任务接管、流式展示、成功刷新、失败保留、重启恢复和项目隔离；该门禁使用独立 mise 任务，不并入核心预生产质量计划。
  - 阻塞于：步骤 1–7。
  - 涉及位置：根 E2E orchestrator、Issue AI Playwright fixture/spec、`mise.toml`、GitHub workflow；若测试命令变化，同步更新 mise task。
  - Red：先新增命名门禁，断言它必须产生持久任务、SSE 重放、重启恢复、禁止自动外发和密钥排除证据；当前实现应因进程内队列和同步 POST 失败。
  - Green：只补齐真实 wiring 和夹具，不复制已有 All-in-One 启动逻辑；对最终一致状态使用有界轮询，不使用固定长休眠。
  - 验证：运行新的 Issue AI E2E mise 任务、`mise run e2e` 和 `mise run workflow-check`，确认核心 e2e 不把 Issue AI 当成预生产完成条件。
  - 验收：Issue AI 自己的门禁可独立启用、失败和排期；核心预生产门禁在完全禁用 AI 时仍可通过。

## Green 后审查与重构

- [ ] 每个 Green 后用 `golines -w -m 120 --chain-split-dots` 格式化本切片修改的非生成 Go 文件；不得格式化 `internal/db/ent`，并重跑聚焦测试。
- [ ] 对 `issueai` 模块做删除测试：删除后 Gateway 不应重新拥有任务、上下文、预算、租约或结算；Issue/告警主链仍应编译并运行。
- [ ] 检查 metadata 只拥有 Ent、事务、方言和原子条件写，`internal/ai` 只拥有 Provider 协议，领域状态机仍在 `issueai`。
- [ ] 新行为接口测试覆盖后，删除旧 channel、同步调用和只断言私有函数调用次数的浅层测试；保留解析、Gateway、双方言和真实 UI 契约。
- [ ] 检查全部缓存租约路径都有数据库纪元条件写作为最终正确性边界，不能把 Redis 当作权威任务队列。
- [ ] 检查上下文裁剪文案没有暗示服务端完成隐私脱敏；任何遥测文本都不能获得指令权威。

## 最终验证

- [ ] `mise run fmt-check`
- [ ] `mise run migration-check`
- [ ] `mise run ent-generate-check`
- [ ] `mise run test`
- [ ] `mise run test-meta-pg`
- [ ] `mise run vet`
- [ ] `mise run lint`
- [ ] `mise run web`
- [ ] Issue AI 独立 E2E mise 任务
- [ ] `mise run e2e`
- [ ] `mise run workflow-check`
- [ ] `git diff --check`
- [ ] `git status --short internal/db/ent` 无输出。
- [ ] 确认未修改 `.agents/docs/requirements.md`，没有发布前旧 schema/API 兼容 adapter、fallback 或 alias。
- [ ] 逐项核对 Issue AI 源规格的用户故事、设置、上下文、持久执行、UI、测试和范围外决策，每项至少由一个公开契约或真实产品断言覆盖。
- [ ] 确认核心预生产质量 Spec、Plan 和门禁在全局 AI 禁用时仍然完整，不引用 Issue AI 成功作为验收条件。

## 未决事项

- 无产品决策未决。Standalone Worker 拆分、多副本共享租约和集群容量语义明确留待未来设计，不得在初始实现中自行推断。
