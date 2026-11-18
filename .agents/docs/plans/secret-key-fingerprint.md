# 元数据库记录平台密钥指纹实施计划

## 目标与来源

- 源需求：本切片任务说明（`feat: 元数据库记录平台密钥指纹，启动时密钥不一致即拒绝启动`）。
- 最终状态：
  1. 元数据库里有一行平台密钥指纹；首次启动写入当前指纹，之后每次启动比对。
  2. 指纹不一致时服务以非零码退出（`main` 里 `os.Exit(1)`），报错含密钥文件路径、元数据库记录值、
     当前值、可能原因（Secret 被重新生成 / 多副本未共享密钥）和恢复方式（换回原密钥，或确认轮换后跑
     `lazy-observe admin reset-key-fingerprint`）。
  3. 多副本并发首次启动只有一个指纹生效，其余副本读到它再比对。
  4. 轮换出口：`lazy-observe admin reset-key-fingerprint` 显式覆盖记录，写一条审计事件，并提示旧密文
     已解不开、需要重新配置 AI key。
  5. `docs/deployment.md`「Platform encryption key」写明新行为、报错含义、恢复与轮换步骤。
- 范围外：不做自动轮换、不迁移/重封存旧密文、不改 `requirements.md`、不改 CHANGELOG、不加依赖、
  不动 HTTP API 契约、不改 `doctor`（见「未决事项」）。

## 仓库证据

- `internal/secret/box.go`：`Box` 只有 `key [32]byte`；`:158` `Derive(purpose)` 已经是
  `HMAC-SHA256(key, "lazy-observe/derive/v1\x00"+purpose)` 的十六进制输出，说明「用平台密钥做 HMAC
  得到稳定标识」是本仓库既有做法。
- `cmd/lazy-observe/server_dependencies.go:28` `loadSecretBox` 决定密钥文件（`security.secret_key_file`
  或 `<data_dir>/secret.key`）；`:86` `openServerMetadata` 是启动时打开元数据库的唯一地方，顺序是
  `openMetadataStore` → `openBlobStore` → `Bootstrap` → `BootstrapSelfMonitoring`
  （`:117` 用 `secretBox.Derive("self-monitoring-project-token")` 派生自监控 token）。
  指纹校验必须排在 `BootstrapSelfMonitoring` 之前，否则密钥被换掉时会先悄悄重新派生 token。
- `internal/meta/store.go:1582` `bind` 把 `?` 换成 `$n`，是双方言手写 SQL 的既有做法；
  `internal/meta/ai_settings.go:51` `UpsertAISettings` 用 `INSERT ... ON CONFLICT (id) DO UPDATE`
  手写 SQL 写单行表、用 Ent 读回，证明「单行表 + 双方言 upsert」可行。
- `internal/db/schema/aisetting.go` + 两个基线里的 `ai_settings`（`id INTEGER PRIMARY KEY CHECK (id = 1)`）
  是单行表惯例；`internal/meta/migrations/001_baseline.sql:407` 是文件末尾，新表追加在其后。
- `rg 'key_value|setting' internal/db/schema`：只有 `ai_settings`（强类型单行配置）与
  `profile_metas`（pprof 索引），**没有**通用键值/设置表可复用。
- ADR 0003：schema 变更直接改配对的 `001_baseline.sql`，开发库需要重建。
- ADR 0004：Ent 留在元数据持久化接缝之后；调用方只用 `meta.Store` 的行为方法，不碰生成的客户端。
- `cmd/lazy-observe/admin.go:31` `newAdminResetPasswordCommand` 是 CLI 子命令样板；
  `:222` `recordResetAudit` 用 `ActorUsername = "cli"` 写审计。
- `internal/meta/dialects_test.go:67` `forEachDialect` 提供 SQLite/PG 双方言测试；`:154`
  `TestPostgresConcurrentBaselineStartup` 是多副本并发首次启动的样板。
- 现有静默失败路径：`internal/gateway/ai_settings.go:97` `loadFromStore` 只打一条 Error，AI 保持关闭。

## 实施策略

