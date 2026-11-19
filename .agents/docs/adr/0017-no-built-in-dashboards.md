# 不做内置 dashboard，用户看板交给 Grafana / Perses

需求基线 v1.0 要求内置可用的 dashboard：3.2 的「每个 SDK 框架对应一套开箱即用 dashboard」、3.13 的主机与中间件内置 dashboard，以及 M1 的「默认 dashboard」（M3 的主机视图也带「内置 dashboard」）。我们决定不做可编辑的 dashboard 系统，也不内置框架或中间件的 dashboard 模板：已经实现的部分连同主机中间件视图一起删除（见 `CHANGELOG.md` 的 Removed 条目，`web/` 里没有 dashboard 路由、导航项或引擎）；用户要自建看板时，把 lazy-observe 作为 Grafana 或 Perses 的数据源。指标走 Prometheus 兼容查询 API，鉴权用用户 API Token（`lou_…`）加 `X-LO-Project`（ADR 0010）。原因：Grafana / Perses 在看板编辑、变量和面板生态上已经很成熟，自建一套等于重做一个 Grafana，和单人维护的人力不匹配；用户存量的 Grafana dashboard 也可以直接复用（基线 3.13 自己就提到了这一点）。

产品内置的固定页面不属于 dashboard，不受本决策影响：overview、performance、主机列表与详情、LLM 用量与成本、metrics/logs/traces 查询、profiling 等照常由 lazy-observe 提供（术语见 `GLOSSARY.md` 的「外部仪表盘」）。

## 备选方案

- **自建可编辑的 dashboard 系统（拒绝）**：看板编辑、变量、面板生态都要自己做，等于重做一个 Grafana，与单人维护的人力不匹配。
- **不做编辑器，只内置框架与中间件的 dashboard 模板（拒绝）**：本决策的范围就是连模板一起不内置，用户存量的 Grafana dashboard 可以直接复用，不需要我们另出一套。

## 后果

- 需求基线 v1.1 改写涉及内置 dashboard 的条目（1.1 夜莺行、1.3-2、3.2、3.9、3.13、5.4 元数据清单、第 8 节 M1 与 M3），并引用本 ADR。
- Grafana / Perses 对接目前只交付了指标一类数据源：`docs/deployment.md` 的 Grafana 一节、README 和设置页的 Grafana 片段都只覆盖 Prometheus 兼容查询 API（`/api/v1/query` 等），文档明确日志与链路读取走 `/api` 下的自有路由，不属于 Prometheus 数据源的支持面；Perses 只在 README 与 CHANGELOG 里提到，没有单独的接入文档，也没有验证过。
- 「交给 Grafana / Perses」覆盖日志、链路两类数据源之前，只是方向，还没有这两类的交付物；要验证并写进文档后才算完成。
