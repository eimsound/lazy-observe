# 管理员密码重置 CLI 与初始密码文件清理实施计划

## 目标与来源

- 源需求：本切片任务说明（`feat: 新增管理员密码重置 CLI，改密后删除初始密码文件`）。
- 最终状态：
  1. `lazy-observe admin reset-password` 只连元数据库（不经 HTTP）为 `admin`（或 `--username` 指定用户）设置新密码，
     吊销该用户全部会话，`--password-file` 与自动生成两条来源都可用，明文只落到 0600 文件或部署者自己的文件里。
  2. 由 bootstrap 生成的 `<data_dir>/initial-admin-password` 在 admin 成功改密后被删除；删除失败只记 `slog.Warn`，
     不影响改密；`security.admin_password_file` 指向的文件永不被删除。
  3. `docs/deployment.md` 的「First admin password」一节写明 CLI 用法。
- 范围外：不改 Ent schema、不改基线 SQL、不动 HTTP API 契约、不加依赖、不改 `requirements.md`。

## 仓库证据

- `internal/meta/bootstrap_admin.go:14`：`InitialAdminPasswordFileName = "initial-admin-password"`；`:19`
  密码下限 8；`:42` `readAdminPasswordFile` 去首尾空白并校验长度；`:63` `writeInitialAdminPassword`
  原子写、0600。
- `internal/meta/store.go:1161` `ensureBootstrapAdmin`：`admin_password_file` 存在时不生成文件，
  `createBootstrapAdmin(..., false)`；`:1218` `createBootstrapAdmin` 写 `must_change_password`；
  `:1243` `randomPassword` 生成 24 字符 base64url 口令（未导出）。
- `internal/meta/store.go:905` `ChangePassword`（当前口令校验 + 清 `must_change_password`）；
  `:946` `DeleteOtherAccountSessions(userID, keepToken)` 是现成的「保留一个会话、删其余」函数；
  `:746` `AuthenticateCredentials` 只用用户名+口令，不涉及会话。
- `internal/account/account.go:341` `Manager.ChangePassword`：先改密再 `sessions.RevokeOthers`，
  失败返回 `CodeUnavailable`，说明改密与会话吊销是两个步骤。
- `internal/account/session.go:32` `Sessions` 接口有 `RevokeOthers(ctx, userID, keepToken)`；
  `metadataSessions.RevokeOthers`（`:104`）→ `DeleteOtherAccountSessions`，
  `stateSessions.RevokeOthers`（`:166`）→ `state.SessionStore.DeleteOtherByUser`。两实现在
  `keepToken == ""` 时都删掉该用户的全部会话（存储的是 token 摘要，空串摘要不可能命中）。
- `internal/gateway/gateway.go:498` 是唯一的自助改密入口，`:1225`/`:1235` 是
  `must_change_password` 的放行表与 403 逻辑；`internal/gateway/account_api.go:78` 是
  `changePassword` handler，成功即改密完成，是「改密成功」这一事实的唯一落点。
- `cmd/lazy-observe/server_dependencies.go:86` `openServerMetadata` 决定元数据库地址
  （`meta.db_url` 或 `<data_dir>/meta/lazy-observe.db`）；`:137` `bootstrapAdmin` 决定
  「部署者文件 vs 生成文件」。`cmd/lazy-observe/command.go:17` 是子命令注册点。
- `internal/meta/bootstrap_admin_test.go` 已用 `newRecordingLogger` 断言日志不含明文；
  `internal/gateway/account_api_test.go:44` `TestChangePasswordEndpoint` 是改密 HTTP 接缝，
  `internal/account/session_test.go` 用真实 SQLite 元数据库 + miniredis 覆盖两个会话适配器。
- ADR 0006：`must_change_password` 每次请求从元数据读取，改密立即对所有副本生效。

## 实施策略