- 推荐路线（采用）：**新增最小单行表 `platform_key_fingerprints`，指纹在 `secret` 包算，
  比对在 `cmd` 启动路径做，轮换走 CLI。**
  - 存储：新增 Ent schema `PlatformKeyFingerprint` + 两个基线各加一张单行表
    （`id INTEGER PRIMARY KEY CHECK (id = 1)`、`fingerprint`、`created_at`、`updated_at`）。
  - `internal/secret`：`Box.Fingerprint()` 返回 `hex(HMAC-SHA256(key, "lazy-observe/key-fingerprint/v1")[:16])`。
  - `internal/meta/platform_key.go`：`EnsurePlatformKeyFingerprint(ctx, fingerprint) (record, created, err)`
    （`INSERT ... ON CONFLICT (id) DO NOTHING` + Ent 读回）与
    `ReplacePlatformKeyFingerprint(ctx, fingerprint) (record, err)`（`ON CONFLICT (id) DO UPDATE`）。
  - `cmd/lazy-observe/server_dependencies.go`：`secretKeyFile(cfg)` 抽出路径；`openServerMetadata`
    在 `Bootstrap` 之前调用 `ensurePlatformKeyFingerprint`，不一致就返回带完整恢复指引的错误。
  - `cmd/lazy-observe/admin.go`：`admin reset-key-fingerprint` 覆盖记录 + 审计事件。
- 备选路线（否决）：
  - **通用键值表 `platform_settings(key, value, updated_at)`**：一张表容纳未来所有「启动期必须一致」
    的东西。否决理由：当前只有一个消费者，通用键值表会把类型、并发语义和校验都推给调用方，等于引入
    一个只有一个生产适配器的假接缝（ADR 0004 的同一个理由），且无法用 `CHECK (id = 1)` 在 schema 层
    钉住单行不变量。留到真有第二、第三个启动期不变量时再抽。
  - **不落库，启动时把密钥指纹写进日志/file marker（`<data_dir>/secret-key-fingerprint`）**：
    否决理由：多副本不共享 `data_dir`，HA 与 GitOps 场景恰好不共享文件系统，这个 file marker 在最需要
    它的场景里无效；元数据库本来就是多副本共同真相。
  - **把指纹也用作派生用途（复用 `Box.Derive("platform-key-fingerprint")` 的 64 位十六进制）**：
    否决理由：`Derive` 的用途命名空间用来派生凭据（自监控 token 就存在元数据库里），把它的 256 位
    输出整段存进库里没有收益；指纹只需要不可碰撞和不可逆，截断到 128 位更省事，也限制泄露面。

### 指纹算法与理由

`hex(HMAC-SHA256(key, "lazy-observe/key-fingerprint/v1")[:16])`：

- **不泄露密钥**：HMAC 是单向的，128 位截断值是密钥的压缩摘要，反推 256 位密钥不可行。
- **为什么不是裸 SHA-256**：固定标签 + 密钥做 HMAC 让输出绑定用途标签，跟 `Derive` 的域分隔做法一致，
  不会与「密钥本身的哈希」这类外部工具输出混淆。
- **关于「不能用来离线验证猜测的密钥」**：任何确定性指纹都允许拿到库的人对**完整**密钥候选做一次
  HMAC 比对——这是指纹的定义决定的，除非指纹不稳定。真正的防线是密钥本身是 32 字节随机数
  （2^256 猜测空间），以及截断到 128 位把泄露面压到最小。计划里明确记录这一点，不假装指纹能防离线
  猜测；它防的是「密钥被悄悄换掉」这种运维事故。
- 比对在 Go 里用 `==` 比较两个十六进制字符串：值不是密钥本身，时序泄露没有意义；但日志与报错只出现
  截断指纹，永远不出现密钥的 base64/hex 编码。

## 已确认的测试接缝

- `secret.Box.Fingerprint()`（`internal/secret` 包内，真实密钥文件）：同一密钥稳定、不同密钥不同、
  长度 32 个十六进制字符、不等于密钥的任何编码。
- `meta.Store.EnsurePlatformKeyFingerprint` / `ReplacePlatformKeyFingerprint`（`forEachDialect`，
  真实 SQLite + 真实 PG）：首次写入 `created=true`；再次调用返回同值 `created=false`；不同指纹返回
  已记录值；`Replace` 覆盖；并发首写收敛到一个值（SQLite 与 PG 双方言）。
