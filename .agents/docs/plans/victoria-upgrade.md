# Victoria 组件升级到「发布满 7 天的最新稳定版」

## 目标与规则

- 负责人要求：追较新的版本，并带「最小发布年龄」防供应链攻击。最小年龄 7 天，与
  `.github/dependabot.yml` 的 cooldown 一致。
- 选择规则：发布时间早于「今天减 7 天」的最新稳定版（排除 prerelease、draft），tag 过滤掉
  `-cluster`、`-enterprise` 变体。今天 2026-11-19，截止线 2026-11-12。
- 只社区版（Apache-2.0）。范围外：`.trivyignore.yaml`（剩余漏洞命中由主线程决定）、
  年龄门禁工具（R1 切片）、`requirements.md`、`.agents/skills/**`。

## 版本选择（`gh api repos/VictoriaMetrics/<repo>/releases` 的 `published_at`）

| 组件 | 旧 pin | 新 pin | 发布时间 | 年龄 | 为什么不是更新的 |
| --- | --- | --- | --- | --- | --- |
| VictoriaMetrics single + vmutils | v1.147.0 | **v1.156.0** | 2026-11-09 | 10 天 | v1.156.0 就是当前最新稳定版 |
| VictoriaLogs | v1.52.0 | **v1.53.0** | 2026-10-01 | 49 天 | v1.54.0、v1.52.2 都发布于 2026-11-13，只有 6 天，不满 7 天 |
| VictoriaTraces | v0.9.4 | **v0.13.0** | 2026-11-05 | 14 天 | v0.13.0 就是当前最新稳定版 |

VM 的 v1.148.9（2026-11-09）等是旧线的向后移植补丁，不是「最新稳定版」，不取。

## sha256 来源

8 个归档的 sha256 取自上游 release 的 `*_checksums.txt`（只下载 checksum 文件），并与 GitHub release
资产的 `digest` 字段交叉核对，两者一致。资产名都不带 `-enterprise`。随后 `mise run storage-bins`
真实下载 linux-amd64 归档并校验；trivy 脚本又下载并校验了 linux-arm64 归档，8 个归档都通过。

## Breaking changes 对照

对照范围：VM v1.147.0 → v1.156.0（含 1.148–1.155 全部 release notes）、Logs v1.52.0 → v1.53.0、
Traces v0.9.4 → v0.13.0（0.10.0、0.11.x、0.12.x、0.13.0）。本仓库实际依赖面：

- 启动 flag（`internal/embed/supervisor.go`）：`-httpListenAddr`、`-loggerOutput`、`-storageDataPath`、
  `-retentionPeriod`、`-inmemoryDataFlushInterval`；metrics 另有 `-search.latencyOffset=0s`；
  traces 另有 `-insert.indexFlushInterval`；vmagent 用 `-promscrape.config`、`-remoteWrite.url`、
  `-remoteWrite.tmpDataPath`。
- 接口：VM `/api/v1/query`、`query_range`、`series`、`labels`、`/api/v1/write`；Logs
  `/insert/jsonline`、`/insert/opentelemetry/v1/logs`、`/select/logsql/*`；Traces
  `/insert/opentelemetry/v1/traces`、`/select/jaeger/api/*`、`/select/logsql/query`。

