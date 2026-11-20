# 后端防坑参考（`internal/`、`cmd/`）

适用于网关、鉴权、元数据、嵌入式托管组件、告警引擎、错误追踪、存储查询、AI 与 MCP 相关改动与审查。

## 1. 网关跨项目隔离、RBAC 与输入边界（`#159`、`#171`、`#173`、`#181`）

- **表单与代理内容类型归一化**：解析 `Content-Type` 前先做媒体类型解析与大小写归一化，覆盖 `application/x-www-form-urlencoded` 及 `multipart/form-data`，防止上游大小写无关解析绕过 `stripReserved` 与 `InjectPromQL`；检查 `parser.Inspect` 等 AST 遍历返回的错误。
- **项目解析 fail-closed 与清晰状态码**：所有项目级路由统一走会话项目解析（如 `loadSessionProject`）并校验 `project.ID > 0`；项目不存在或无权限返回 404/403，切勿返回裸 401 误触发前端会话过期登出。
- **白名单 RBAC 与标识校验**：角色鉴权用白名单判定目标权限集合，不用黑名单排除单个角色；对 `trace_id`、`Project.Slug` 等路由与租户参数执行严格字符白名单校验，防止路径穿越、路由碰撞与标签注入。
- **多层解压与流式连接边界**：请求解压处理多值 `Content-Encoding`、嵌套层数上限与 `zstd`/双层 `gzip`（如 pprof 经 `gunzipBounded` 后再次触发 `ParseData` 解压）的统一字节预算；Live Tail 等长流绑定客户端断连与关机取消信号、设并发上限，并启用 `ImmediateHeaderFlush` 与 `X-Accel-Buffering: no`。
- **对外错误脱敏**：未认证探针（`/healthz`、`/readyz`）与代理 5xx 仅输出稳定错误码与摘要，不在响应体回显底层存储、Redis 或 PostgreSQL 的原始地址与错误细节。

## 2. 登录抗 DoS 与会话/凭据安全（`#160`、`#165`）

- **认证入口防资源放大**：Argon2id 等高内存口令校验通过并发信号量限流，避免少量并发即触发 OOMKill；在执行密码学校验与审计写库前先校验 `username` 长度并做限流降噪；支持显式配置 `TrustProxy` 避免反代后全员共享同一限流桶；内存态限流器与锁定 map 配置容量或过期回收。
- **区分凭据错与存储故障**：`AuthenticateCredentials` 与 `queryRequired` 仅在明确 `IsNotFound` 时走 `argon2Dummy` 或返 401；数据库/Redis 故障原样映射为 503，避免误锁账号 15 分钟或误踢在线会话。
- **会话吊销与令牌隔离**：管理员改密、降权或删人时联动吊销目标用户的全部已有会话，并在事务内保护平台最后一个 `admin`；不同用途令牌使用独立前缀（会话令牌与 `los_` Source Map 上传令牌分离），启用原生 TLS 时自动开启 `SecureSessionCookie`。
- **热路径 `singleflight` 与共享密钥原子写**：`Authenticator` 的 `singleflight` 用 `defer` 确保 panic 时清理 `inflight` 并关闭通知通道，且使用脱离 leader 取消的 `context` 防单请求断连波及同批请求；`secret.LoadOrCreate` 使用唯一临时文件、`fsync` 与原子独占落盘，防共享卷多副本首启覆盖密钥。

## 3. 出站 SSRF、重定向与 SMTP 安全（`#161`、`#164`、`#171`）