- 推荐路线（采用）：
  - **文件删除放 gateway**。`gateway.Config` 新增 `InitialAdminPasswordFile string`，由
    `cmd/lazy-observe` 从配置算出并注入；`changePassword` handler 在 `account.ChangePassword`
    成功后、且用户名为 `admin`、且路径非空时删除该文件，失败只 `g.logger.Warn`。
    理由：文件路径是部署配置（`data_dir`）的知识，只有 `cmd` 层掌握；gateway 已持有改密成功的
    请求边界、`slog.Logger` 和 HTTP 接缝（可直接测「改密后文件消失」）。`account` 保持传输无关，
    `meta` 保持只碰数据库——把 `data_dir` 塞进这两层会让 HA 共享库的语义更难看清。
  - **重置密码的编排放 `account` 包的新函数** `ResetPassword(ctx, metadata, sessions, username,
    newPassword, mustChangePassword)`：先算哈希、再查用户（新增 `meta.Store.UserByUsername`）、
    吊销一次会话、写哈希与 `must_change_password`（新增 `meta.Store.SetPasswordHash`，接受已算好的
    哈希）、再吊销一次会话。CLI 只负责参数、口令来源与会话后端的适配器选择（与 server 一致：
    Redis 开启用 `account.NewStateSessions`，否则 `account.NewMetadataSessions`），复用
    `openServerSharedState`。
    理由：满足「重置后旧口令失效 / 新口令可用 / flag 正确 / 会话被吊销」这四条可在
    account 层用真实 SQLite + miniredis 断开红→绿证明，CLI 测试只需证明文件与退出码。
    两次吊销 + 哈希先算（C4 N1）：单次「先吊销后写」在写哈希和吊销之间留下窗口，持旧口令的人
    可以登录并拿到不被吊销的新会话；Argon2 放在吊销之后又会拉长窗口。
  - **口令来源**：`--password-file` 复用 `meta.ReadAdminPasswordFile`（把现有
    `readAdminPasswordFile` 导出）；自动生成复用一个新增的
    `meta.GenerateInitialAdminPassword(path)`（`randomPassword` + `writeInitialAdminPassword`），
    只把路径写到 stdout，从不打印口令。自动生成只允许 `admin`，且 `security.admin_password_file`
    必须为空（C4 B2/B3）：否则要么抢走 admin 的 bootstrap 文件，要么覆盖部署者的文件。
    生成前文件已存在时先打印 `replacing existing <path>`（不打印内容），失败清理只删本次新建的文件。
  - **HA 安全**：`cmd` 只在 `security.admin_password_file` 与生成路径不是同一个文件（路径或
    `os.SameFile` 身份，C4 N4）时才注入 `InitialAdminPasswordFile`；HA 必须配
    `admin_password_file`（通常 `/etc/...`），与 `<data_dir>/initial-admin-password` 不同名同址，
    因此永远不会删到部署者的文件。删除失败中「文件已不存在」保持静默，只有真正删不掉才 Warn（C4 b）。
- 备选路线（否决）：
  - **删文件放 `account.Manager.ChangePassword`**（`Options` 增加 `InitialAdminPasswordFile` 或回调）。
    否决理由：`account` 是传输无关域包，`data_dir` 属于部署配置；把文件系统副作用塞进去后，
    两个会话适配器和所有 `account` 测试都要带上一个与业务无关的路径，而且 CLI 与 HTTP 之外的
    调用方（MCP、嵌入式用法）会继承一个无意义参数。
  - **删文件放 `meta`**（例如 `Store.ChangePassword` 内部删）。否决理由：`meta` 只知道 `InitialAdminPasswordFileName`，
    不知道 `data_dir`；要让 store 记住路径就得把 bootstrap 配置挂到 `Store` 上，多副本共享库时
    「哪个副本写的文件」更含糊，且 store 测试需要伪造文件系统。
  - **重置完全放 `meta.Store`（一条 SQL 事务里连会话一起删）**。否决理由：会话不一定在元数据库里
    （`redis.enabled` 时在 Redis，见 `server_dependencies.go` 的 `openServerSharedState`），
    store 层吊销不到，HA 会留下可用会话；放入口更低的 `account.ResetPassword` 才能同时覆盖两种后端。

## 已确认的测试接缝

- `account.ResetPassword`（新导出函数，真实 SQLite 元数据库 + metadata/Redis 两个 `Sessions` 适配器）：
  证明旧口令失效、新口令可用、`must_change_password` 取值正确、会话全部吊销。
- `meta.Store.UserByUsername` / `meta.Store.ResetPassword`（放在上一条的测试里间接覆盖；
  用户不存在时返回 `meta.IsNotFound` 可判定的错误）。
- HTTP `POST /api/auth/password`（`newMetadataTestGateway`）：证明 admin 改密成功后生成文件消失、
  非 admin 改密不删、`InitialAdminPasswordFile` 为空（部署者提供文件）时不删。