- `openServerMetadata`（`cmd/lazy-observe` 包内，真实 SQLite 元数据库 + 真实密钥文件）：这正是启动
  路径调用的依赖函数——首次启动记录指纹、同密钥二次启动通过、换密钥后返回错误且元数据库里的自监控
  token 没有被重新派生、日志/错误里没有密钥字节。
- `run(["admin","reset-key-fingerprint", ...])`（`cmd/lazy-observe` 包内）：覆盖记录、打印新指纹、
  不改密钥文件、之后启动通过。

## 依赖顺序

1. `secret.Fingerprint`（无依赖）。
2. Ent schema + 两个基线加表 + `meta` 的两个方法（依赖 1 只为了测试里的指纹字符串）。
3. `cmd` 启动校验 + `admin reset-key-fingerprint`（依赖 2）。
4. `docs/deployment.md`（依赖 3 的最终输出形态）。
5. 门禁 + 单 commit。

## 实施步骤

- [x] 1. `secret.Box.Fingerprint`
  - 交付：`Box` 能产出稳定的截断指纹。
  - 阻塞于：无
  - 涉及位置：`internal/secret/box.go`
  - Red：`internal/secret/box_test.go` 新增 `TestFingerprintIsStableAndOpaque`：断言同一文件两次
    `LoadOrCreate` 指纹相同、不同密钥指纹不同、`len(fp) == 32` 且可 hex 解码、指纹里不含密钥的
    base64（标准/无填充）或 hex 编码 → 编译失败即红。
  - Green：`hmac.New(sha256.New, b.key[:])` 写固定标签，`hex.EncodeToString(sum[:16])`。
  - 验证：`CGO_ENABLED=0 go test ./internal/secret/ -run Fingerprint -v`
  - 验收：上述断言全绿，且 `TestKeysDoNotCross` 等既有用例不受影响。
- [x] 2. 元数据表与 `meta` 方法（双方言）
  - 交付：指纹落库、条件首写、显式覆盖。
  - 阻塞于：1
  - 涉及位置：`internal/db/schema/platformkeyfingerprint.go`（新）、
    `internal/meta/migrations/001_baseline.sql`、`internal/meta/migrations/postgres/001_baseline.sql`、
    `internal/meta/platform_key.go`（新）
  - Red：`internal/meta/platform_key_test.go` 用 `forEachDialect` 覆盖首次写入/重复写入/不同指纹/
    `Replace`/并发首写，当前 `undefined: Store.EnsurePlatformKeyFingerprint` → 红。
  - Green：schema 单行表 + 两个基线追加 `CREATE TABLE platform_key_fingerprints`；
    `Ensure` 用 `INSERT ... ON CONFLICT (id) DO NOTHING`，用 `RowsAffected` 判断是否本轮写入，
    再用 Ent `Get(ctx, 1)` 读回；`Replace` 用 `ON CONFLICT (id) DO UPDATE`。
  - 验证：`CGO_ENABLED=0 go test ./internal/meta/ -run PlatformKeyFingerprint -v`（SQLite）；
    `mise run test-meta-pg`（含 PG）。
  - 验收：双方言红→绿；并发首写所有调用方读到同一个指纹；`created` 只有一个调用者为 true。
- [x] 3. 启动校验
  - 交付：密钥不一致时 `openServerMetadata` 返回错误，`server` 以非零码退出。
  - 阻塞于：2
  - 涉及位置：`cmd/lazy-observe/server_dependencies.go`
  - Red：`cmd/lazy-observe/platform_key_test.go` 新增
    `TestServerStartupRecordsAndVerifiesPlatformKeyFingerprint`、
    `TestServerStartupRefusesChangedPlatformKey`，两者都调用
    `assertNoPlatformKeyMaterial`（日志与错误里不出现密钥的 base64/hex 编码）。
    先红在行为上（`startup did not record the platform key fingerprint` /
    `startup accepted a platform key that differs from the recorded fingerprint`）。
  - Green：`secretKeyFile(cfg)` 抽出路径；`ensurePlatformKeyFingerprint` 写入/读回并比对；
    `openServerMetadata` 在 `Bootstrap` 之前调用它。普通启动打 `Info`（含路径、指纹），首次写入打
    `Info`（`recorded`）；不一致时返回的错误包含密钥文件路径、`recorded`/`current` 指纹、
    「Secret 被重新生成 / 多副本未共享密钥」两个原因和 `lazy-observe admin reset-key-fingerprint`
    恢复命令。
  - 验证：`CGO_ENABLED=0 go test ./cmd/lazy-observe/ -run 'PlatformKey|ChangedPlatformKey' -v`
  - 验收：错误文本断言全绿；错误与日志里不出现密钥的 base64/hex；换密钥后元数据库里的自监控 token
    与换之前一致（证明没走 `BootstrapSelfMonitoring`）。
