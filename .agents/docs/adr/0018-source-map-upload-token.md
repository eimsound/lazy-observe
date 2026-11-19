# Source Map 上传使用独立的项目级保密 Token

`POST /api/releases/:release/sourcemaps` 原先和摄取路由一样只认项目 API Token。按 ADR 0010，这个 token 会写进公开页面，而上传接口对同一 release 下的同名文件直接覆盖（`UpsertSourceMapArtifact`），release 名又由上传方任意指定、数量没有上限。结果是任何访客都能替换已发布 release 的 Source Map，把报错堆栈还原到伪造的源码位置，还能不断创建新 release 把存储写满；而删除 Source Map 反而要求 admin 用户凭据。我们决定新增项目级的 **Source Map 上传 Token**（前缀 `los_`）：它是保密凭据，只放在接入方 CI 的 secret 里，只能为所属项目上传 Source Map，不能摄取也不能查询；由 admin（全局角色，与项目 API Token 的管理权限相同）创建和吊销，值只在创建时显示一次。上传接口只接受这种 token，项目 API Token 和用户 API Token 访问时返回 403。DSN 形状不变（`<origin>/p/<project>@<token>`），bundler-plugin 和 CLI 照常使用 DSN，只是其中放的是上传 Token。

## 决策

- **独立的表 `source_map_upload_tokens`**，字段与 `project_api_tokens` 相同（`token` 存 SHA-256 摘要、`prefix`、`revoked`、`created_at`、`last_used_at`、`project_id`），不在 `project_api_tokens` 上加 `kind` 字段。摄取鉴权读的是 `project_api_tokens`，物理分表让 `los_` 在结构上不可能被摄取路径认出来，不依赖每条查询记得带上 `kind` 过滤；加字段则要改所有现有的 token 查询、自监控 token 引导和两份基线里的唯一约束语义。代价是多一张表和一套几乎重复的存取代码，可以接受。
- **鉴权分层**：上传路由用独立的 `Authenticator`，只查上传 Token 表。`lo_`（项目 API Token）与 `lou_`（用户 API Token）返回 `403`，错误码 `source_map_upload_token_required`；缺失、未知、已吊销返回 `401`。`los_` 用于其它任何路由（摄取、查询、`/mcp`、管理 API）返回 `403`，错误码 `upload_only_token`。
- **管理**：`GET|POST /api/projects/:id/source-map-upload-tokens` 和 `DELETE /api/projects/:id/source-map-upload-tokens/:tokenID`，都要求 admin（列表也是，因为它暴露保密凭据的存在与最近使用时间）。创建与吊销写审计 `source_map_upload_token.create` / `source_map_upload_token.revoke`，审计不含 token 值。
- **吊销与 `last_used_at`** 与项目 API Token 一致：吊销后本副本立即失效（认证缓存按存储 revision 清空），其它副本最迟在 30 秒缓存过期后失效；`last_used_at` 至多每分钟更新一次，更新失败不影响上传。

## 备选方案

- **上传改用用户 API Token（拒绝）**：能沿用 ADR 0010 的现成机制，但凭据绑定个人，成员离开后 CI 就会失效；用户 Token 还带读权限，比上传需要的大。
- **项目 API Token 不变，只禁止覆盖（拒绝）**：挡得住篡改已发布的 release，挡不住塞满存储，也挡不住抢先上传还没发布的 release。
- **做成通用的「发布 / CI Token」（暂不做）**：目前 CI 只需要上传 Source Map；以后有其他 CI 写入需求时，再决定是扩大这个 token 的权限还是另设一种。
- **在 `project_api_tokens` 加 `kind` 字段（拒绝）**：见上，分表的失败方式更安全。

## 后果

- 协议面变更：`docs/version-matrix.md`、服务端、SDK 文档（bundler-plugin README、`docs/sdk/browser.md`）、web 和 browser journey 在同一个 PR 里修改。项目处于发布前阶段（ADR 0003），基线 SQL 直接加表，不保留项目 API Token 上传的旧行为。
- 是对 ADR 0010 的补充，不改它的正文：项目 API Token 仍然只能用于摄取，Source Map 上传不算摄取。
- 上传 Token 泄露的影响是持有者能覆盖该项目的 Source Map 并占用存储，范围限于一个项目；处置办法是吊销并重建，审计记录可查创建与吊销。上传仍受现有的单 release 文件数与大小上限约束，没有新增每项目的总量配额。
