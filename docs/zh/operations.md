# 运维手册：备份、恢复、升级、回滚与应急操作

本手册覆盖 lazy-observe 的单机嵌入式（All-in-One）与多副本高可用（HA）两种运行形态，说明备份、恢复、只读预检、升级回滚以及密钥和密码的应急处置。

> **v0.x 公开预览期兼容性约束（[ADR 0003](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0003-pre-release-single-database-baseline.md)）**
>
> 首个受支持版本发布前，SQLite 与 PostgreSQL 各自只维护一份可变的 `001_baseline.sql` 元数据基线，不提供跨不兼容 Schema 变更的原地升级或降级迁移链。
>
> - 同一元数据基线版本内：正常与异常重启均保持已提交数据持久化，可以原地替换二进制或滚动更新镜像。
> - 跨元数据基线版本：旧库启动时会因基线校验和不匹配直接失败退出（`metadata migration 1 checksum mismatch`），需要重建元数据数据库。升级前请先确认目标版本发布说明与 [version-matrix.md](version-matrix.md)。

## 1. 持久化状态与备份资产清单

备份前须分清哪些状态属于权威持久化数据，哪些可由启动流程自动重建。

| 资产 | All-in-One 模式 | HA 模式（`topology: ha`） | 必须备份 | 说明 |
| --- | --- | --- | --- | --- |
| 元数据数据库 | `<data_dir>/meta/lazy-observe.db`（SQLite WAL） | 外部 PostgreSQL（`meta.db_url`） | 是 | 存用户、项目、Token、告警规则/状态/租约/Outbox、Issue、错误 Inbox、Replay/Source Map/Profile 索引、`platform_key_fingerprints`、`schema_version` |
| 对象 / Blob 存储 | `<data_dir>/blobs/` | S3 或兼容对象存储（`blob.backend: s3`） | 是 | 存会话回放分段、Source Map 制品、gzip pprof 剖析文件 |
| 信号存储 | `<data_dir>/storage/{metrics,logs,traces}`（及 `storage/scrape`） | 外部 VictoriaMetrics / VictoriaLogs / VictoriaTraces 集群 | 视保留需求 | 指标、日志（含原始错误事件）、链路数据 |
| 平台加密密钥 | `<data_dir>/secret.key`（或 `security.secret_key_file`） | 共享只读密钥文件（`security.secret_key_file`） | 极高 | 256-bit AES-256-GCM 密钥（32 字节标准 base64），加密 AI Provider API Key 并派生自监控 Token；丢失后旧密文不可恢复 |
| 配置文件与初始密码文件 | `config.yaml` / `security.admin_password_file` | ConfigMap / Secret / `config.yaml` / `security.admin_password_file` | 是 | 不含 `<data_dir>/initial-admin-password`（随机生成的首次密码在管理员首次改密后由网关自动删除） |
| 共享会话与限流 | 进程内内存 / SQLite | Redis / Valkey（`redis.enabled: true`） | 否（易失） | 仅存登录会话、限流与登录锁定计数；丢失后用户重新登录即可 |
| 解压的嵌入存储二进制 | `<data_dir>/bin/` | `<data_dir>/bin/`（仅启用 `vmagent` 时解压） | 否 | 启动时从 `lazy-observe` 二进制校验并原子重建 |

## 2. 备份流程

### 2.1 All-in-One 模式

#### 方案 A：冷备份（推荐，全栈强一致）

冷备份可确保 SQLite、本地 Blob、嵌入式 VictoriaMetrics/Logs/Traces 与平台密钥处于同一一致时间点。

1. 优雅停止服务，等待网关摘流（`process.drain_timeout` 默认 `20s`）及托管子进程顺序退出（`process.shutdown_timeout` 默认 `15s`；内置 Supervisor 先停 `vmagent`，再优雅关闭嵌入式存储引擎，嵌入式存储的刷盘间隔为 `1s`）：

   ```sh
   # systemd 部署
   sudo systemctl stop lazy-observe

   # Docker Compose 部署
   docker compose -f deploy/compose/docker-compose.yml stop lazy-observe
   ```