- [x] 4. `admin reset-key-fingerprint`
  - 交付：显式轮换出口，覆盖记录并写审计。
  - 阻塞于：3
  - 涉及位置：`cmd/lazy-observe/admin.go`、`cmd/lazy-observe/command.go`
  - Red：`cmd/lazy-observe/platform_key_test.go` 里 `TestAdminResetKeyFingerprintAcceptsRotation`：
    换密钥后启动报错 → 跑 CLI → 再启动通过、stdout 含新指纹与旧值、密钥文件字节未变 → 命令不存在时红。
  - Green：子命令 `reset-key-fingerprint`（仅配置 flag）：`loadSecretBox` → `openMetadataStore` →
    `ReplacePlatformKeyFingerprint` → 审计事件 `security.key_fingerprint_reset`（actor `cli`）→
    stdout 打印路径/新指纹/旧值/「旧密文已解不开」。审计失败只打 warning，不影响轮换结果。
  - 验证：`CGO_ENABLED=0 go test ./cmd/lazy-observe/ -run ResetKeyFingerprint -v`
  - 验收：CLI 前后密钥文件 sha256 不变；审计行存在；再次启动通过。
- [x] 5. 用户文档
  - 交付：`docs/deployment.md`「Platform encryption key」写清新行为、报错、恢复、轮换、开发库重建。
  - 阻塞于：4
  - Red：无自动断言（md）。
  - Green：更新该节 + 在迁移段落写明 schema 改动需要重建开发库。
  - 验证：人工阅读；`mise run fmt-check` 不覆盖 md。
  - 验收：命令、报错文案与实现一致。

## Green 后审查与重构

- [x] 检查 `loadSecretBox` 与 `ensurePlatformKeyFingerprint` 共用 `secretKeyFile(cfg)`，不重复算路径。
- [x] 检查 `internal/db/ent` 未被跟踪、未被 golines 触碰（`git status --short`、`git check-ignore`）。
- [x] 用 `golines -m 120 --chain-split-dots -w` 格式化改动的 Go 文件。
- [x] 重跑 `CGO_ENABLED=0 go test ./internal/secret/ ./internal/meta/ ./cmd/lazy-observe/`。
- [x] 轮换出口额外返回「上一个指纹」：新增 `meta.Store.RecordedPlatformKeyFingerprint`（`ok=false` 表示
  还没记录），CLI 输出 `(previous <fp>|none)`。

## 最终验证

- [x] `mise run vet`
- [x] `mise run test`
- [x] `mise run fmt-check`
- [x] `GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint`
- [x] 改了 schema/基线：`mise run migration-check`、`mise run ent-generate-check`、`mise run test-meta-pg`
- [x] 影响启动路径：`mise run e2e-restart`
- [x] 改了 Helm 渲染规则（K1 B1）：`mise run helm-lint`、`mise run helm-template-test`
- [x] 真实二进制冒烟：首次启动记录指纹；换密钥后 exit 1；CLI 轮换后 `/readyz` 返回 200
- [x] 单个 commit：`feat: 元数据库记录平台密钥指纹，启动时密钥不一致即拒绝启动`

## 未决事项

- `lazy-observe doctor` 是否也报告指纹状态：本切片不做（doctor 目前只查外部状态与 HA 配置），
  留给后续切片决定，避免把「启动门禁」和「诊断输出」混在同一个 commit。
- 指纹算法版本化（例如将来换 KDF）：表里只有一个 `fingerprint` 列，换算法会让所有老库变成「不一致」。
  预发布阶段接受这一点（ADR 0003：老库可重建）；正式发布前若要换算法，需要在那时给列加版本前缀。