- **统一出站安全传输层**：通知渠道、BYO AI `base_url` 与 Scrape Target 共用出站防护——使用解析后 IP 校验的 `SafeDialer` 拒绝环回、私网、链路本地与云元数据地址，规范化端口（拒绝前导零或偏移端口），默认禁止跟随 HTTP 3xx 重定向（防止跨主机泄露 Anthropic `x-api-key` 等鉴权头；Scrape 显式设置 `follow_redirects: false`）。
- **渠道凭据密封与错误脱敏**：通知渠道密钥走 `secret.Box` 静态加密且接口脱敏回显；记录 `notification_outbox.last_error`、`notification_deliveries.error` 及渠道测试报错前剥离 `*url.Error` 中的敏感查询参数与 Webhook Token，避免 `member` 通过通知日志读取管理员凭据。
- **SMTP 边界与渠道业务码校验**：SMTP 拨号与发送严格绑定带超时的 `ctx`，防止单慢渠道阻塞整批 `OutboxWorker.drain`；剔除邮件主题/发件人/规则名中的 `\r\n` 防头注入；`Sender` 与 `Outbox` 不叠加嵌套重试；飞书/钉钉/企微在 HTTP 200 后校验响应体业务错误码并 drain 响应体；Scrape 配置校验拦截 `__` 元标签与非法 `interval`。

## 4. 告警租约时钟、Fencing、状态机与 Outbox 幂等（`#161`、`#162`）

- **数据库时钟与 fencing 必记日志**：成员与租约过期全部走数据库时间 `alertClockSQL`/`alertLeaseExpiry`（ADR 0011）；`ErrStaleRuleFence`、`ErrRuleLeaseHeld`、`ErrInactiveAlertEngineMember` 等租约或 fencing 被拒路径一律记录含规则、成员与纪元的 `slog.Warn`（见 `AGENTS.md`）。
- **单调纪元与独立续租**：释放或暂停规则租约时保留高水位纪元而非删行回退至 `1`；规则评估池与独立批量续租解耦，防止慢查询超过 `LeaseTTL` 导致所有权震荡；`UpdateAlertRule` 修改表达式或启停时重置 `alert_rule_states` 并推进纪元；`DeleteAlertRule` 正确处理关联 `alert_events` 外键。
- **状态收敛与通知语义**：`recovered` 状态落盘后收敛清理，不每轮覆写 `ends_at`/`updated_at` 破坏 30 天保留；被静默或抑制的告警不更新 `LastNotifiedAt`，非通知评估周期保持已有 `inhibited` 状态；平台告警（如 `error_rate_spike`）纳入持久状态，幂等键包含周期/窗口维度避免同内容后续告警永久被吞；规则自定义 `Labels` 不可覆盖受信任 `project_id`。
- **重排与 Matcher 校验**：`RequeueNotification` 同步重置 `attempts` 并使用数据库时钟；静默/抑制 Matcher 正则统一锚定 `^(?:...)$`，并在写接口校验正则语法、时间窗与 `for` 持续时间。

## 5. 双库（SQLite & PG）一致性与节点生命周期（`#163`、`#165`、`#175`）

- **时区、连接参数与双端 Schema 对齐**：时间列统一按 UTC RFC3339 读写，避免非 UTC 宿主机 SQLite 比较错乱；SQLite 连接级 PRAGMA（`foreign_keys`、`busy_timeout`）写入 DSN，PostgreSQL 显式配置最大连接数与生命周期；双端 `001_baseline.sql` 保持索引列序、排序方向与外键动作一致。
- **分批清理与全局锁序**：所有保留清理按批 `LIMIT` 删除并补齐 `sessions`、`notification_deliveries`、`notification_outbox` 三张表，防止无界 `DELETE` 锁表阻塞摄取；跨表事务遵循统一加锁顺序（如先 `telemetry_inbox_state` 后 `telemetry_inbox`），杜绝 PostgreSQL `40P01` AB-BA 死锁。
- **托管组件关闭顺序与只读 `doctor`**：`runServer` 启动失败通过 `defer` 回收已拉起的托管组件；`vmagent` 与存储引擎使用独立关闭超时，避免前者卡死导致存储被直接 `SIGKILL`；`pre-stop` 先满足 readiness 摘流窗口再触发服务停止；`doctor` 保持纯只读，检查真实 HTTP 状态码并覆盖关键密钥与写入端点。
- **HA 协调与不可复用租户键**：HA 多副本的 Janitor、全局抓取等周期任务通过选主或分片避免重复工作；避免以可复用 `slug` 作为持久指标与 Blob 命名空间，删项目同步清理孤儿 Blob 与采集任务；`blob.Local` 独占子目录并在 `List` 中过滤 `.blob-*` 临时文件。