2. 打包备份 `<data_dir>` 与配置文件（排除自动重建的 `bin/` 目录）：

   ```sh
   BACKUP_TS=$(date -u +%Y%m%dT%H%M%SZ)
   BACKUP_DIR="/var/backups/lazy-observe/${BACKUP_TS}"
   install -d -m 0700 "${BACKUP_DIR}"

   # 记录创建该备份的二进制版本（预览期回滚必须与二进制成对保留）
   lazy-observe --version > "${BACKUP_DIR}/binary-version.txt"

   # 打包数据目录（含 meta/、blobs/、storage/、secret.key）
   tar --exclude='./bin' -czf "${BACKUP_DIR}/data.tar.gz" -C /var/lib/lazy-observe .

   # 若配置文件或外部 secret_key_file 在 data_dir 之外，一并备份并限制权限
   tar -czf "${BACKUP_DIR}/etc.tar.gz" -C /etc/lazy-observe .
   chmod 0600 "${BACKUP_DIR}"/*.tar.gz
   ```

3. 重新启动服务并校验探针：

   ```sh
   sudo systemctl start lazy-observe
   curl -fsS http://127.0.0.1:3100/readyz
   ```

> 禁止在服务运行时直接 `cp` 或 `tar` 活的 SQLite 文件或活的嵌入式 Victoria 数据目录。`lazy-observe.db` 启用了 WAL 模式，直接拷贝可能遗漏 `-wal`/`-shm` 文件，导致数据库损坏。

#### 方案 B：热备份（不停机备份元数据、Blob 与密钥）

业务不允许停机时，可分层在线备份：

1. 使用 SQLite Online Backup API 热备元数据，安全处理 WAL：

   ```sh
   BACKUP_TS=$(date -u +%Y%m%dT%H%M%SZ)
   BACKUP_DIR="/var/backups/lazy-observe/${BACKUP_TS}"
   install -d -m 0700 "${BACKUP_DIR}"

   sqlite3 /var/lib/lazy-observe/meta/lazy-observe.db \
     ".backup '${BACKUP_DIR}/lazy-observe.db'"
   sqlite3 "${BACKUP_DIR}/lazy-observe.db" "PRAGMA integrity_check;"
   ```

2. 同步本地 Blob（Replay、Source Map 与 Profile 文件写入后即不可变）：

   ```sh
   rsync -a /var/lib/lazy-observe/blobs/ "${BACKUP_DIR}/blobs/"
   ```

3. 归档平台加密密钥与指纹：

   - 将 `secret.key`（默认 `<data_dir>/secret.key`，或 `security.secret_key_file` 指向的路径）加密存到独立密钥库或离线介质，权限保持 `0600`。
   - 从启动日志记录 128-bit 平台密钥指纹（例如 `platform encryption key matches the recorded fingerprint ... fingerprint=<hex>`）。指纹是单向截断摘要，不含密钥明文，可安全写入备份清单和工单。

4. 嵌入式信号存储热备份：

   - 嵌入式 VictoriaMetrics（`127.0.0.1:18428`）、VictoriaLogs（`127.0.0.1:19428`）、VictoriaTraces（`127.0.0.1:20428`）与 `vmagent`（`127.0.0.1:22428`）仅监听本机回环地址，不可对外暴露。
   - 如需不停机备份指标，可在本机调用 VictoriaMetrics 快照接口（`POST http://127.0.0.1:18428/snapshot/create`）配合 `vmbackup`，或使用底层文件系统快照（LVM/ZFS）；否则建议在维护窗口执行 2.1 节方案 A 的全栈冷备份。

### 2.2 HA 模式（PostgreSQL + S3 + 外部 Victoria 集群 + Redis）

HA 模式下每个应用副本的 `<data_dir>` 都是临时工作目录（用于解压可选的 `vmagent` 与生成 `scrape.yml`），不要把副本 `<data_dir>` 当作备份来源。所有持久化状态按外部组件分别热备：

1. PostgreSQL 元数据备份：

   ```sh
   BACKUP_TS=$(date -u +%Y%m%dT%H%M%SZ)
   pg_dump \
     --format=custom \
     --no-owner \
     --no-privileges \
     --file="/var/backups/lazy-observe/meta-${BACKUP_TS}.dump" \
     "${LO_META_DB_URL}"
   ```

   生产环境建议结合 PostgreSQL WAL 归档与定时基础备份（如 `pg_basebackup` 或 pgBackRest），以支持时间点恢复（PITR）。

