# 遥测摄取限流统一采用单副本本地执行，不依赖 Redis

网关（Gateway）的遥测摄取热路径此前存在两套彼此矛盾的限流语义：`ingest_rate_limit`（`internal/ingestlimit`，默认关闭）在单个应用副本（Application replica）内使用内存令牌桶按项目限流，不访问 Redis，但 `docs/version-matrix.md` 未明示其单副本（per-replica）语义；与此同时，Replay（`POST /api/replay/segments`）、Profile（`POST /api/profiles`）与 Prometheus Remote Write（`POST /api/v1/write`）在 `ingest_rate_limit` 之外又经 `g.limits` 叠加了固定窗口限流，HA 启用 Redis 时每次写入会增加 1～3 次同步 Redis RTT，且 Redis 不可用时 fail-closed 返回 `503`。这既在高频写入热路径上引入网络延迟与单点热点，也令同属遥测摄取的接口在 Redis 抖动下表现出不一致的失败模式，违背 ADR 0001「Redis 丢失不应破坏核心路径」的原则。我们决定：全量遥测摄取限流统一改为应用副本本地执行，热路径零 Redis 依赖，同时保留可配置的统一项目限流与默认开启的端点级反滥用护栏。

## 决策

- **全量遥测摄取热路径零 Redis 依赖**：所有遥测写入路由（OTLP HTTP/gRPC、Private Events、Replay、Profile、Prometheus Remote Write）的限流都在单个应用副本进程内完成，不再经共享 `g.limits` 访问 Redis，也不因 Redis 抖动或不可用返回 `503`。
- **保留两层本地护栏、职责分明**：
  - **可配置的统一项目摄取限流（`ingest_rate_limit`，默认关闭）**：覆盖全部 HTTP 摄取路由与原生 OTLP/gRPC 服务，按 `project.ID` 在单应用副本内执行 `requests_per_second`、`burst` 与可选 `bytes_per_second` 令牌桶限流（由 `max_tracked_projects` 控制有界淘汰）。其阈值明确定义为**单应用副本限额**；超限时继续返回 HTTP `429`（含 `Retry-After` 与 `{"code":"ingest_rate_limited","error":"project ingest rate limit exceeded"}`）或 gRPC `ResourceExhausted`（含 `google.rpc.RetryInfo`）。
  - **始终开启的端点级本地反滥用护栏（默认开启）**：保留面向公开浏览器或大载荷写入端点的默认防刷防线，但底层从共享 `g.limits`（Redis）改为进程内有界本地限流器，并在落地时收口重复的限流入口与错误响应处理：
    - **Replay**（`POST /api/replay/segments`）：单段请求体上限 5MB；单副本每项目 600/min、每 IP 300/min、每 session 120/min；
    - **Profile**（`POST /api/profiles`）：单次上传上限 10MB（含 multipart 余量）；单副本每项目 120/min、每 IP 240/min；
    - **Prometheus Remote Write**（`POST /api/v1/write`）：单副本并发上限 16、单副本每项目 600/min。
- **收窄 Redis 共享限流的职责边界**：共享 `g.limits`（Redis）仅服务于非遥测摄取热路径（登录防爆破与审计降噪、通知渠道跨副本发件箱限流、AI 助手用户配额等），不进入任何遥测写入关键路径。
- **精确全局配额由外围承担**：HA 部署若需要跨应用副本的严格全局项目 QPS 或带宽上限，由网关前端的反向代理、Ingress 或负载均衡器实施，不在网关摄取热路径引入集中式同步计数。

## 备选方案

- **全部遥测摄取限流统一迁入 Redis 共享计数（拒绝）**：虽可在多副本 HA 下提供精确的全局项目配额，但每次遥测写入都要承担同步 Redis RTT 与单点热点，且把全量遥测写入可用性与 Redis 强绑定（Redis 闪断即造成全量摄取 `503`）。
- **维持现状（`ingest_rate_limit` 本地，Replay / Profile / Remote Write 走 Redis），仅补充文档说明（拒绝）**：继续保留多套割裂的限流实现，且无法消除 Replay、Profile 与 Prometheus Remote Write 在 Redis 抖动时的 `503` 耦合。
- **删除 Replay / Profile / Remote Write 的内置限流，全部合并进 `ingest_rate_limit`（拒绝）**：`ingest_rate_limit` 默认关闭，且只按项目维度计数、不含每 IP、每 session 与并发上限；合并会导致默认零配置下公开浏览器摄取接口失去防滥用防线。

## 后果

- **HA 容量规划与协议文档契约**：多副本 HA 下，单个项目的有效摄取限额与端点默认防刷限额随应用副本数线性缩放（约为 `单副本阈值 × 应用副本数`）。实施代码改动时，须在同一个 PR 中更新 `docs/version-matrix.md`、`docs/zh/version-matrix.md` 及相关部署/配置文档，明示上述配额均为单副本本地限额。
- **落地方式**：端点级护栏使用 `internal/ingestlimit.Window`（进程内按键固定一分钟窗口，键数有界，默认上限 `DefaultMaxWindowKeys`，按窗口起点排序、先清理过期键、满时淘汰最早重置的键，无后台 goroutine）。每个维度（项目、IP、session）各用独立实例，避免一个键空间的高基数冲刷挤掉另一维度的计数；Replay、Profile 与 Remote Write 统一经 `internal/gateway/ingest_guard.go` 的 `admitLocalIngest` 返回 `429` 与按窗口剩余秒数计算的 `Retry-After`，对应的 Redis fail-closed `503` 分支已移除。
- **不影响鉴权与非摄取限流**：项目 API Token（`lo_`）鉴权缓存、登录限流、会话存储、通知渠道发件箱限流及 AI 请求限流不在本决策调整范围内。