- `run([]string{"admin","reset-password", ...})`（`cmd/lazy-observe` 包内，临时 SQLite）：
  证明自动生成文件 0600 与 stdout 路径、`--password-file` 去空白后生效且不打印明文、
  用户不存在返回错误（非零退出）。
- 复用 `meta` 包 `newRecordingLogger` 断言日志不含明文（CLI 侧不引入日志，只有 stdout 路径）。

## 依赖顺序

1. meta：`UserByUsername`、`ResetPassword`、导出 `ReadAdminPasswordFile`、`GenerateInitialAdminPassword`、
   `BootstrapAdminUsername` 常量。
2. account：`ResetPassword` 编排 + 测试（红→绿）。
3. cmd：`admin reset-password` 子命令 + 测试（红→绿）。
4. gateway：`InitialAdminPasswordFile` + `changePassword` 删除逻辑 + 测试（红→绿）。
5. docs：`docs/deployment.md` 用法说明。
6. 门禁 + 单 commit。

## 实施步骤

- [x] 1. `meta` 增加重置所需的最小 API
  - 交付：可查询用户、可无当前口令地改密并显式设置 flag；口令来源函数可复用。
  - 阻塞于：无
  - 涉及位置：`internal/meta/store.go`、`internal/meta/bootstrap_admin.go`
  - Red：`internal/account/reset_test.go` 里对 `account.ResetPassword` 的断言先编译失败/行为失败。
  - Green：`UserByUsername`（ent `UsernameEQ`）、`SetPasswordHash`（哈希由调用方先算好，便于把
    Argon2 放在第一次吊销之前）、`ReadAdminPasswordFile`（导出现有实现）、
    `GenerateInitialAdminPassword(path)`（`randomPassword` + `writeInitialAdminPassword`）。
  - 验证：`CGO_ENABLED=0 go test ./internal/meta/ ./internal/account/`
  - 验收：`go build ./...` 通过；上一条测试由红转绿。
- [x] 2. `account.ResetPassword` 编排
  - 交付：按用户名重置口令并吊销全部会话；用户不存在返回 `CodeNotFound`。
  - 阻塞于：1
  - 涉及位置：`internal/account/`（新文件或 `account.go` 末尾）
  - Red：`internal/account/reset_test.go` 中断言旧口令失败、新口令成功、`must_change_password`、
    `sessions.List` 为空；当前无此函数 → 编译失败即红。
  - Green：算好哈希 → 查用户 → `sessions.RevokeOthers(ctx, id, "")` → `metadata.SetPasswordHash`
    → 再 `sessions.RevokeOthers`。第一次吊销失败时旧口令仍有效，CLI 可安全删除刚生成的文件；
    第二次吊销失败时口令已经生效，返回 `StageSessionRevokeAfterReset`，调用方必须保留生成文件。
  - 验证：`CGO_ENABLED=0 go test ./internal/account/ -run ResetPassword -v`
  - 验收：metadata 与 miniredis 两个适配器都绿。
- [x] 3. `lazy-observe admin reset-password`
  - 交付：命令可用，两条口令来源，用户不存在非零退出，明文不外泄。
  - 阻塞于：2
  - 涉及位置：`cmd/lazy-observe/admin.go`（新）、`cmd/lazy-observe/command.go`、
    `cmd/lazy-observe/server_dependencies.go`（抽出 `openMetadataStore`、`initialAdminPasswordPath`）
  - Red：`cmd/lazy-observe/admin_test.go`：先跑命令再断言文件/认证/退出错误；命令不存在时红。
  - Green：cobra 子命令，`--username`（默认 `admin`）、`--password-file`。顺序固定为
    「开库/开共享状态 → `UserByUsername` → 取口令 → 重置 → 审计」：用户名打错或后端连不上时
    不碰任何文件（C4 B1）。自动生成只对 `admin` 且 `admin_password_file` 为空时开放（B2/B3），
    flag 语义：生成 → `must_change_password=true`，文件 → `false`。写审计事件
    `auth.password_reset`（`ActorUsername=cli`，C4 c）；Redis 模式下重置成功后
    `account.ClearLoginFailures`（C4 N2）。失败清理只删本次新建的生成文件。
  - 验证：`CGO_ENABLED=0 go test ./cmd/lazy-observe/ -run ResetPassword -v`
  - 验收：生成文件 mode 0600、stdout 含路径且不含口令、`--password-file` 内容不被 CLI 改动。