## K1 审查修正（同一 commit）

- B1 阻塞：`standalone-ha` 单副本 `replicaCount=1 + autoGenerate=false` 原先落到 `datadir`，
  而 standalone-ha 的 `/data` 是 emptyDir、元数据库在外部 PG：pod 重建后密钥与库里的指纹对不上且旧密钥
  已随 emptyDir 消失。`_helpers.tpl` 改为 standalone-ha 无 Secret 即渲染失败（任意副本数，含 HPA），
  报错说明「密钥必须比 pod 活得久」；`allinone`（PVC）与 `allinone-ephemeral`（共享 Secret / 同卷丢
  库丢钥）行为不变，理由写进 helper 注释。同步 `scripts/helm-template-test.sh`（改为 `assert_render_fails`
  并断言文案）、`values.yaml` 注释、`docs/deployment.md` 密钥与 Helm 两节、`templates/NOTES.txt` 的
  死分支改为 `fail`。
- B2 阻塞：`admin reset-key-fingerprint` 走 `LoadOrCreate`，密钥文件缺失时会生成新密钥并覆盖记录，
  与帮助文本「only read」矛盾。新增只读 `secret.Load`（缺失即报错，带路径），reset 改用它；错误提示
  「挂上 server 读的那个密钥文件」。补测试：缺文件时非零退出、不生成文件、记录不变。
- B3 阻塞：文档「existing deployment … unaffected」错误——基线 SQL 变了，旧库先在
  `metadata migration 1 checksum mismatch` 失败。改为「保留密钥文件、重建元数据库后首启记录当前指纹」，
  并写明不重建会看到 checksum mismatch。
- 建议 1：轮换步骤写明先停掉所有副本（scale 到 0）再跑 reset，并给挂同一 ConfigMap/Secret 的 K8s Job 片段
  与 `kubectl run` 提示。
- 建议 2：reset 的 previous 与 current 相同时打印 `already recorded`，不写库、不写审计；读 previous 失败
  的分支补失败审计；审计 `changed_fields` 记录 `fingerprint <previous> -> <current>`（表里没有专门列，
  只放指纹不放密钥）。
- 建议 3：文档写明密钥必须是 32 字节随机数，不得从口令派生。
- 建议 4：去掉 Ent schema 里多余的 `entsql.Annotation{Table: "platform_key_fingerprints"}`（与
  `ai_settings` 一致，默认表名相同），重跑 `ent-generate` 后表名仍是 `platform_key_fingerprints`。

## C1 修正（PR #60 CI 红：HA E2E）

- 根因：`scripts/e2e-multi-replica.sh` 的 `write_config` / `write_performance_config` 让两个副本
  各自用 `${file%.yaml}-data`，却连同一个 PostgreSQL，又都没配 `security.secret_key_file`。并发首启时
  只有一个副本抢到指纹写入，另一个因不一致退出，CI 报 `timeout waiting for http://127.0.0.1:23101/readyz`。
- 改法：脚本在 `write_config` 之前用 `python3 -c 'import base64, os; print(base64.b64encode(os.urandom(32)).decode())'`
  生成 `$WORK/secret.key`（umask 077；python3 已是脚本的必需前置，不引入 openssl 依赖，也不写
  `| head` 这种后面会被 pipefail 检查拒绝的管道），两个 config 写入函数都加 `secret_key_file`；
  `write_performance_config` 新增 `security:` 段（perf 的两个副本同样共享一个 PG）。
- 排查同类问题：`rg -n 'meta.*db_url|LO_META_DB_URL|data_dir' scripts internal --glob '*.sh' --glob '*_test.go'`
  → 只有该脚本起两个以上副本（`:444/:448` 与 perf 的 `:808/:813`）；`internal/gateway/*_e2e_test.go`
  与 `scripts/e2e-restart.sh` 都是单进程或用同一个 `data_dir` 重启，共享同一把密钥，无需修改。