| 上游变更 | 影响本仓库？ | 结论 |
| --- | --- | --- |
| 以上所有 flag：没有任何一个被改名、删除或改默认值（`rg` 对照 notes 无命中；Traces 只弃用了 `-search.traceServiceAndSpanNameLookbehind`、`-search.traceMaxServiceNameList`、`-search.traceMaxSpanNameList`，本仓库不用） | 否 | 不改 |
| VM v1.149.0：`/api/v1/admin/tsdb/delete_series`、`/tags/delSeries` 只接受 POST | 否，仓库不调用 | 不改 |
| Logs v1.53.0、Traces v0.12.0：`/internal/force_merge`、`/internal/force_flush`、`/internal/log_new_streams`、`/internal/partition/*`、`/delete/run_task` 要求 POST | 否，`rg` 在代码和脚本里没有这些路径 | 不改 |
| Logs v1.53.0：`/select/vmalert/*` 的 `/config`、`/reload` 要求 httpAuth | 否，不用 vmalert 代理 | 不改 |
| Traces v0.12.0：`-search.latencyOffset`（默认 30s）开始也作用于大部分 LogsQL API（此前只有 Jaeger、Tempo）。本仓库 `internal/gateway/llm_api.go` 的 LLM 调用统计走 traces 的 `/select/logsql/query` | 潜在：最近 30s 的 span 在该面板里不可见 | 已处理，见下面「主线程决定」第 1 条：托管的 traces 传 `-search.latencyOffset=0s` |
| Traces v0.12.0：Tempo `startTimeUnixNano` 改为字符串、v0.10.0 Tempo 搜索行为变化 | 否，本仓库只用 Jaeger API | 不改 |
| Traces v0.13.0：vtagent 改 OTLP 导出、`-remoteWrite.format` 与 `/insert/native` 删除；OTLP gRPC TLS 默认关；`-opentelemetry.traces.maxRequestSize` 默认 64MiB→32MiB | 否：不用 vtagent、不用 OTLP gRPC TLS；本仓库只向 `/insert/opentelemetry/v1/traces` 发 OTLP/HTTP，请求远小于 32MiB | 不改 |
| Traces v0.10.0：基础镜像改 distroless、不再发 linux/386 | 否，我们用上游二进制归档 | 不改 |
| Traces v0.9.4：集群版 vtselect/vtstorage 内部协议版本升级 | 否，只用单机版 | 不改 |
| VM v1.150.0：`-enableMultitenancyViaHeaders` 默认变 true（cluster vmselect/vminsert、vmagent） | 单机版不涉及；cluster 路径由网关剥掉客户端的 AccountID/ProjectID 头，HA e2e 用显式租户路径（如 `/select/41/prometheus/...`） | 不改；e2e-ha 隔离断言在新镜像上全部通过 |
| VM v1.149.0：vminsert `-disableRerouting` 默认变 false | 仅 cluster；HA e2e 用 `-replicationFactor=2`，此时上游自动关闭 slowness 重路由 | 不改，由 e2e-ha 验证 |
| VM v1.153.0/1.148.7 说明：纯 Go（无 CGO）构建读块可能报 `window size exceeded` | 否：官方 linux/amd64、linux/arm64 归档使用 CGO，不受影响（我们的 `CGO_ENABLED=0` 只约束 lazy-observe 本身） | 不改 |
| VM v1.154.0：单机版不再切换到全局索引，`/api/v1/series`、`/labels`、`/label/*/values` 长时间范围可能内存暴涨，说明里写「升级到 v1.155.0」 | 本仓库调用 series/labels；v1.155.0 的条目只明确修了 cluster 的 `/admin/tenants`，单机版的长范围行为 notes 里没有对应的修复说明 | 已记录为风险，见「待判断」；e2e 覆盖功能面，不覆盖大保留期内存面 |
| 数据格式 | v1.147→v1.156 notes 没有写不可回滚的存储格式迁移 | 不改 |

## 追加核对项（SC2 审查线程提供，逐条核对）

1. **VM v1.154 起只用 per-day index（#11791）**：本仓库的补全与 AI schema 采集会调 `/api/v1/labels`、
   `/label/*/values`、`/api/v1/series`。实测（`/tmp` 里的临时脚本，不入库）：单机 VM，`extra_filters[]={project_id="p1"}`，
   导入 90 天数据，对比 v1.147.0 与 v1.156.0（全新导入）。
   2000 序列：两者 1d/30d/90d 都在 10–25 ms（v1.156.0 的 90d 偶发 80 ms）。
   30000 序列 × 90 天（每 12 小时一个点）：

   | 范围 | v1.147.0 labels / values / series | v1.156.0 labels / values / series |
   | --- | --- | --- |
   | 1d | 20 / 12–14 / 23 ms | 21 / 13–14 / 25 ms |
   | 30d | 31 / 25–27 / 27 ms | 27 / 25 / 50 ms |
   | 90d | 31 / 27–28 / 27 ms | 81 / 56 / 85 ms |

   结论：长范围慢约 2–3 倍，但仍是几十毫秒，没有 OOM、没有失败；v1.154.0 的 OOM 警告针对的是更大的
   序列规模与保留期。不改代码，列为风险，主线程可决定是否限制补全接口的时间范围。