- [x] 4. 改密后删除初始密码文件
  - 交付：admin 通过 HTTP 改密成功后，bootstrap 生成的初始密码文件消失；其它情况不动文件。
  - 阻塞于：2（与 3 并行无依赖）
  - 涉及位置：`internal/gateway/gateway.go`（`Config` + handler）、`internal/gateway/account_api.go`、
    `cmd/lazy-observe/server_gateway.go`（注入）
  - Red：`internal/gateway/account_api_test.go` 新增三个用例（admin 删、非 admin 不删、
    路径为空不删），当前字段不存在 → 红。
  - Green：成功改密后，`g.initialAdminPasswordFile != "" && username == meta.BootstrapAdminUsername`
    时 `os.Remove`；`os.ErrNotExist` 静默，其余失败 `g.logger.Warn`（带 path/username/err），
    绝不影响响应（C4 b）。
  - 验证：`CGO_ENABLED=0 go test ./internal/gateway/ -run ChangePassword -v`
  - 验收：文件删除与保留断言都绿，改密响应仍是 204。
- [x] 5. 用户文档
  - 交付：`docs/deployment.md`「First admin password」补一段英文用法（生成与 `--password-file`、
    用户不存在、HA 仍应使用 `admin_password_file`、改密后文件被删除）。
  - 阻塞于：3
  - 验证：人工阅读；`mise run fmt-check` 不含 md。
  - 验收：命令示例与实现一致。

## Green 后审查与重构

- [x] 检查 `bootstrapAdmin` 与新 `initialAdminPasswordPath` 不重复计算路径；两者共用一个 helper。
- [x] 跑 `golines -m 120 --chain-split-dots -w` 于改动的 Go 文件（不动 `internal/db/ent`）。
- [x] 重跑 `CGO_ENABLED=0 go test ./internal/meta/ ./internal/account/ ./internal/gateway/ ./cmd/lazy-observe/`。

## 最终验证

- [x] `mise run vet`
- [x] `mise run test`
- [x] `mise run fmt-check`
- [x] `GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint`
- [x] 未改 Ent schema / 基线 SQL / 网关路由 → 不跑 migration、ent-generate-check、test-meta-pg、e2e-contract-check。
- [x] 单个 commit：`feat: 新增管理员密码重置 CLI，改密后删除初始密码文件`

## 未决事项

- 无（本切片范围内）。CLI 的 Redis 会话吊销沿用 `redis.enabled` 配置，与 server 的适配器选择一致；
  HA 部署需要同时能连到共享 Redis，这一点写进文档而不另加开关。

## C4 审查修正（同一 commit）

- B1：CLI 顺序改为先开库/会话、再查用户、最后生成文件；失败只删本次新建的生成文件。
- B2：只有 `admin` 可以自动生成；其他账号必须 `--password-file`。
- B3：只要配置了 `security.admin_password_file`（无论路径）就拒绝自动生成。
- N1：`account.ResetPassword` 改为哈希先算、吊销两次，并新增
  `StageSessionRevokeAfterReset` 供 CLI 保留生成文件并提示 rerun。
- N2：Redis 模式下重置成功后清登录锁定（`account.ClearLoginFailures`），单机内存锁定写进文档。
- N3：修正 gateway `InitialAdminPasswordFile` 与其 handler 的注释（只有同一文件时才为空）。
- N4：同文件判断加 `os.SameFile`，CLI 与 gateway 共用 `gatewayInitialAdminPasswordFile`。
- (b)：gateway 删除时 `os.ErrNotExist` 静默。
- (c)：CLI 写 `auth.password_reset` 审计事件，actor 为 `cli`。

## 执行日志