2. S3 或兼容对象存储备份：

   - 开启目标 Bucket（`blob.s3.bucket`）的对象版本控制与生命周期策略；
   - 或定期增量同步至异地灾备 Bucket：

   ```sh
   aws s3 sync "s3://${LO_BLOB_S3_BUCKET}" "s3://${DR_S3_BUCKET}/lazy-observe/${BACKUP_TS}" \
     --endpoint-url "${LO_BLOB_S3_ENDPOINT}"
   ```

3. 外部 Victoria 集群备份：

   - VictoriaMetrics：用 `vmbackup` 对 `vmstorage`（集群）或单节点创建增量快照；
   - VictoriaLogs 与 VictoriaTraces：按 Victoria 官方备份工具与快照文档独立备份。

4. 平台密钥与部署配置备份：

   - 加密备份 `security.secret_key_file`（Kubernetes 中为 `encryptionKey.existingSecret` 或 `<fullname>-encryption-key` Secret）；
   - 备份 `config.yaml`、Helm values 以及当前容器镜像的不可变 digest（例如 `ghcr.io/eimsound/lazy-observe:0.1.0@sha256:...`）。

5. Redis / Valkey：

   - 仅存登录会话、限流窗口、登录失败锁定和跨副本认证缓存失效通知，属于可重建易失状态，无需纳入强一致备份。

## 3. 恢复与自检校验

### 3.1 All-in-One 恢复

1. 停止当前运行实例，避免双写同一个 `<data_dir>`。
2. 移走或清空损坏的 `<data_dir>`，解压备份并修正属主与权限：

   ```sh
   sudo systemctl stop lazy-observe
   sudo mv /var/lib/lazy-observe "/var/lib/lazy-observe.broken.$(date -u +%Y%m%dT%H%M%SZ)"
   sudo install -d -m 0750 -o lazy-observe -g lazy-observe /var/lib/lazy-observe
   sudo tar -xzf /var/backups/lazy-observe/<TS>/data.tar.gz -C /var/lib/lazy-observe
   sudo chown -R lazy-observe:lazy-observe /var/lib/lazy-observe
   sudo chmod 0600 /var/lib/lazy-observe/secret.key
   ```

   > 容器部署（`deploy/compose`）中进程以 `65532:65532`（distroless `nonroot`）运行，宿主机目录或卷属主须为 `65532:65532`。

3. 确认当前二进制与备份时的元数据基线匹配后启动服务。
4. 按 3.3 节执行启动后功能校验。

### 3.2 HA 恢复与 `lazy-observe doctor --ha` 只读预检

1. 停止全部应用副本（Kubernetes 执行 `kubectl scale deployment/<name> --replicas=0`），防止旧副本在恢复期间写元数据或续约告警租约。
2. 将 PostgreSQL 恢复到新建的空数据库：

   ```sh
   createdb -h <pg-host> -U <pg-user> lazy_observe_restored
   pg_restore \
     --no-owner \
     --no-privileges \
     --dbname="postgres://<pg-user>:<pass>@<pg-host>:5432/lazy_observe_restored?sslmode=require" \
     /var/backups/lazy-observe/meta-<TS>.dump
   ```

