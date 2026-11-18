---
status: superseded by ADR 0003
---

# 使用 Grafana 风格的应用内数据库迁移生命周期

> 本决策已由 [ADR 0003](0003-pre-release-single-database-baseline.md) 取代。
> 本文仅保留为历史设计背景；项目处于首次发布前阶段时，不启用其中面向用户的版本化升级契约。

元数据 schema 的版本由 lazy-observe 发布版本拥有，而不是由外部 schema 服务拥有。应用嵌入一组有序的前向迁移，在数据库中记录成功执行的迁移，通过数据库锁串行化共享 PostgreSQL 的应用并发启动，并在任何迁移错误发生时拒绝进入就绪状态。

lazy-observe 只独立实现同类运维模式，不采用 Grafana 的 [AGPL 许可实现](https://github.com/grafana/grafana/blob/5342253d0a4b144ab825c8ce055c7cff22fb105e/LICENSE)。项目不会链接或引入 Grafana 软件包，也不会复制 Grafana 源码。不可变的 v1.0 需求基线继续作为历史背景；本 ADR 只取代其中选择 Atlas 的实现决策，同时保留可控、可测试的用户升级目标。

## 决策

- SQLite 与 PostgreSQL 配对且经过评审的 SQL 文件是权威迁移来源。迁移嵌入发布产物，按稳定 ID 排序，只允许追加；发布后绝不编辑或删除。
- Ent 继续作为 schema 和查询代码生成器。禁止使用 Ent 自动迁移。Atlas 不得进入运行时、生产 `go.mod`、发布二进制、迁移编写契约或用户升级流程。现有隔离的 Ent 代码生成工具依赖图不会因本 ADR 获得许可证例外，仍受仓库独立的许可证策略约束。
- 现有 `schema_version` 表作为项目迁移日志。它至少扩展记录稳定版本、名称、内容校验和、应用版本、耗时和执行时间。启动时拒绝校验和不一致，也拒绝数据库已记录版本高于当前二进制能够理解的版本。
- 启动流程必须先获取迁移协调权，才能创建或读取迁移日志。SQLite 沿用单进程、单连接模型，不支持多个应用进程。PostgreSQL 在一个专用连接上持有稳定的数据库级 advisory lock（咨询锁），在上下文限定的超时内等待，并在得到锁后重新读取已执行版本。等待中的应用实例保持未就绪；超时或锁丢失会导致启动失败。
- 每个普通迁移及其成功日志记录在同一个数据库事务中提交。错误会回滚该迁移、停止后续序列、保持实例未就绪，并留下可操作日志。无法在事务内有界完成的变更，必须使用明确记录且可恢复的数据回填，再由后续 schema 迁移收尾；不得把无界表重写隐藏在普通启动中。
- 发布采用扩展/迁移/收缩（expand/migrate/contract）流程。扩展迁移必须在滚动部署期间与上一版本二进制兼容；随后执行数据回填；只有旧应用副本已无法运行时，才发布破坏性的收缩迁移。迁移锁只能避免并发 DDL，不能自动提供跨版本 schema 兼容性。
- 不提供向下迁移。升级前，运维者必须备份 SQLite，或使用 PostgreSQL 原生备份/快照。只有发布说明明确声明迁移后的 schema 与旧应用兼容时，才允许回滚应用；否则必须同时恢复升级前数据库和旧二进制。

协调参与者是共享同一可写 PostgreSQL 数据库的 lazy-observe 应用进程。外部服务不属于迁移协调边界。

## 实施顺序

### 阶段 1：部署理解迁移锁、但不包含 schema 变更的二进制

- 增加带有有界默认值和校验的 `meta.migration_lock_timeout`，并贯穿服务器启动路径。新增生产与短超时测试使用的 options 入口，同时保持 `OpenStore` 源码兼容。
- PostgreSQL 必须在任何 `CREATE`、读取或写入 `schema_version` 前固定一个 `*sql.Conn`。使用 Go 标准库，根据项目命名空间和 `current_database()` 派生稳定的 64 位 advisory-lock 键。以有界退避轮询 `pg_try_advisory_lock`，直到超时或调用方取消。
- 所有迁移查询和 `BeginTx` 都通过同一固定连接执行。每个正常或错误出口都显式解锁；连接丢失也视为锁丢失并导致启动失败。等待者最终获得锁后，必须重新读取完整迁移日志，再判断待执行迁移。
- 数据库中 `MAX(schema_version.version)` 高于最高嵌入版本时拒绝启动。锁超时、解锁失败、数据库错误或迁移错误都会使 `OpenStore` 失败，因此实例绝不会进入就绪状态。SQLite 保持现有单进程路径，不使用 PostgreSQL advisory lock。
- 该兼容版本不得包含新的业务 schema 迁移。后续滚动发布依赖迁移锁前，所有 lazy-observe 应用副本都必须先运行该版本。跳过过渡版本的运维者必须在升级时把应用 Deployment 缩容到一个副本，因为旧进程不遵守 advisory lock。

验收需要真实 PostgreSQL 测试：外部连接持有锁时，短超时的 opener 必须失败且不能创建 `schema_version`；释放锁后，两个针对空数据库的并发 opener 都应成功，并且每个版本只记录一次。还需覆盖取消、连接丢失、迁移失败和锁释放。

### 阶段 2：在已部署的锁保护下加固迁移历史

- 增加一组配对的 SQLite/PostgreSQL 扩展迁移，为 `schema_version` 添加可空的 `name`、`checksum`、`app_version` 和 `duration` 字段。滚动部署期间，已有字段和旧读取方式仍然有效。
- 在持有迁移协调权时，幂等回填已发布迁移的名称和 SHA-256 校验和；之后的新迁移记录完整元数据。启动时，如果非空的已记录校验和与嵌入文件不一致，则拒绝继续。
- 在所有受支持的升级路径都完成回填前，新字段保持可空。后续任何 `NOT NULL`、重命名或删除操作，都必须是旧二进制退出支持窗口后的独立收缩迁移。
- 扩展 `mise run migration-check`，校验配对历史与校验和 fixture。使用普通 SQLite 测试、真实 PostgreSQL 测试套件，以及两个应用副本同时针对空白 PostgreSQL 启动的高可用测试作为门禁。PostgreSQL fixture 必须以非 root 用户运行，因为 `initdb` 拒绝 root。

Helm startup probe（启动探针）的预算必须长于配置的锁等待时间与预期迁移耗时之和。等待或迁移期间 readiness 保持 false；startup probe 可以避免 liveness 重启把正常等待者变成重启循环。

N1 兼容版本实现了阶段 1，且没有增加迁移文件：PostgreSQL 启动现在预留一个 pgx session，最多等待配置的超时时间以获取数据库专用 advisory lock，在该 session 上执行并重新读取迁移，拒绝数据库版本超前；显式解锁失败时丢弃连接。

阶段 2 由配对迁移 `014_migration_history.sql` 和 N2 迁移器实现。旧记录在迁移协调保护下幂等回填；新执行的记录保存当前应用版本和测得耗时；任何非空的已记录校验和都必须在执行下一迁移前得到验证。

## 实施状态

仓库已经嵌入配对 SQL，在启动时以事务执行每个迁移，使用 session 级 advisory lock 协调 PostgreSQL 应用进程，拒绝数据库版本超前和校验和漂移，记录扩展后的迁移历史，通过 `mise run migration-check` 拒绝修改已发布文件，并确保 Atlas 不进入生产迁移路径。完整 Gateway 高可用门禁会在等待 readiness 前，让两个应用副本针对空白 PostgreSQL 同时启动。在 N1 兼容版本运行于全部应用副本之前，任何修改 schema 的 PostgreSQL 升级仍必须把 lazy-observe 缩容至一个应用副本。

## 后果

用户通过替换应用版本完成升级，无须安装独立的 Atlas CLI 或迁移镜像。二进制和 schema 历史保持为一个可审计单元；备份与分阶段兼容，而不是反向 DDL，构成回滚边界。PostgreSQL 迁移锁和双方言升级 fixture 将成为发布门禁。

## 参考资料

- Grafana 在其[迁移器](https://github.com/grafana/grafana/blob/5342253d0a4b144ab825c8ce055c7cff22fb105e/pkg/services/sqlstore/migrator/migrator.go#L128-L185)中显式注册迁移 ID，并在 `migration_log` 中记录 SQL、成功、错误和时间戳。
- Grafana 在 SQLStore 初始化期间执行待处理迁移，并在错误时停止，参见 [`sqlstore.go`](https://github.com/grafana/grafana/blob/5342253d0a4b144ab825c8ce055c7cff22fb105e/pkg/services/sqlstore/sqlstore.go#L67-L89)。
- 其 PostgreSQL 路径在 [`migrator.go`](https://github.com/grafana/grafana/blob/5342253d0a4b144ab825c8ce055c7cff22fb105e/pkg/services/sqlstore/migrator/migrator.go#L198-L255) 与 [`postgres_dialect.go`](https://github.com/grafana/grafana/blob/5342253d0a4b144ab825c8ce055c7cff22fb105e/pkg/services/sqlstore/migrator/postgres_dialect.go#L297-L348) 中用 advisory-lock 生命周期包裹迁移执行。
- Grafana 官方运维指南要求升级前备份数据库：[备份 Grafana](https://grafana.com/docs/grafana/latest/administration/back-up-grafana/)。
- lazy-observe 当前未协调的迁移路径是 [`runMigrations`](../../../internal/meta/store.go)；Helm [`startupProbe`](../../../deploy/helm/lazy-observe/templates/deployment.yaml) 定义了实现必须保留的外层启动预算。

Grafana 当前 PostgreSQL 实现只尝试一次非阻塞的 `pg_try_advisory_lock`，尽管存在超时设置；本文规定的有界等待是 lazy-observe 的加固措施，不是对 Grafana 行为的描述。