- 2026-11-18 19:30 `git fetch origin && git switch -c slice/admin-reset-password-1 origin/main` → 分支在 `9ee0fac` 上创建。
- 2026-11-18 19:35 `CGO_ENABLED=0 go test ./internal/account/ -run TestResetPassword` → 红：`undefined: account.ResetPassword`。
- 2026-11-18 19:36 `CGO_ENABLED=0 go test ./internal/gateway/ -run TestChangePassword` → 红：`unknown field InitialAdminPasswordFile`。
- 2026-11-18 19:37 `CGO_ENABLED=0 go test ./cmd/lazy-observe/ -run TestAdminResetPassword` → 红：`unknown flag: --data-dir`。
- 2026-11-18 19:45 `CGO_ENABLED=0 go test ./internal/account/ -run TestResetPassword` → 绿（metadata + redis 适配器）。
- 2026-11-18 19:50 `CGO_ENABLED=0 go test ./internal/gateway/ -run TestChangePassword` → 绿（4 个用例）。
- 2026-11-18 19:52 `CGO_ENABLED=0 go test ./cmd/lazy-observe/ -run 'TestAdminResetPassword|TestGatewayInitialAdminPasswordFile' -v` → 绿（5 个用例）。
- 2026-11-18 19:54 `CGO_ENABLED=0 go test ./internal/meta/ ./internal/account/ ./internal/gateway/ ./cmd/lazy-observe/` → 全绿。
- 2026-11-18 19:56 `mise run vet` → `Finished in 2.61s`，无输出错误。
- 2026-11-18 19:57 `mise run fmt-check` → EXIT=0。
- 2026-11-18 19:58 `mise run test` → EXIT=0，`Finished in 7.47s`。
- 2026-11-18 19:59 `GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint` → `0 issues.`，EXIT=0。
- 2026-11-18 20:05 审查发现「先写哈希后吊销」会让失败路径删掉唯一的口令文件，改为「先吊销后写哈希」，
  新增 `TestResetPasswordKeepsTheOldPasswordWhenRevocationFails` → 绿。
- 2026-11-18 20:07 `mise run vet` → EXIT=0；`mise run fmt-check` → EXIT=0。
- 2026-11-18 20:08 `mise run test` → EXIT=0（cmd/account/gateway/meta 全 ok）。
- 2026-11-18 20:09 `GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint` → `0 issues.`，EXIT=0。
- 2026-11-18 20:12 `go build -o /tmp/lo-admin-smoke ./cmd/lazy-observe && /tmp/lo-admin-smoke admin reset-password --data-dir <tmp>`
  → 退出码 1，`lazy-observe: user "admin" does not exist`，`<tmp>` 下只剩 `meta/`（生成的文件已被清理）。
- 2026-11-18 20:13 补 `TestRootCommandHelpAndVersion` 的 `admin reset-password --help` 断言 → 绿。
- 2026-11-18 20:14 `mise run fmt-check` → EXIT=0；`GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint` → `0 issues.`。
- 2026-11-19 04:07 `git fetch origin && git rebase origin/main` → 变基到 `07f0991`，无冲突。
- 2026-11-19 04:20 `CGO_ENABLED=0 go test ./internal/account/ -run TestResetPassword -v` → 4 个用例全绿
  （含新增 `TestResetPasswordReportsThePostWriteRevocationFailure`）。
- 2026-11-19 04:25 `CGO_ENABLED=0 go test ./cmd/lazy-observe/ -run 'TestAdminResetPassword|TestGatewayInitialAdminPasswordFile' -v`
  → 10 个用例全绿（新增 B1/B2/B3 两个场景、Redis 锁定、审计、symlink 同文件）。
- 2026-11-19 04:28 `CGO_ENABLED=0 go test ./internal/gateway/ -run TestChangePassword -v` → 全绿
  （新增「文件已删不告警」「真删不掉才告警」）。
- 2026-11-19 04:13 真实二进制 + 真实 server 复现（`mise run dev-storage` 下载 storage bins，
  `go build -o /tmp/lo ./cmd/lazy-observe`，server 监听 127.0.0.1:43111，临时 data-dir 完成 bootstrap）：
  - B1 `admin reset-password --username ghost` → 退出码 1，`initial-admin-password` 的 sha256 前后一致。
  - admin 用 bootstrap 口令登录并改密 → 生成文件被 gateway 删除；再经 HTTP 创建 `bob`。
  - B2 `admin reset-password --username bob` → 退出码 1，提示必须给 `--password-file`，未写生成文件；
    `--password-file` 后 bob 可用新口令登录，admin 原口令仍可登录。
  - B3 `--config` 配 `security.admin_password_file`（同路径 / 不同路径各一次）→ 两次都退出码 1，
    部署者文件内容与生成路径均未被改动。
  - CLI 对已存在的生成文件：先打印 `replacing existing <path>` 再写；随后登录 → `must_change_password=true`
    → 改密 → 文件删除；CLI 重置前签发的会话在重置后返回 401。
- 2026-11-19 04:30 `mise run vet` / `mise run fmt-check` / `mise run test` /
  `GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint` → 全绿（结果见报告）。
- 未改 Ent schema / 基线 SQL / 路由注册，`migration-check`、`ent-generate-check`、`test-meta-pg`、
  `e2e-contract-check` 不适用于本切片。