2. **Logs v1.54 `/select/logsql/tail` 高写入速率下直接返回错误**：本次升到 v1.53.0，不涉及。
   以后升 v1.54+ 时要复查 `internal/gateway` 的日志 tail 路径（`/api/v1/logs/tail`）。v1.53.0 的 tail 在冒烟里返回 200。
3. **VM 非法查询状态码 422 → 400**（v1.150.0 notes，Prometheus 查询 API 请求参数缺失或非法）：`rg` 查
   `422|StatusUnprocessableEntity`，命中全部是网关自己对客户端输入返回的 422（`replay_api.go`、`profile_api.go`、`error_api.go`、
   `ai_assistant.go`、`issue_ai_analysis.go`），没有任何地方把上游 VM 的 422 当信号（上游状态码只按 2xx/非 2xx 判断）。无依赖，不改。
4. **VM v1.153 起忽略空 `extra_label`/`extra_filters`**（notes 原文：ignore empty ... query args instead of returning an error）：
   网关的值恒为 `{project_id="<slug>"}`/`project_id=<slug>`，字符串非空，不会被忽略；`project_id` 列 schema 是
   `NotEmpty().Unique()`，`management` 也拒绝空 slug。但 `applyMetricsIsolation` 此前没有显式保护，
   调用方还用 `metricsProject != ""` 决定是否隔离。新增：`applyMetricsIsolation` 对空 project 直接报错，
   并加 `internal/gateway/metrics_isolation_test.go` 钉住「VM 模式下 `extra_filters[]` 永远非空、覆盖客户端传入的值、
   空 project 被拒绝」。审查补充：`proxy` 在 `metricsProject == ""` 时根本不调用 `applyMetricsIsolation`，上面的检查
   覆盖不到 `/api/v1/query` 等代理路径；`metricsQuery` 现在对空 slug 直接返回 500、不转发，
   `TestMetricsQueryWithoutProjectFailsClosed` 钉住（去掉守卫时该测试看到 200 且请求到达上游）。
5. **VM `fill` 修饰符**：它是二元运算符修饰符（`a + fill(0) b`），不是函数。`web/src/lib/metrics-query.ts` 的
   `METRICSQL_FUNCTIONS` 表按 vmui 自带 MetricsQL.md 的 h4 标题生成；v1.147.0 与 v1.156.0 的 MetricsQL.md
   都是 232 个 h4，标题完全相同，所以函数表不需要变，没有改 web，也就没有跑 web 门禁。
   遗留：`metrics-query.ts:328` 的运算符分隔正则（`or|and|unless|default|ifnot|if|group_left|group_right`）
   不认识 `fill`，写 `fill(0)` 之后的指标补全上下文判断可能不准，属于体验问题，建议另开 web 切片；表头注释里的
   「v1.147.0」也留给那个切片一起改。
6. **Traces v0.11.1 多租户同 trace_id 建索引的修复**：包含在 v0.13.0 里。HA e2e 的「VTraces ... ProjectID isolation」通过。
7. **cluster 镜像升级后的 `-enableMultitenancyViaHeaders` 默认 true**：e2e-ha 用 v1.156.0 cluster 镜像全部通过，包含
   「VM cluster account 42 preserves project_id label isolation across replicas」和 VLogs/VTraces 的双向 ProjectID 隔离断言。
8. **`docs/version-matrix.md`**：里面没有写 Victoria 的版本号（`rg -i victoria` 只命中 OTLP 转发路径说明），无需更新。
   仓库其余位置的旧版本号只有 `web/src/lib/metrics-query.ts:393` 的注释，见第 5 条。

## 主线程决定与落实