3. 恢复 S3 对象与外部信号存储，并挂载原来的共享 `security.secret_key_file`。
4. 启动副本前，先运行只读预检：

   ```sh
   lazy-observe doctor --ha --config /etc/lazy-observe/config.yaml
   ```

   `lazy-observe doctor --ha`（源码见 `cmd/lazy-observe/doctor.go`）全程只读，不修改数据库、不生成密钥文件、不写入测试对象，按顺序检查以下项目：

   | 检查项 | 校验内容 |
   | --- | --- |
   | `standalone` | 已启用 `standalone: true` 或 `topology: ha` |
   | `redis` | `redis.enabled: true` 且 `PING` 成功 |
   | `metadata-postgres` | 只读连接 PostgreSQL，确认表结构与当前二进制元数据基线校验和一致 |
   | `secret-key` | 读取 `security.secret_key_file`，校验格式合法且指纹与库中 `platform_key_fingerprints` 一致 |
   | `admin-password` | 库中尚无 `admin` 账号时要求 `security.admin_password_file` 已配置且合法（不少于 8 个字符）；已有 `admin` 时允许省略 |
   | `blob-s3` | `blob.backend: s3`、`blob.s3.bucket` 非空，且对 `doctor/` 前缀执行只读 `List` 成功 |
   | `storage-metrics` / `storage-metrics-ingest` | 探测指标查询端与写入端健康检查 |
   | `storage-logs`（及分拆的 `storage-logs-ingest`） | 探测日志查询端与写入端健康检查 |
   | `storage-traces`（及分拆的 `storage-traces-ingest`） | 探测链路查询端与写入端健康检查 |

   全部输出 `PASS` 且末行打印 `HA ready: replicas can use shared PostgreSQL, Redis, S3, and signal storage` 后，再启动应用副本。

### 3.3 启动后功能校验

```sh
# 1. 存活、就绪与组件聚合健康检查（均应返回 HTTP 200）
curl -fsS http://127.0.0.1:3100/livez
curl -fsS http://127.0.0.1:3100/readyz
curl -fsS http://127.0.0.1:3100/healthz

# 2. 使用用户 API Token (lou_...) 校验系统信息与三类信号读链路
curl -fsS http://127.0.0.1:3100/api/system/info \
  -H 'Authorization: Bearer lou_<your-token>' \
  -H 'X-LO-Project: default'

curl -fsS -G http://127.0.0.1:3100/api/v1/query \
  -H 'Authorization: Bearer lou_<your-token>' \
  -H 'X-LO-Project: default' \
  --data-urlencode 'query=up'

curl -fsS -G http://127.0.0.1:3100/api/v1/logs/query \
  -H 'Authorization: Bearer lou_<your-token>' \
  -H 'X-LO-Project: default' \
  --data-urlencode 'query=*' \
  --data-urlencode 'limit=1'

curl -fsS http://127.0.0.1:3100/api/traces/services \
  -H 'Authorization: Bearer lou_<your-token>' \
  -H 'X-LO-Project: default'
```

使用管理员账号时，可额外查看 `GET /api/system/error-inbox` 确认错误 Inbox 无异常积压，并在 Web UI 中抽查历史 Issue、Replay 会话和 Source Map 列表。

## 4. 版本升级与回滚

### 4.1 升级前检查清单

1. 阅读目标版本发布说明与 [version-matrix.md](version-matrix.md)：
   - 确认 `001_baseline.sql` 是否有变更（`v0.x` 预览期基线变更需重建元数据数据库，不能原地升级）；
   - 确认协议面与四端 SDK 兼容矩阵是否要求同步升级客户端 SDK。
2. 完成第 2 节的对应备份，并记录当前二进制版本或镜像 `tag@sha256:<digest>`。
3. 容器或 Helm 部署优先使用 GitHub Release 附带的 `helm-values-lazy-observe-<version>.yaml` 固定镜像不可变 digest。

### 4.2 All-in-One 升级步骤

1. 发送 `SIGTERM` 优雅停止服务。
2. 执行 2.1 节方案 A 冷备份。
3. 替换单一 `lazy-observe` 二进制（或更新 Docker Compose 镜像的 tag 与 digest）。发布包已内嵌 Web UI、校验清单和四套匹配的 Victoria 二进制；切勿在 `storage_bin_dir` 中混用不同版本，若处于开发或离线覆写模式，须整组同步替换四套二进制。
4. 若新版本变更了元数据基线，保留旧 `<data_dir>` 备份并初始化新库；若基线未变，直接启动即可，启动会自动校验并原子刷新 `<data_dir>/bin`。
5. 执行 3.3 节的探针与查询验收，确认正常后再清理旧备份。

### 4.3 HA 升级步骤

1. 完成 2.2 节备份（PostgreSQL dump、确认 S3 备份与 `secret.key` 备份完好）。
2. 先用新版本二进制或镜像跑一次只读预检：

   ```sh
   lazy-observe doctor --ha --config /etc/lazy-observe/config.yaml
   ```

   如果新版本与现有 PostgreSQL 基线不兼容，`metadata-postgres` 会在预检阶段直接报 `FAIL`，不中断正在运行的线上副本。

