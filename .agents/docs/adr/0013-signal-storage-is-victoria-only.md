# 信号存储固定为 Victoria 系，不做 Loki / Tempo 适配

v1.0 需求基线 5.2 把 Standalone 的兼容范围定为「VM 系 + Grafana 系」：metrics 走任意 Prometheus 兼容查询端点，logs 额外做 Loki/LogQL 查询适配器，traces 经 Jaeger API 或 Tempo 原生 API 接 Tempo；第 2 节目标用户表也把「已有 Prometheus/VM 体系的团队」对应到 Standalone。这些兼容项的前提是「用户已经有一套别家的存储，lazy-observe 去接它」。我们决定 lazy-observe 的 metrics、logs、traces 信号存储固定为 Victoria 系（VictoriaMetrics、VictoriaLogs、VictoriaTraces），存储由 lazy-observe 自己带，不去兼容别家存储：

- **单机用 All in One**：lazy-observe 作为托管组件拉起 Victoria 系子进程，这是默认形态，不变。
- **集群用 Helm 部署 HA**：网关多副本连接 Victoria 系集群（vmselect/vminsert、VictoriaLogs/VictoriaTraces 集群前的 vmauth/LB）。
- **Standalone 保留为「外接 Victoria 系」**：它是 HA 网关连接外部存储走的同一条 `storage.SignalBackend` 路径，也覆盖已经自己运维 Victoria 系的团队。外接的存储必须是 Victoria 系。
- **Loki/LogQL、Tempo/TraceQL 适配器出范围**：不是「尚未实现的后续工作」，而是按设计不支持。不在路线图中排期。
- **通用 Prometheus 兼容查询端点不再是官方支持范围**：`storage.metrics.kind: prometheus`（Prometheus 本体、Mimir、Thanos 等）的代码和现有测试保留，但文档不再推荐，示例与发布验证只覆盖 VictoriaMetrics，出问题不保证修。Prometheus remote-write 1.0 摄入与 PromQL/MetricsQL 查询兼容属于协议层生态兼容，不受本决策影响。

本 ADR 记录对需求基线 v1.0 的 5.2、第 2 节 Standalone 一行以及第 9 节决策记录第 4 条的偏离。

## 备选方案

- **保留 Loki/Tempo 适配器，降级为远期工作（拒绝）**：存储已经由 lazy-observe 自己带，接别家存储的需求不存在；LogQL 与 LogsQL 语义不同，适配器是 5.2 里最大的单项工作，日志查询界面、告警、AI 生成查询都要再按 LogQL 做一遍，与单人维护的人力不匹配。
- **删掉 Standalone，只留 All in One 与 HA（拒绝）**：HA 网关连外部 Victoria 系集群用的正是 Standalone 的外部存储实现，拿掉模式名不会减少代码，反而让单副本外接 Victoria 系的部署失去说法。
- **删掉 `kind: prometheus` 代码（暂不做）**：现有代码与测试维护成本很低，删除不带来收益；先降为非官方支持，确有负担再另行决定。

## 后果

- `README.md`、`README.zh-CN.md`、`.agents/docs/architecture.md`、`docs/deployment.md` 把 Loki/Tempo 写成「按设计不支持」，Standalone 示例改用 VictoriaMetrics。
- 「对接 Grafana / Perses」指的是把 lazy-observe 作为 Grafana/Perses 的数据源（只读查询），与本决策无关，仍在路线图中。
- 存储能力上限取决于 Victoria 系上游，VictoriaTraces 仍是 v0.x，这一风险由 lazy-observe 承担，不能靠切换到别家存储规避。
- 需求基线中依赖 Grafana 系存储的验收项不再适用；之后若出 v1.1 基线，按本 ADR 改写 5.2。