- 顺手改的非阻塞项：
  - 审计 `changed_fields` 回到约定（逗号拼接的字段名）只记 `fingerprint`；新指纹仍在 `target_name`，
    旧指纹没有合适列，故不入库（只打印给操作者），测试改为断言 `ChangedFields == "fingerprint"`。
  - reset 首次记录（`previous=none`）不再打印「values sealed with the previous key …」，改为
    `(first record)`；新增 `TestAdminResetKeyFingerprintFirstRecordSkipsRotationWarning`。
  - 文档 K8s Job 片段：镜像写「与 Deployment 相同的镜像和 tag」，补 `items` 投影与
    `existingSecret` / 自定义 `encryptionKey.key` 的替换说明。

## 执行日志

- 2026-11-19 04:20 `git fetch origin && git switch -c slice/secret-key-fingerprint-1 origin/main` → 分支建在 `2c26b35`。
- 2026-11-19 04:25 `CGO_ENABLED=0 go test ./internal/secret/ -run Fingerprint` → 红：`first.Fingerprint undefined`。
- 2026-11-19 04:26 `CGO_ENABLED=0 go test ./internal/secret/ -run Fingerprint -v` → 绿（1 个用例）。
- 2026-11-19 04:29 `CGO_ENABLED=0 go test ./internal/meta/ -run PlatformKeyFingerprint` → 红：
  `Store.EnsurePlatformKeyFingerprint undefined`（先跑 `mise run ent-generate` 生成被忽略的 `internal/db/ent`）。
- 2026-11-19 04:33 加 `internal/db/schema/platformkeyfingerprint.go` + 两个基线各追加一张单行表，
  `CGO_ENABLED=0 go test ./internal/meta/ -run PlatformKeyFingerprint -v` → SQLite 全绿，PG 待 `LO_TEST_PG=1`。
- 2026-11-19 04:34 `mise run test-meta-pg` → `ok internal/meta 17.356s`（含并发首写与 schema 等价断言）。
- 2026-11-19 04:36 `CGO_ENABLED=0 go test ./cmd/lazy-observe/ -run PlatformKey` → 红：
  `startup did not record the platform key fingerprint` / `startup accepted a platform key that differs ...`。
- 2026-11-19 04:38 实现 `secretKeyFile` + `ensurePlatformKeyFingerprint` 并接到 `openServerMetadata`
  （`Bootstrap` 之前）→ 绿（2 个用例）。
- 2026-11-19 04:39 `CGO_ENABLED=0 go test ./cmd/lazy-observe/ -run ResetKeyFingerprint` → 红：`unknown flag: --data-dir`。
- 2026-11-19 04:41 加 `admin reset-key-fingerprint` + 审计 → 绿（3 个用例）。
- 2026-11-19 04:43 `CGO_ENABLED=0 go test ./internal/secret/ ./internal/meta/ ./cmd/lazy-observe/` → 全 ok。
- 2026-11-19 04:45 真实二进制（`go build -o /tmp/lo-keyfp ./cmd/lazy-observe`）：
  - 首启（临时 `data_dir`，`--storage-bin-dir .cache/storage-bin`）：INFO `recorded platform encryption key fingerprint`
    `fingerprint=791e2e7dc4725f6b875166b2c3d7a12b`。
  - 用 `openssl rand -base64 32` 覆盖 `secret.key` 再启动 → 退出码 1，报错含路径、两个指纹、原因、恢复命令。
  - `admin reset-key-fingerprint --data-dir <tmp>` → 退出码 0，打印 `(previous 791e… )` 与新指纹 `174e…`，
    密钥文件 sha256 未变。
  - 再启动 → INFO `matches the recorded fingerprint`，`/readyz` = 200。
- 2026-11-19 04:50 `mise run vet` → `Finished in 2.34s`；`mise run test` → `Finished in 17.09s`；
  `mise run fmt-check` → EXIT=0；`GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint` → `0 issues.`
- 2026-11-19 04:55 `mise run migration-check` → `pre-release single-baseline migration check passed` + ok；
  `mise run ent-generate-check` → EXIT=0；`mise run test-meta-pg` → `ok internal/meta 12.125s`。
- 2026-11-19 05:00 `mise run e2e-restart` → 4 条断言全 PASS（graceful / forced / pending-work / managed-child）。
- 2026-11-19 05:20 `git fetch origin && git rebase origin/main` → 变基到 `64497b4`，无冲突；
  `CGO_ENABLED=0 go test ./cmd/lazy-observe` → ok。