## 6. 错误追踪、Source Map 与 HA 同步（`#163`）

- **唯一 `claim_owner` 与 O(1) 就绪检查**：Error Inbox 使用跨副本全局唯一的 `claim_owner` 并结合尝试次数/纪元做 fencing，丢弃 `ErrClaimLost` 时记 `slog.Warn`；`/readyz` 与高频诊断指标只做 O(1) 状态行查询，不扫全表 `telemetry_inbox`。
- **指纹截断方向与时间钳位**：堆栈指纹按 SDK 栈帧顺序保留最内层（真正出错点）栈帧，避免截断头部合并无关 Bug；客户端 `occurred_at` 按 ADR 0007 做服务端时钟漂移钳位，防止未来时间钉死 `last_seen` 或逃逸保留期。
- **Source Map 抗崩溃与字节预算**：上传时即校验 JSON v3 结构并剥离未用 `sourcesContent`；后台符号化 worker 捕获 `recover()`，防止畸形 `.map` 触发 `go-sourcemap` panic 形成多轮崩溃循环；LRU 按字节设预算，符号化显式区分瞬态存储错误与缺失制品；上传与 GC 通过宽限期或一致引用快照避免误删新 Blob。
- **VLogs 大事件回读与持久生命周期**：读取 VLogs 错误事件时匹配大事件二次转义后的单行扫描上限，查询显式加 `uniq by (event_id)` 与 `sort by (_time) desc`；Issue 生命周期事件在提交事务内持久化，不依赖提交后进程内回调。

## 7. 存储高基数、大对象界限与可观测查询（`#171`、`#173`、`#175`、`#177`）

- **公开写入面准入边界**：面向公开项目 API Token 的 Remote Write、Vitals、Replay、Profile 接口均实施按项目/IP 限流、批条数与展开体积上限及标签规范化（如 `vitalsIngest` 规范化 `page` 去除随机查询串），防范高基数与空对象内存放大。
- **查询投影、排序与数值安全**：LLM/Logs/Profiling/Replay 查询下推字段投影（如 VLogs `| fields`）、先排序再截断并限定时间窗，避免无序 `limit` 丢最新记录或响应超限返 502；聚合指标防 `NaN`/除零与时间窗溢出（如 `step=1s` 放大查询）；所有上游探测与代理请求设超时、校验状态码并 drain body。
- **编码与成本核算契约**：内部转发 Trace ID 等路径参数只执行一次 URL 编码；尊重客户端压缩头（如 `X-LO-Replay-Compression: identity`）；LLM 定价配置确保 `ai.pricing_file` 生效，精准匹配模型前缀并计入缓存 token，避免开源或商用模型静默记 `$0`。

## 8. AI / MCP / LLM 契约与并发安全（`#164`、`#177`）

- **不可变配置快照与 HA 刷新**：动态 AI 设置整体封装为带 `revision` 的不可变快照（含 `updatedBy`），更新路径串行或 CAS 写库并支持多副本热刷新；单次请求只取一次快照并校验 `client != nil`，防止热关闭引发竞态空指针；`ollama` 等本地 provider 允许无 API Key 启用，旧密文损坏不阻断管理员禁用或重写配置。
- **自动分析预算与 Prompt 边界**：自动 Issue AI 受项目显式开关、日预算与独立队列/限流约束，避免公开项目 API Token 刷爆 LLM 账单；持久分析任务遵循 ADR 0009 防并发覆写与关机丢队列；不可信遥测与 schema 放入结构化用户数据边界，不拼入 system prompt；限制 AI 助手 `stats` 体积与查询窗口。
- **流式传输与 MCP 契约**：`ChatStream` 使用响应头/空闲超时而非全局 `http.Client.Timeout` 截断长流，并透传供应商流式 `error` 事件；MCP 工具（如 `get_issue`）与网关真实响应 DTO 保持一致并配真实网关契约测试，空结果做判空保护，`APIClient` 强制配置超时，CLI 避免用命令行 `--dsn` 暴露 `lou_` 密钥。