1. **Traces latency offset → 托管 traces 传 `-search.latencyOffset=0s`**（`internal/embed/supervisor.go`，与 metrics 一致）。
   理由：v0.12 起该 flag 也作用于 LogsQL API，会让 LLM 统计面板相对升级前延迟 30s，属于升级引入的回退，按「保持升级前行为」处理。
   - v0.13.0 的 `victoria-traces-prod -help` 列出 `-search.latencyOffset`（默认 30s），实测接受。
   - standalone 实测（`-search.latencyOffset=0s`，写入当前时间的 span）：写入后 3 秒 logsql 默认查询即可查到（586 字节，升级前行为是 30s 后才可见）；
     Jaeger 取 trace 3 秒时仍是 404，约 5 秒变 200（逐秒轮询：5.1s 首次 200）。Jaeger 比 logsql 慢，是索引刷新所需时间，
     不是 offset；相比默认 30s 仍属改善。
   - 单测：`TestManagedStorageUsesBoundedUncleanShutdownFlushWindow` 增加 metrics 与 traces 都带 `-search.latencyOffset=0s` 的断言。
   - HA/cluster 路径：supervisor 只在内嵌模式启动 traces；standalone/HA 下的 vtinsert/vtselect 是外部服务，网关通过 `traces.url`
     访问，flag 由运维配置，本代码改不到。网关在集群模式下同样走 vtselect 的 `/select/logsql/query`（LLM 统计），
     所以外部 VictoriaTraces 保留 30s 默认，LLM 面板会延迟最多 30s。处理：在 `docs/deployment.md` Standalone 一节写明这一点和 flag；
     `scripts/e2e-multi-replica.sh` 的 vtselect 不加该 flag，e2e 里为 30s offset 放宽的 `waitFor` 也不改，HA e2e 继续覆盖默认 offset 的行为。
   - 审查补充（未实施，留给主线程决定）：v0.12.0 notes 还提供按请求关闭的 `disable_latency_offset=true` 查询参数
     （单机版和 vtselect 都支持）。`internal/gateway/llm_api.go` 是网关唯一走 traces LogsQL 的调用点，给它加该参数后，
     外部 VictoriaTraces/vtselect 不配 flag 也不会延迟；落地前要先确认 v0.12 以前的外部版本对未知参数是忽略还是报错。
2. **网关空 project 防护与测试**：保留在本切片（见追加核对项第 4 条）。
3. **Logs v1.53.0 的 4 条 + CVE-2026-97054**：本切片不处理。按负责人新规则（pin 已是满 7 天最新稳定版的命中只警告），合入后这些都是警告。
   Logs v1.54.0 于 2026-11-20 满 7 天，届时另开切片升级，并复查 `/select/logsql/tail`。
4. **VM 长范围查询内存**：只测到 3 万序列，列为风险，不在本切片处理。

## cluster 镜像（HA e2e）

`scripts/e2e-multi-replica.sh` 的 vmstorage/vminsert/vmselect 同步升到 `v1.156.0-cluster-scratch`，digest
从 `quay.io` 的 registry API 只读获取（`HEAD /v2/victoriametrics/<img>/manifests/<tag>` 的
`docker-content-digest`；同一方法对 `v1.147.0-cluster-scratch` 复现了脚本里原有的 3 个 digest，说明取法正确）。
docker daemon 没配置 registry mirror，拉取来源仍是 quay.io，没有换源。

不升的话：HA e2e 里 cluster 只被 lazy-observe 通过 HTTP 用，single 与 cluster 版本不一致本身不会有协议问题
（两者互不通信），但那样 HA 门禁就不再覆盖实际发布的 VM 版本，所以保持同一版本。

| 镜像 | 新 digest（index） |
| --- | --- |
| vmstorage | `sha256:af5ff1687fbdce82ad4efa77b83cff78ff42b866aa7ef8628ee6c7166268adec` |
| vminsert | `sha256:41fbf25df0ab77a12cae8ed0d2e8d76cd8d922e4e2fb824818f1087641a80e5e` |
| vmselect | `sha256:310ba425872897a618e60b884dc943772767d8ef0bad37a71c8e01678c232009` |

## LICENSE 副本

VM、Logs 的 LICENSE 与上游新 tag 逐字节一致，未改。Traces 新 tag 只有版权年份
`2019-2025` → `2019-2026`，已从 `v0.13.0` 重新取。三个仓库在新 tag 都没有 NOTICE 文件。

## 验证记录（全部为真实运行，worktree `slice/victoria-upgrade-1`，amd64 主机）