- 2026-11-19 05:24 `CGO_ENABLED=0 go test ./internal/secret/ -run LoadNeverCreates` → 红 `undefined: Load`；
  加 `secret.Load` 后 → 绿。
- 2026-11-19 05:28 `CGO_ENABLED=0 go test ./cmd/lazy-observe/ -run 'RefusesMissingKeyFile|IsIdempotent'` → 红：
  reset 在密钥缺失时**成功并覆盖记录**（B2 复现）、已记录时重复写；改用 `secret.Load` + `already recorded`
  短路 + 失败审计 + 审计记录新旧指纹后 → 5 个用例全绿。
- 2026-11-19 05:33 去掉 schema 的 `entsql` 注解，`mise run ent-generate` 后
  `grep 'Table = ' internal/db/ent/platformkeyfingerprint/...` 仍是 `platform_key_fingerprints`。
- 2026-11-19 05:38 Helm：`_helpers.tpl` standalone-ha 无 Secret 一律 fail；`scripts/helm-template-test.sh`
  改为 `assert_render_fails` 并断言文案；`mise run helm-lint` → `1 chart(s) linted, 0 failed`；
  `mise run helm-template-test` → PASS（先有一次 `single-no-key.yaml` 残留 grep 报错，同步清理后 PASS）。
- 2026-11-19 05:55 门禁（K1 修正后串行）：`mise run vet` → `Finished in 2.17s`；`mise run test` →
  `Finished in 16.08s`；`mise run fmt-check` → EXIT=0；`GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint` →
  `0 issues.`；`mise run migration-check` → passed + ok；`mise run ent-generate-check` → EXIT=0；
  `mise run test-meta-pg` → `ok internal/meta 10.756s`；`mise run helm-lint` → `1 chart(s) linted, 0 failed`；
  `mise run helm-template-test` → PASS。
- 2026-11-19 06:00 真实二进制（`go build -o /tmp/lo-keyfp2`）：
  - B2 复现：`admin reset-key-fingerprint --data-dir <空目录>` → 退出码 1，
    `load platform encryption key: read encryption key file <data_dir>/secret.key: … no such file or directory
    (mount the key file the server reads via security.secret_key_file and retry; this command never creates a key)`，
    `find <data_dir> -mindepth 1` 为空（连元数据库都没建）。
  - 端到端：首启 `/readyz` 200 并记录 `73ade83e…`；换密钥后启动退出码 1（报错含两个指纹与恢复命令）；
    reset → `reset1-exit=0` 且打印 `(previous 73ade83e…)`；再跑一次 → `already recorded … nothing to do`；
    再启动 `/readyz` 200。审计 `changed_fields` 由测试断言同时含新旧指纹。
- 2026-11-19 06:10 admin.go 审计参数收敛（`previous` 直接传文本，读失败记为 `unread`）后重跑门禁：
  `mise run vet` → 1.36s；`mise run test` → 1.24s；`mise run fmt-check` → EXIT=0；`lint` → `0 issues.`；
  `migration-check` → passed；`ent-generate-check` → EXIT=0；`test-meta-pg` → `ok internal/meta 12.397s`；
  `helm-lint` → 0 failed；`helm-template-test` → PASS；最终二进制 B2 复现仍为退出码 1、`<data_dir>` 空。
- 2026-11-19 06:40 C1 修 `scripts/e2e-multi-replica.sh`（共享 `$WORK/secret.key`），`bash -n` → ok。
- 2026-11-19 06:42 去掉审计 `changed_fields` 里的指纹值、首次记录不再打印轮换警告、文档 Job 片段补镜像与
  `items` 说明；`CGO_ENABLED=0 go test ./cmd/lazy-observe/ -run ResetKeyFingerprint -v` → 4 个用例全绿。
- 2026-11-19 06:50 门禁：`mise run vet` → 1.48s；`mise run test` → 3.65s；`mise run fmt-check` → EXIT=0；
  `lint` → `0 issues.`；`helm-template-test` → PASS。
- 2026-11-19 06:53 `mise run e2e-ha` → EXIT=0，`Finished in 125.36s`，14 条 PASS，含
  `PASS: both application replicas started concurrently against a blank PostgreSQL database`
  （正是 CI 失败的那条路径），无 FAIL 行。