3. 预检全部 `PASS` 后滚动更新副本：
   - Kubernetes / Helm：默认启用 `RollingUpdate` 与 `PodDisruptionBudget`，配合 `pre-stop` 钩子先发 `SIGUSR1` 让 `/readyz` 返回 `503`，等端点摘流窗口结束后再发 `SIGTERM` 优雅关闭；
   - 非 Kubernetes 多副本主机：逐台滚动升级。先发 `SIGUSR1` 等待前置负载均衡摘流，再发 `SIGTERM` 停机，替换二进制或镜像后启动并等待 `/readyz` 返回 `200`，随后再升级下一台。
4. 全部副本就绪后，在日志与 `/healthz` 中确认告警规则评估、通知投递与 `vmagent` 选主切换正常。

### 4.4 回滚步骤

#### 同元数据基线回滚

1. 停止当前版本（或滚动切回旧版镜像 digest 或二进制）。
2. 若新版本未写入不兼容数据，直接启动旧版二进制即可；若怀疑存在脏数据，先按第 3 节恢复升级前备份。
3. 重新执行 `/readyz`、`/healthz` 及信号查询校验。

#### 跨元数据基线回滚

- 严禁把新基线生成的元数据数据库交给旧版二进制启动。
- 必须将升级前的数据库或数据目录备份，与升级前的精确二进制或镜像 digest 成对恢复；HA 模式下须先跑 `lazy-observe doctor --ha` 确认基线和密钥指纹一致后再恢复流量。

## 5. 应急运维流程

### 5.1 平台加密密钥失配与主动轮换（`admin reset-key-fingerprint`）

lazy-observe 启动时会计算 `security.secret_key_file`（默认 `<data_dir>/secret.key`）的 128-bit 指纹，并与元数据表 `platform_key_fingerprints` 比对。指纹不一致时，服务会在初始化网关与存储前直接拒绝启动，并在错误日志中打印文件路径、库中指纹和当前文件指纹。

#### 场景 A：误重建 Secret 或某副本挂载了错误的密钥文件（无损恢复）

**不要运行 `reset-key-fingerprint`。**

1. 停止报错副本。
2. 从备份或密钥库恢复原始 `secret.key` 文件，保证字节内容与其余健康副本完全一致，权限设为 `0600`（容器只读挂载为 `0444`）。
3. 重启副本，确认日志输出 `platform encryption key matches the recorded fingerprint`。既有密文（包括 AI Provider API Key）均保持可读。

#### 场景 B：旧密钥已永久丢失或确认主动轮换

> `admin reset-key-fingerprint` 不会对历史密文重加密。确认轮换即表示接受放弃旧密钥加密的数据。

1. 停止全部 lazy-observe 应用副本（Kubernetes 将 Deployment 缩容至 `0`；systemd 与 Compose 全部停止）。必须先停完所有副本，因为运行中的旧副本会在内存中继续用旧密钥加密写入，导致新副本无法解密。
2. 生成或挂载新的 256-bit 随机密钥文件（32 字节标准 base64，禁止使用人工口令）：

   ```sh
   openssl rand -base64 32 > /etc/lazy-observe/secret.key
   chmod 0600 /etc/lazy-observe/secret.key
   chown lazy-observe:lazy-observe /etc/lazy-observe/secret.key
   ```

3. 使用与服务端相同的配置和新密钥文件执行指纹重置。该命令只读取密钥文件，文件不存在时会报错退出，绝不自动创建或改写密钥：

   ```sh
   lazy-observe admin reset-key-fingerprint --config /etc/lazy-observe/config.yaml
   ```

   成功后会输出新旧指纹，并向审计日志写入 `security.key_fingerprint_reset`（`actor=cli`）。对同一密钥重复执行会打印 `already recorded` 且不做修改。