| 命令 | 结果 |
| --- | --- |
| `mise run storage-bins` | 退出 0；4 个 amd64 归档下载并通过 SHA-256；`-version` 为 VM/vmagent v1.156.0、Logs v1.53.0、Traces v0.13.0 |
| 8 个 pin 与上游 `*_checksums.txt` 对照；`check-storage-bin-vulns.sh` 另下载并校验了 4 个 arm64 归档 | 8/8 一致 |
| `mise run fmt-check`、`mise run vet`、`mise run test`、`GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint`（串行） | 最终代码上全部退出 0（lint 0 issues）。注意：第一版 commit 前的 fmt-check 漏检了当时还未被 git 跟踪的 `metrics_isolation_test.go`（`fmt-check` 只查 `git ls-files`），提交后才报出需要 golines，已格式化 |
| `mise run e2e`（全部代码改动之后） | 退出 0，389 s；`PASS e2e: consolidated functional and recovery evidence complete` |
| `mise run e2e-restart`（全部代码改动之后） | 退出 0；`PASS e2e-restart: graceful, forced, pending-work, and managed-child recovery gates passed` |
| `mise run e2e-ha`（v1.156.0 cluster 镜像，按 digest 从 quay.io 拉取；全部代码改动之后） | 退出 0；14 条 PASS，含 VM account 42 隔离、VLogs/VTraces ProjectID 双向隔离、`replicationFactor=2` 停一个 vmstorage |
| `go test ./internal/embed -run TestManagedStorageUsesBounded -v` | PASS（断言 metrics、traces 都带 `-search.latencyOffset=0s`） |
| `mise run dist-cross`、`mise run gate-release-artifacts` | 退出 0；amd64 二进制 110,567,586 字节、arm64 100,991,138 字节（门限 150,000,000）；空闲 RSS 446,652,416 字节（门限 524,288,000）；零配置冒烟通过 |
| 升级兼容实测 | 见下 |

升级兼容实测（临时脚本在 `/tmp`，不入库；旧二进制用旧 pin 下载并校验）：

- VM：v1.147.0 写 3 万序列 × 90 天，停止后用 v1.156.0 直接启动同一个 `-storageDataPath`，labels/values/series/instant query
  都正常返回，日志无 error/panic。
- Logs：v1.52.0 写 500 行，v1.53.0 启动同一目录，`count()` 仍为 500，hits/field_names/stream_field_values 返回 200，
  升级后写入并读回正常。
- Traces：v0.9.4 写入的 trace，v0.13.0 启动同一目录后 Jaeger 取回 200、搜索可用。
- vmagent：v1.147.0 在目标不可达时积压 persistent queue（3 个文件），v1.156.0 用同一个 `-remoteWrite.tmpDataPath`
  启动，日志 `opened fast queue ... 176257 pending bytes`，目标 VM 里能查到 12 条早于新 agent 启动时间的样本，即积压被送达。

## trivy 剩余命中（`check-storage-bin-vulns.sh`，参数同发版门禁，未改 `.trivyignore.yaml`）

amd64 与 arm64 相同，各 8 条 HIGH（均已有修复版本）：

| 二进制 | stdlib | 漏洞 | 修复版本 |
| --- | --- | --- | --- |
| victoria-logs-prod（Go 1.27.1） | v1.27.1 | CVE-2026-111943 | 1.26.10, 1.27.3 |
| victoria-logs-prod | v1.27.1 | CVE-2026-78667 | 1.26.9, 1.27.2 |
| victoria-logs-prod | v1.27.1 | CVE-2026-78669 | 1.26.9, 1.27.2 |
| victoria-logs-prod | v1.27.1 | CVE-2026-97031 | 1.26.9, 1.27.2 |
| victoria-logs-prod | v1.27.1 | CVE-2026-97054 | 1.26.11, 1.27.4 |
| victoria-metrics-prod（Go 1.27.3） | v1.27.3 | CVE-2026-97054 | 1.26.11, 1.27.4 |
| victoria-traces-prod（Go 1.27.3） | v1.27.3 | CVE-2026-97054 | 1.26.11, 1.27.4 |
| vmagent-prod（Go 1.27.3） | v1.27.3 | CVE-2026-97054 | 1.26.11, 1.27.4 |

对比升级前：每个架构 63 条 HIGH / 17 个 CVE。