4. 重新启动所有应用副本（HA 模式建议先跑一次 `lazy-observe doctor --ha`）。
5. 完成轮换后收尾操作：
   - 登录 Web UI 进入 **Settings > AI** 重新输入并保存 AI Provider API Key，用新密钥重新加密入库；
   - 新密钥在下次启动时会派生新的自监控项目 Token，但旧派生 Token 不会自动吊销，仍可向自监控项目上报遥测。若旧密钥已泄露，请到自监控项目的令牌列表手动吊销旧 Token。

### 5.2 密码应急重置与登录锁定解除（`admin reset-password`）

管理员忘记密码、账号被锁或需离线重置任意用户密码时，直接用 CLI 操作元数据数据库（不经过 HTTP API）：

```sh
# 1. All-in-One 且未配置 security.admin_password_file 时重置 admin（自动生成随机密码）
lazy-observe admin reset-password --config /etc/lazy-observe/config.yaml

# 2. 指定新密码文件（重置非 admin 账号、或配置了 security.admin_password_file / HA 部署时必填）
install -m 0600 /dev/null /run/lazy-observe-new-password
printf '%s\n' '<at-least-8-chars-strong-password>' > /run/lazy-observe-new-password
lazy-observe admin reset-password \
  --config /etc/lazy-observe/config.yaml \
  --username admin \
  --password-file /run/lazy-observe-new-password
rm -f /run/lazy-observe-new-password
```

关键行为（源码见 `cmd/lazy-observe/admin.go`）：

- **随机生成模式（省略 `--password-file`）：** 仅允许目标为 `admin` 且未配置 `security.admin_password_file`。新密码以 `0600` 权限写入 `<data_dir>/initial-admin-password`，终端只打印路径而不打印明文，并强制下次登录修改密码；改密后网关自动删除该文件。
- **密码文件模式（传入 `--password-file`）：** 忽略首尾空白，长度不少于 8 个字符；不强制下次登录改密，且不会删除或覆盖部署方的 `security.admin_password_file`。
- **会话吊销与登录锁定清理：**
  - 命令会吊销目标账号的全部现有会话，并写入 `auth.password_reset`（`actor=cli`）审计事件；
  - 启用 Redis 时（HA 部署），命令须读到相同的 `redis.*` 配置，以便同步吊销 Redis 会话并清除 Redis 登录失败锁定计数（连续 5 次失败默认锁定 15 分钟）；
  - 未启用 Redis 的单进程部署中，登录锁定计数保存在网关进程内存里，CLI 无法清除；若账号正处于锁定窗口，请重启网关。

### 5.3 优雅关停与信号控制

`lazy-observe server`（源码见 `cmd/lazy-observe/server.go`、`cmd/lazy-observe/server_gateway.go`）支持两阶段优雅退出：

| 信号 | 行为 |
| --- | --- |
| `SIGUSR1` | 立即将 `/readyz` 置为 `503`（`not_ready`）并中断长连接流（如 Live Tail），但监听器继续处理在途和尚未摘流的请求。适用于前置负载均衡摘流窗口。 |
| `SIGTERM` / `SIGINT` | 触发完整关停流程：若此前未收到 `SIGUSR1`，短暂保持监听 `250ms` 以便探针观测 `503`；随后刷出自监控 SDK；在 `process.drain_timeout`（默认 `20s`）内完成在途请求并关闭错误 Inbox；最后在 `process.shutdown_timeout`（默认 `15s`）内由 Supervisor 先停 `vmagent`，再优雅关闭三个嵌入式 Victoria 存储进程。 |

容器编排或服务管理器的关停宽限期（Docker Compose `stop_grace_period`、Kubernetes `terminationGracePeriodSeconds`、systemd `TimeoutStopSec`）应至少设为 `45s`，以完整覆盖 `pre-stop` 摘流窗口、`drain_timeout` 与 `shutdown_timeout`。

## 相关文档

- [部署指南（配置字段、Standalone 与 Helm）](deployment.md)
- [Grafana 与 Perses 对接指南](grafana-perses.md)
- [Docker Compose 部署](https://github.com/eimsound/lazy-observe/blob/main/deploy/compose/README.md)
- [systemd 服务部署](https://github.com/eimsound/lazy-observe/blob/main/deploy/systemd/README.md)
- [服务端与 SDK 版本兼容矩阵](version-matrix.md)
