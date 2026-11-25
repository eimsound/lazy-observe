# 部署指南

lazy-observe 提供单机嵌入式（All-in-One）发布包、Linux 容器镜像、[Docker Compose](https://github.com/eimsound/lazy-observe/blob/main/deploy/compose/README.md) 与 [systemd](https://github.com/eimsound/lazy-observe/blob/main/deploy/systemd/README.md) 参考配置，以及支持单副本嵌入式和多副本高可用集群的 Helm Chart。备份、恢复、升级、回滚和密钥轮换见[运维手册](operations.md)；接入外部看板见 [Grafana 与 Perses 对接指南](grafana-perses.md)。

## 安装发布版

从 GitHub Releases 安装最新版本（设置 `PREFIX` 可覆盖默认的 `/usr/local`）：

```sh
curl -fsSL https://raw.githubusercontent.com/eimsound/lazy-observe/main/scripts/install.sh | sh
lazy-observe server
```

执行安装脚本时传入 `VERSION=v1.2.3` 可安装指定版本。当前不计划通过系统包管理器分发。

发布包中只有一个可执行文件，内嵌 Web UI 与四套受管存储可执行文件。首次启动时，网关会校验这些内嵌文件并将其解压到配置的数据目录下。

## 容器运行

```sh
docker run --rm -p 3100:3100 -v lazy-observe-data:/data \
  ghcr.io/eimsound/lazy-observe:latest
```

默认只需对外暴露 HTTP 网关端口 `3100`。可选的原生 OTLP/gRPC 端口 `4317` 默认关闭（`otlp_grpc.enabled: false`）。请持久化挂载 `/data`。官方镜像基于 distroless `nonroot`，以非 root 用户 `65532:65532` 运行，且不含 Shell。非 Kubernetes 环境下需要安全加固时，请参考 [Docker Compose 参考配置](https://github.com/eimsound/lazy-observe/blob/main/deploy/compose/README.md)（含单机嵌入式 All-in-One 与外部存储两种配置）或 [systemd 服务单元](https://github.com/eimsound/lazy-observe/blob/main/deploy/systemd/README.md)。

## 单机嵌入式（All-in-One）

受支持的构建目标为 Linux `amd64` 与 `arm64`，且必须保持 `CGO_ENABLED=0`。

```sh
mise run dist
./bin/lazy-observe server --data-dir ./data
```

`mise run dist` 会按生产形态在本地构建二进制，内嵌当前平台已校验的存储清单与可执行文件。保持 `storage_bin_dir` 为空（默认值），网关会在启动时把这些内嵌文件解压到 `data_dir/bin`。

HTTP 网关监听 `:3100`，可选的 OTLP/gRPC 监听端口 `:4317` 默认关闭（`otlp_grpc.enabled: false`）。受管存储子进程只监听固定本机回环地址，切勿对外暴露：

- VictoriaMetrics：`127.0.0.1:18428`
- VictoriaLogs：`127.0.0.1:19428`
- VictoriaTraces：`127.0.0.1:20428`
- `vmagent`：`127.0.0.1:22428`

首次启动创建 `admin` 账号时，服务端会生成随机密码并写入 `<data_dir>/initial-admin-password`（权限 `0600`），日志只输出该文件的路径。管理员首次登录必须修改该密码，未改密前其他接口均不可访问。同一次启动会打印一次 `default` 项目的上报 API Token。随机密码不会写入日志，初始化完成后这两项凭据都不会再次打印。

每个受管子进程拥有独立的重启循环。杀掉其中一个存储子进程不会改变网关 PID，也不会停止其他信号存储；健康与就绪接口会把该组件标记为重启中，直到其 `/health` 探针恢复成功。重启退避在当前指数退避时长的 50% 到 100% 之间做等幅抖动，上限由 `process.restart_max` 控制。正常关停会先停用这些重启循环，再排空 `vmagent`，最后优雅关闭存储写入进程。

受管的 VictoriaMetrics、VictoriaLogs 和 VictoriaTraces 每 1 秒把内存数据刷盘一次，Trace ID 索引也使用同样的 1 秒间隔。同目录重启门禁会等待这一落盘窗口结束后再终止进程组。

若在本地开发前端且不想重新打包内嵌 UI：

```sh
cd web
mise exec -- pnpm install --frozen-lockfile
mise exec -- pnpm dev
```

Vite 监听 <http://localhost:5173>，并将 `/api` 代理至网关。

## 配置参考

复制[示例配置文件](https://github.com/eimsound/lazy-observe/blob/main/config.example.yaml)并通过 `--config` 启动：

```sh
cp config.example.yaml config.yaml
./bin/lazy-observe server --config config.yaml
```

配置优先级从高到低依次为：命令行参数、`LO_*` 环境变量、YAML 配置文件、内置默认值。服务端会拒绝未知的 YAML 字段。时长字段支持 Go 时长语法（如 `30s`、`4h`）以及整天写法（如 `14d`）。

示例配置文件列出了规范的路径式环境变量名，例如 `LO_STORAGE_METRICS_QUERY_URL` 与 `LO_RETENTION_REPLAY`。服务端不接受简短兼容别名。

### 服务与存储配置

| YAML 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | `:3100` | 对外 HTTP 网关监听地址 |
| `otlp_grpc.enabled` | `false` | 开启原生 OTLP/gRPC 上报（链路、指标、日志） |
| `otlp_grpc.listen` | `:4317` | 原生 OTLP/gRPC 监听地址（开启后生效） |
| `otlp_grpc.max_recv_bytes` | `54525952` | OTLP/gRPC 单条解码后消息大小上限（字节，默认 52 MiB） |
| `ingest_rate_limit.enabled` | `false` | 为 HTTP 与 OTLP/gRPC 上报开启按项目隔离的令牌桶限流 |
| `ingest_rate_limit.requests_per_second` | `500` | 单项目每秒上报请求数上限 |
| `ingest_rate_limit.burst` | `1000` | 单项目上报突发请求配额 |
| `ingest_rate_limit.bytes_per_second` | `0` | 可选单项目每秒字节吞吐上限（`0` 表示不限制字节数） |
| `ingest_rate_limit.max_tracked_projects` | `4096` | 内存中同时跟踪的项目限流桶上限 |
| `metrics_endpoint.enabled` | `false` | 暴露网关自身运行指标的 Prometheus 文本端点（`/metrics`） |
| `metrics_endpoint.bearer_token` | 空 | 可选静态 `Authorization: Bearer <token>` 抓取凭据 |
| `metrics_endpoint.allow_unauthenticated` | `false` | 允许在内网免鉴权抓取 `/metrics`；配置 `bearer_token` 时禁止开启 |
| `data_dir` | `./data` | 元数据、本地 Blob、生成文件与内嵌存储二进制根目录 |
| `serve_web_ui` | `true` | 由网关直接提供内嵌 Web UI |
| `external_url` | 空 | 控制台对外访问根地址（`http(s)://host[:port]`，不含路径、查询串或锚点）。配置后告警通知将附带可点击的控制台绝对链接；留空时不附带链接 |
| `topology` | 空 | 显式声明拓扑：`""`、`all-in-one`、`standalone` 或 `ha`。设为 `ha` 时若缺少 PostgreSQL、Redis、S3、共享密钥文件或外部信号端点，启动直接失败 |
| `standalone` | `false` | 使用外部信号存储 |
| `storage_bin_dir` | 空 | 可选开发或离线覆写目录；留空则使用校验后的内嵌 Victoria 二进制 |
| `meta.db_url` | 空 | 留空时使用 `data_dir/meta/lazy-observe.db`；否则填写 `sqlite://...`、`postgres://...` 或 `postgresql://...` |
| `meta.migration_lock_timeout` | `60s` | PostgreSQL 迁移咨询锁等待上限 |
| `storage.metrics.kind` | `vm` | `vm`（官方支持）或 `prometheus`（通用 Prometheus 兼容 API，非官方支持），决定查询租户隔离方式 |
| `storage.metrics.cluster` | `false` | 使用 VictoriaMetrics 集群的 `vmselect`/`vminsert` 路径；需处于 standalone 模式且 `kind: vm` |
| `storage.metrics.tenant_account_id` | 空 | 平台级无符号 32 位 VictoriaMetrics 账号 ID；开启集群模式时必填 |
| `storage.metrics.query_url` | 空 | standalone 模式必填；Prometheus 兼容查询根地址或 `vmselect` 根地址 |
| `storage.metrics.ingest_url` | 空 | standalone 可选写入地址或 `vminsert` 根地址；`/api/v1/write` 走 remote-write 1.0，OTLP 与 Web Vitals 依赖 VictoriaMetrics 端点；配置后启用 `vmagent` |
| `storage.logs.url` | 空 | standalone 模式下 VictoriaLogs 根地址（未同时配置分拆读写地址时必填） |
| `storage.logs.query_url` | 空 | 可选 `vlselect` 查询地址覆写；留空回退至 `storage.logs.url` |
| `storage.logs.ingest_url` | 空 | 可选 `vlinsert` 写入地址覆写；留空回退至 `storage.logs.url` |
| `storage.traces.url` | 空 | standalone 模式下 VictoriaTraces 根地址（未同时配置分拆读写地址时必填） |
| `storage.traces.query_url` | 空 | 可选 `vtselect` 查询地址覆写；留空回退至 `storage.traces.url` |
| `storage.traces.ingest_url` | 空 | 可选 `vtinsert` 写入地址覆写；留空回退至 `storage.traces.url` |
| `redis.enabled` | `false` | 启用基于 Redis 的共享会话与限流 |
| `redis.addr` | `127.0.0.1:6379` | 启用 Redis 时的连接地址 |
| `redis.password` | 空 | 可选 Redis 密码；生产环境应通过 Secret 注入 |
| `redis.db` | `0` | Redis 逻辑库编号 |
| `redis.key_prefix` | `lazy-observe` | 全部网关副本共享的键前缀 |
| `self_monitoring.enabled` | `true` | 通过内置 Go SDK 与自监控项目观测 lazy-observe 自身 |
| `self_monitoring.profiling` | `false` | 为常驻网关进程开启持续性能剖析 |

### 保留策略、对象存储、告警与进程配置

| YAML 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `retention.metrics` | `90d` | 内嵌 VictoriaMetrics 保留时长 |
| `retention.logs` | `14d` | 内嵌 VictoriaLogs 保留时长 |
| `retention.traces` | `7d` | 内嵌 VictoriaTraces 保留时长 |
| `retention.replay` | `3d` | 会话回放元数据与 Blob 清理期限 |
| `retention.sourcemaps` | `90d` | Source Map 清理期限 |
| `retention.alert_events` | `30d` | 告警事件清理期限 |
| `retention.profiles` | `7d` | 性能剖析元数据与 Blob 清理期限 |
| `retention.issues_closed` | `true` | 已解决或已忽略的 Issue 静默满 90 天后删除 |
| `replay.cleanup_interval` | `1h` | 历史回放清理间隔配置项；平台保留清理器当前使用自身默认的 1 小时间隔 |
| `blob.backend` | `local` | 会话回放、Source Map 与剖析数据存储后端：`local` 或 `s3` |
| `blob.s3.bucket` | 空 | `blob.backend` 为 `s3` 时必填 |
| `blob.s3.region` | `us-east-1` | AWS 区域 |
| `blob.s3.endpoint` | 空 | 可选 S3 兼容服务根地址；设置后自动启用 path-style 访问 |
| `blob.s3.access_key` | 空 | 可选静态凭据，须与 `blob.s3.secret_key` 成对配置 |
| `blob.s3.secret_key` | 空 | 可选静态凭据，须与 `blob.s3.access_key` 成对配置 |
| `alert.storm_threshold` | `10` | 同一告警分组触发风暴抑制前的告警数量阈值 |
| `alert.group_wait` | `30s` | 分组首次聚合等待时长 |
| `alert.repeat_interval` | `4h` | 重复告警通知间隔 |
| `scrape.mode` | `auto` | 内嵌 `vmagent` 抓取模式：`auto`（单节点直接运行，HA 模式通过 PostgreSQL 选主）或 `disabled` |
| `error_inbox.max_items` | `100000` | 持久化错误 Inbox 条目上限，超出后返回 `503` 与 `Retry-After` |
| `error_inbox.max_bytes` | `1073741824` | 持久化错误 Inbox 字节上限（1 GiB），超出后返回 `503` 与 `Retry-After` |
| `error_inbox.readiness_items` | `80000` | 触发就绪探针降级的错误 Inbox 条目阈值 |
| `error_inbox.readiness_bytes` | `838860800` | 触发就绪探针降级的错误 Inbox 字节阈值（800 MiB） |
| `process.startup_timeout` | `20s` | 单个受管子进程启动健康检查超时 |
| `process.shutdown_timeout` | `15s` | 受管子进程关停超时 |
| `process.restart_initial` | `1s` | 受管子进程重启退避初始间隔（抖动前） |
| `process.restart_max` | `1m` | 受管子进程重启退避最大间隔 |
| `process.drain_timeout` | `20s` | 网关在途请求与 Issue 批处理排空超时 |
| `process.component_port_offset` | `0` | 本地多实例隔离测试使用的受管子进程端口偏移；正常部署保持 `0` |
| `security.session_secure_cookie` | `false` | 网关前置 TLS 终止时为会话与 CSRF Cookie 添加 `Secure` 属性 |
| `security.tls_cert` | 空 | 原生 TLS 1.2+ 证书文件路径，须与 `security.tls_key` 成对配置 |
| `security.tls_key` | 空 | 原生 TLS 1.2+ 私钥文件路径，须与 `security.tls_cert` 成对配置 |
| `security.trusted_proxies` | `[]` | 可信反向代理 IP 或 CIDR 列表，用于读取 `X-Forwarded-For` 计算客户端限流 IP |
| `security.secret_key_file` | 空 | 平台 AES-256-GCM 密钥文件路径；留空使用 `<data_dir>/secret.key` |
| `security.admin_password_file` | 空 | 运维托管的首次 `admin` 密码文件路径 |
| `ai.pricing_file` | 空 | 可选静态 LLM Token 定价覆写文件路径 |

### 首次管理员密码

当元数据数据库中不存在名为 `admin` 的用户时（例如首次连接空库启动），服务端会自动创建 `admin` 账号；若此时缺少 `default` 项目，也会一并创建。如果 `admin` 账号被删除，下次启动会重新创建并生成新的初始密码。

- `security.admin_password_file`（环境变量 `LO_SECURITY_ADMIN_PASSWORD_FILE`）指定存放初始密码的文件路径。服务端会忽略首尾空白与末尾换行，要求密码不少于 8 个字符。以这种方式提供的密码视为运维自行指定，不会强制首次登录改密。
- 未配置该字段时，服务端会生成随机密码，在创建账号前以 `0600` 权限写入 `<data_dir>/initial-admin-password`，日志只打印绝对路径而不输出密码。该密码必须在首次登录时修改：带有 `must_change_password` 标记的账号只能访问 `/api/auth/me`、修改密码或登出，其余接口统一返回 `403 password_change_required`。

多副本部署必须配置 `security.admin_password_file`（或在 Helm 中使用 `adminPassword` Secret）。否则随机密码文件只会出现在抢到初始化锁的那一个副本上，而在 `standalone-ha` 模式下，Pod 的 `data_dir` 卷使用 `emptyDir`。后续启动若发现 `admin` 已存在，不会生成密码文件，原有密码保持不变。

忘记密码时，可直接通过 CLI 连接配置文件与元数据数据库重置任意账号密码，无需知晓原密码：

```bash
lazy-observe admin reset-password --config /etc/lazy-observe/config.yaml
lazy-observe admin reset-password --username operator --password-file /run/secrets/operator-password
```

命令默认重置 `admin`，通过 `--username` 可指定其他账号。只有目标为 `admin` 且未配置 `security.admin_password_file` 时，才允许使用自动生成密码模式。该模式会把新密码以 `0600` 权限写入 `<data_dir>/initial-admin-password`，终端只打印路径，并给账号打上 `must_change_password` 标记。若目标文件已存在，命令会先输出 `replacing existing <path>` 再覆盖，不显示原文件内容。

其他所有账号，以及配置了 `security.admin_password_file` 的部署（涵盖全部 HA 部署），都必须传入 `--password-file`；命令会拒绝生成随机密码，避免改写部署方托管的文件。`--password-file` 遵循与 `security.admin_password_file` 相同的规则：忽略首尾空白、至少 8 个字符、不强制下次登录改密。命令直连元数据数据库，吊销目标账号的全部会话；若账号不存在则返回非零退出码。开启 `redis.enabled` 时需传入相同的 `redis.*` 配置，以便同步吊销 Redis 中的会话。命令还会写入一条 `actor` 为 `cli` 的 `auth.password_reset` 审计事件。

开启 `redis.enabled` 时，命令会同时清除该账号在 Redis 中的登录失败锁定计数，重置后即可登录。未启用 Redis 的单进程部署把锁定计数保存在网关内存中，CLI 无法触及；若账号正被锁定，请重启网关。

管理员通过 Web UI 或 `POST /api/auth/password` 修改完随机密码后，网关会自动删除 `<data_dir>/initial-admin-password`。只有真正删除失败时才会记录警告日志；文件已不存在时保持静默，且两种情况都不会导致改密失败。网关不会删除 `security.admin_password_file` 指向的文件，HA 部署的共享初始密码文件在改密后保持原样。

### 平台加密密钥

`security.secret_key_file`（环境变量 `LO_SECURITY_SECRET_KEY_FILE`）指定平台 256-bit AES-256-GCM 密钥的文件路径；环境变量只传路径，不传密钥明文。该密钥用于加密元数据中的敏感字段，例如在 **Settings > AI** 配置的 AI Provider API Key（密文存库，绝不返回浏览器）；服务端同时用它派生内置自监控项目的上报 Token，而不以可还原形式存库。

密钥文件内容为 32 字节随机数据的标准 base64 编码（有无等号填充均可）。服务端会忽略首尾空白与换行，可以直接使用 `openssl rand -base64 32` 的输出。密钥必须来自密码学安全随机源，不得使用人工口令或对口令做 KDF 派生：人工口令可被字典攻击还原，一旦命中，既能匹配库中指纹，也能解开全部密文。该字段留空时默认使用 `<data_dir>/secret.key`：首次启动以 `0600` 权限生成新密钥；文件已存在时只读不写，并在进程整个生命周期内常驻内存。

共享同一元数据数据库的全部副本必须读取相同密钥，服务端会在启动阶段强制校验。首次启动时，服务端对固定标签计算密钥的 128-bit HMAC-SHA256 指纹，写入元数据表 `platform_key_fingerprints`（不存密钥本身），并在日志中输出密钥路径与指纹。此后每次启动都会计算本地密钥指纹并与库中记录比对：一致则正常启动；不一致则在网关、存储和初始化执行前直接退出（非零退出码），并打印密钥路径、新旧指纹、可能原因和恢复步骤。多副本首次并发连接空库时，插入操作带条件约束，只会写入一条指纹，其余副本读取该记录并核验，不会互相覆盖。这样就把原先静默损坏 AI 配置、派生冲突自监控 Token 的隐患，变成了启动即拦截。

指纹是截断后的单向摘要，不会泄露密钥明文，可以写入工单和日志。不过拿到数据库备份的人仍可离线拿完整候选密钥去验证指纹，因此密钥本身必须保持 32 字节真随机。

Helm Chart 会将单一 Secret 以只读方式挂载到 `/etc/lazy-observe-key/secret.key`，来源为 `encryptionKey.existingSecret`，或在 `encryptionKey.autoGenerate` 为 `true` 时使用 Chart 托管的 `<fullname>-encryption-key`。`standalone-ha` 模式若两者均未提供，无论副本数为多少（包括 1 副本）都会拒绝渲染模板：它的 `/data` 是 `emptyDir`，元数据数据库在外部，Pod 内生成的密钥随 Pod 重建而丢失，新 Pod 会因指纹失配被拒且无法找回原密钥。请设置 `encryptionKey.existingSecret` 或保持 `encryptionKey.autoGenerate=true`。带 PVC 的 `allinone` 模式把密钥保存在 PVC 内的 `data_dir`；不带 PVC 的 `allinone` 把密钥与元数据数据库放在同一个 `emptyDir` 中，两者同生同灭，不会出现指纹失配。

出现指纹失配说明当前配置的密钥与初始化部署时不同，恢复有两条路径：

1. **恢复原密钥（常规情况）。** 把原来的密钥文件放回原位（GitOps 场景下还原先前的 Secret 内容）并重启。历史密文未被改写，恢复后即可正常读取。
2. **确认主动轮换。** 仅在旧密钥确实永久丢失且接受放弃既有密文时，才显式登记当前密钥。轮换属于全集群操作：服务端只在启动时校验指纹，因此必须先停止全部副本（Kubernetes 中先缩容至 0）。若仍有旧副本在线，它会继续用旧密钥加密写入，导致轮换后的副本无法解密。

   ```sh
   lazy-observe admin reset-key-fingerprint --config /etc/lazy-observe/config.yaml
   ```

   该命令要求密钥文件必须已存在，绝不会自动创建或改写文件，防止写错路径或漏挂 Secret 导致录入无效指纹。执行后会更新库中指纹、输出旧指纹与新指纹，并记录一条 `actor` 为 `cli` 的 `security.key_fingerprint_reset` 审计事件。对同一密钥重复执行会输出 `already recorded` 且不做修改。系统不会自动轮换，只有运维手动执行该命令才会变更记录。完成后再用新密钥启动各副本。

   在 Kubernetes 中，通过 Job 挂载与 Deployment 相同的配置 ConfigMap 和密钥 Secret，并使用与 Deployment 一致的镜像及 tag：

   ```yaml
   apiVersion: batch/v1
   kind: Job
   metadata:
     generateName: lazy-observe-key-rotation-
   spec:
     template:
       spec:
         restartPolicy: Never
         containers:
           - name: rotate
             image: ghcr.io/eimsound/lazy-observe:<same tag as the Deployment>
             args: ["admin", "reset-key-fingerprint", "--config", "/etc/lazy-observe/config.yaml"]
             env:
               - name: LO_META_DB_URL
                 valueFrom:
                   secretKeyRef:
                     name: <release>-lazy-observe
                     key: postgres-url
             volumeMounts:
               - name: config
                 mountPath: /etc/lazy-observe
                 readOnly: true
               - name: encryption-key
                 mountPath: /etc/lazy-observe-key
                 readOnly: true
         volumes:
           - name: config
             configMap:
               name: <release>-lazy-observe
           - name: encryption-key
             secret:
               secretName: <release>-lazy-observe-encryption-key
               # Project the same key name and path as the Deployment. With
               # encryptionKey.existingSecret, use that Secret's name instead;
               # when encryptionKey.key is not the default `secret.key`, add
               #   items: [{key: <encryptionKey.key>, path: secret.key}]
               # so security.secret_key_file in the ConfigMap still resolves.
               items:
                 - key: secret.key
                   path: secret.key
   ```

   使用 `kubectl run` 配合相同的 `--env` 与 `--overrides` 卷挂载同样可行，只要该 Pod 能访问元数据数据库并读取密钥文件。

轮换不是透明重加密，旧密文不会被重新封装，原有加密数据保持不可读。实操中需重新进入 **Settings > AI** 输入并保存 AI Provider API Key，用新密钥重新加密入库。换用新密钥后，下次启动也会派生新的自监控项目 Token；旧的派生凭据不会自动吊销，仍可作为自监控项目的上报凭据使用，若旧密钥已泄露，请到自监控项目的 Token 列表中手动吊销。

请像备份数据库一样妥善备份该密钥，密钥丢失后旧密文不可找回。`platform_key_fingerprints` 表已并入元数据基线，若元数据数据库（SQLite 文件或 PostgreSQL 库）由更早提交创建，启动会在指纹校验之前先因 `metadata migration 1 checksum mismatch` 失败。请按 [ADR 0003](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0003-pre-release-single-database-baseline.md) 重建该开发库并保留原密钥文件，首次启动即会记下当前密钥指纹。

### 可选 AI 访问层

AI 功能默认关闭，仅管理员可在 **Settings > AI** 中配置 Provider（`openai`、`anthropic`、`deepseek` 或 `ollama`）、可选 `base_url`、模型与 API Key。Provider 配置和 API Key 存于元数据数据库，API Key 由 `security.secret_key_file` 加密且绝不返回浏览器。服务端 YAML 与 `LO_AI_*` 环境变量不提供 Provider 或 API Key 配置项，仅支持通过 `ai.pricing_file`（`LO_AI_PRICING_FILE`）配置静态 LLM Token 定价覆写。

启用后，网关提供管理员连通性测试、自然语言生成与解释 MetricsQL/LogsQL，以及新 Issue 的异步分析。AI 生成的查询仍经过按项目隔离的网关执行，并受响应大小与耗时上限约束。Issue 分析采用有界的跨信号证据，不会阻塞错误上报；除非 SDK 显式开启，否则不采集 Prompt 与 Completion 原文。

### 告警通知中的控制台链接

配置 `external_url`（环境变量 `LO_EXTERNAL_URL`、参数 `--external-url` 或 Helm 值 `externalURL`）为控制台对外访问根地址（例如 `https://observe.example.com`，不含路径、查询串或锚点）后，出站告警通知将自动附带可点击的绝对链接。留空时不附带任何控制台链接。

各通知类型对应的链接目标如下：

- **Issue 告警**（`new_issue`、`regression`）：`<external_url>/issues/<id>?project=<slug>`
- **规则告警**：单条告警指向 `<external_url>/alerts?fingerprint=<fp>&project=<slug>`；聚合多条告警指向 `<external_url>/alerts?project=<slug>&rule=<rule>`
- **告警风暴**：`<external_url>/alerts?project=<slug>&status=firing`
- **错误率突增**（`error_rate_spike`）：`<external_url>/issues?project=<slug>`
- **通知渠道测试**：`<external_url>/alerts/channels`

各渠道渲染形式：

- **飞书（Feishu）**：卡片底部主按钮（`View in lazy-observe`），多条告警聚合时每条附带 `lark_md` 链接（`View`）。
- **钉钉（DingTalk）与企业微信（WeCom）**：Markdown 链接（`[View in lazy-observe](...)`，多条聚合时每条附带 `[View](...)`）。
- **Slack**：`mrkdwn` 链接（`<url|View in lazy-observe>`，多条聚合时每条附带 `<url|View>`）。
- **邮件（`smtp`）**：正文首行显示 `View in lazy-observe: <url>`，随后附 JSON 载荷。
- **Webhook**：JSON 载荷附带可选字段 `url` 与 `alerts[].url`。

打开链接时，浏览器标签页会切换到链接所指项目；项目不存在或无权访问时，控制台显示错误页，不会展示其他项目的数据。

链接在通知发送阶段动态生成（而非入队时固化），修改 `external_url` 后对已入队或重试中的通知同样生效。

## 独立部署（Standalone）

Standalone 模式仍运行网关、进程内告警引擎、元数据数据库、Blob 存储以及可选的 `vmagent`。最小配置如下：

```yaml
standalone: true
storage:
  metrics:
    kind: vm
    query_url: "http://victoria-metrics:8428"
    ingest_url: "http://victoria-metrics:8428"
  logs:
    url: "http://victoria-logs:9428"
  traces:
    url: "http://victoria-traces:10428"
```

发布包已内嵌经过校验的 `vmagent-prod`，最小 Standalone 配置保持 `storage_bin_dir` 为空即可。如果显式配置了开发或离线 `storage_bin_dir` 覆写目录，在设置 `ingest_url` 时该目录必须包含 `vmagent-prod`。未配置 `ingest_url` 时，指标 OTLP 上报与抓取管理均会关闭。外部信号存储必须为 Victoria 家族（VictoriaMetrics、VictoriaLogs、VictoriaTraces），Loki 与 Tempo 按设计不支持（[ADR 0013](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0013-signal-storage-is-victoria-only.md)）。`kind: prometheus` 仍会把查询转发到通用 Prometheus 兼容 API，但不属于官方支持范围，也不在发布验证覆盖内。

内置 Supervisor 不会启动、发信号或重启外部指标、日志与链路服务。若 lazy-observe 启动了可选的 `vmagent`，该子进程仍由 Supervisor 托管并自动重启。

由于外部存储不受 Supervisor 管理，查询延迟取决于外部实例自身的启动参数。内嵌存储启动 VictoriaMetrics 与 VictoriaTraces 时默认带 `-search.latencyOffset=0s`，新数据可立即查到。

VictoriaTraces 默认对 Jaeger 和 Tempo 搜索接口隐藏最近 `-search.latencyOffset`（默认 30 秒）内的 Span，v0.12 起 LogsQL 查询同样受此偏移影响。网关在发往 VictoriaTraces 的每一条 LogsQL 请求（用于 LLM 调用统计）上都会附带 `disable_latency_offset=true`。单节点 VictoriaTraces 与 v0.12 及以上的 `vtselect` 支持该参数，外部部署无需额外改参数即可查看实时 LLM 统计；v0.12 之前 LogsQL 本身不设偏移且会忽略未知查询参数（经上游源码核对，未用旧二进制实测）。

Jaeger API 没有单次请求开关。对接外部 VictoriaTraces 时，链路列表会漏掉 `-search.latencyOffset` 窗口内的新 Span，链路详情（含 Issue AI 分析调用）在 Trace ID 索引刷盘前也会返回未找到，刷新间隔由 `-insert.indexFlushInterval` 决定（单节点或 `vtinsert` 默认 20 秒）。若需在页面立即看到新链路，请在外部 VictoriaTraces 上调低这两个参数。内嵌 VictoriaTraces 默认使用 `-search.latencyOffset=0s` 与 `-insert.indexFlushInterval=1s`。

两种运行模式在网关内部共用同一套存储接口。Standalone 模式下各配置项承接的流量如下：

| 配置项 | 网关流量 |
| --- | --- |
| `storage.metrics.query_url` | Prometheus 兼容查询与元数据接口 |
| `storage.metrics.ingest_url` | OTLP 指标、Prometheus remote-write、Web Vitals 导入以及可选的 `vmagent` remote-write |
| `storage.logs.url`（或分拆的 `query_url` / `ingest_url`） | VictoriaLogs OTLP/JSON-line 写入与 LogsQL 读取 |
| `storage.traces.url`（或分拆的 `query_url` / `ingest_url`） | VictoriaTraces OTLP 写入与 Jaeger 兼容读取 |

`/api/v1/write` 接收 Prometheus remote-write 1.0（`prometheus.WriteRequest`），在网关侧重写 protobuf 标签以强制写入鉴权后的 `project_id`，因此不依赖 VictoriaMetrics 专有的 `extra_label` 查询参数。目标接收端须暴露标准的 `/api/v1/write` 路径。`/v1/metrics` 依赖 VictoriaMetrics 的 `/opentelemetry/v1/metrics` 端点，Web Vitals 依赖 `/api/v1/import/prometheus` 端点。未配置 `ingest_url` 时，这些写入接口返回 `501`，不会回退到 `query_url`。独立配置的写入端点也会纳入健康与就绪探针。

VictoriaLogs 与 VictoriaTraces 请求沿用官方文档定义的单节点路径，网关会在读写两侧把客户端传入的 `AccountID`/`ProjectID` 请求头替换为鉴权后的项目租户标识。

Standalone 模式下，指标、日志与链路的保留周期由外部存储自行管理；平台保留策略继续负责清理会话回放、Source Map、告警事件、性能剖析与已关闭 Issue。

## 查询 API、MCP 与 Grafana

所有读取请求一律按用户身份鉴权，不按项目身份鉴权。项目 API Token（`lo_...`）仅限遥测上报：任何读取路由收到项目 Token 都会返回 `403` 与 `code: ingest_only_token`，因为 SDK DSN 会直接暴露在公开浏览器页面中（[ADR 0010](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0010-project-api-tokens-are-ingest-only.md)）。`/mcp` 与 `/api/system/info` 同样遵循该规则。Source Map 上传令牌（`los_...`）仅限上传 Source Map。

读取请求支持以下两种用户凭据之一：

- 浏览器会话 Cookie（`lss_...`）；
- 用户 API Token（`lou_...`），通过 `Authorization: Bearer ...` 传递，并配合 `X-LO-Project: <slug>` 指定目标项目。未传该请求头时默认查询 `default` 项目。

在 **Settings > Personal API tokens** 中创建用户 API Token，明文仅展示一次。对应的 HTTP 接口为 `POST /api/auth/tokens`（需携带会话 Cookie 与 `X-CSRF-Token` 双重提交头）。用户 API Token 继承签发用户的当前角色（`admin`、`member` 或 `viewer`），管理员签发的 Token 可访问管理员专属接口。

### Prometheus 兼容查询

| 端点 | 用途 |
| --- | --- |
| `GET`/`POST /api/v1/query` | 即时 PromQL / MetricsQL 查询 |
| `GET`/`POST /api/v1/query_range` | 区间查询 |
| `GET`/`POST /api/v1/series` | 序列元数据 |
| `GET`/`POST /api/v1/labels` | 标签名列表 |
| `GET`/`POST /api/v1/label/<name>/values` | 标签值列表 |

网关会为每条查询自动注入当前鉴权项目的 `project_id` 过滤条件，并丢弃客户端传入的隔离参数，防止跨项目读取指标。写入留在上报侧：`/api/v1/write` 处理 Prometheus remote-write，继续使用项目 API Token（`lo_...`）。

```sh
curl -fsS -G http://localhost:3100/api/v1/query \
  -H 'Authorization: Bearer lou_...' \
  -H 'X-LO-Project: default' \
  --data-urlencode 'query=up'
```

LogsQL、链路、会话回放、Issue、告警与性能读取路由位于 `/api` 下（如 `/api/v1/logs/query`、`/api/traces/search`、`/api/v1/issues`），使用相同的凭据组合，不属于 Prometheus 数据源支持范围。

### Grafana

添加 Prometheus 数据源时，将 URL 指向网关根地址（如 `http://lazy-observe:3100`），不加路径前缀。Grafana 的 Prometheus 数据源没有单独的 Bearer Token 字段，请通过自定义 HTTP Header 传入两个请求头：

- `Authorization: Bearer lou_...`
- `X-LO-Project: <slug>`

使用文件预置（Provisioning）时，按 [Grafana Provisioning 文档](https://grafana.com/docs/grafana/latest/administration/provisioning/)的约定，把 Header 名称写在 `jsonData.httpHeaderNameN`，对应值写在 `secureJsonData.httpHeaderValueN`，避免 Token 以明文出现在配置文件中。每个数据源固定一个 `X-LO-Project`，因此一个数据源对应一个项目。Grafana 中会调用其他 Prometheus 端点（如 metadata、exemplars、rules）的功能在网关无对应路由，会返回 `404`；上表列出的五个端点即为完整支持范围。

如需覆盖指标、日志与链路的 Grafana / Perses 完整预置示例（含 LogsQL 与 Jaeger 路径重写代理及 [ADR 0017](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0017-no-built-in-dashboards.md) 看板边界说明），请参见 [Grafana 与 Perses 对接指南](grafana-perses.md)、[`deploy/integrations/grafana/datasources.yaml`](https://github.com/eimsound/lazy-observe/blob/main/deploy/integrations/grafana/datasources.yaml) 以及 [`deploy/integrations/perses/datasources.yaml`](https://github.com/eimsound/lazy-observe/blob/main/deploy/integrations/perses/datasources.yaml)。

### MCP

`lazy-observe mcp` 为 MCP 客户端提供项目作用域的 stdio 代理：

```sh
lazy-observe mcp --dsn 'http://localhost:3100/p/default@lou_...'
```

该 DSN 沿用 SDK 格式（`/p/<slug>@<token>`），有两处差异：`<token>` 必须为用户 API Token（`lou_...`），CLI 在启动时即做检查，无需等到首次调用工具才报错；`<slug>` 会作为 `X-LO-Project` 请求头发出。启动校验只检查前缀，已吊销的 `lou_...` 仍能启动代理，并在后续调用工具时失败。`/mcp` 端点本身会以 `403 ingest_only_token` 拒绝 `lo_...`。暴露的 MCP 工具均为只读，包含有界的会话回放元数据，不暴露原始回放事件和 URL 查询参数。

## Kubernetes 与 Helm 部署

Helm Chart 位于 `deploy/helm/lazy-observe`。默认 `standalone-ha` 模式运行两个副本，要求外部 PostgreSQL、Redis、S3 兼容对象存储、VictoriaMetrics、VictoriaLogs 与 VictoriaTraces 可达。请将凭据存入 Kubernetes Secret，切勿写入 values 文件提交：

```sh
kubectl create namespace lazy-observe
kubectl -n lazy-observe create secret generic lazy-observe \
  --from-literal=postgres-url='postgres://lazy_observe:password@postgres:5432/lazy_observe?sslmode=require' \
  --from-literal=redis-password='redis-password' \
  --from-literal=s3-access-key='access-key' \
  --from-literal=s3-secret-key='secret-key'

helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe \
  --set secret.existingSecret=lazy-observe \
  --set external.redis.addr=redis:6379 \
  --set external.s3.bucket=lazy-observe \
  --set external.s3.endpoint=https://s3.example.com \
  --set external.metrics.queryURL=http://vmselect:8481 \
  --set external.metrics.ingestURL=http://vminsert:8480 \
  --set external.logs.url=http://victoria-logs:9428 \
  --set external.traces.url=http://victoria-traces:10428
```

生产级三副本集群拓扑可直接基于仓库内置的集群配置起步，覆盖其中的示例地址与 Secret 引用即可：

```sh
helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe \
  --set secret.existingSecret=lazy-observe \
  -f deploy/helm/lazy-observe/values-standalone-ha-cluster.yaml
```

该配置把指标查询发往 `vmselect`、写入发往 `vminsert`，共享同一个平台账号，由网关提供第二层 `project_id` 边界。其中的 VictoriaLogs 与 VictoriaTraces 地址须为能识别路径的 Service 或 `vmauth` 前端，负责将 `/insert/*` 和 `/select/*` 路由到对应分拆角色，同时透传 `AccountID`/`ProjectID`。分拆角色负责集群路由与分片；外部运维仍需自行配置前端冗余、存储副本（在引擎支持的前提下）与持久卷。

如需带内嵌存储的单 Pod 安装，可切换到 `allinone` 模式。Chart 会强制单副本、采用 `Recreate` 更新策略，并在 `/data` 挂载 PVC：

```sh
helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe --create-namespace \
  --set mode=allinone \
  --set persistence.size=50Gi
```

### 按 digest 固定网关镜像

`image.digest` 接收镜像仓库 digest，格式为 `sha256:` 加 64 位小写十六进制字符。设置后，Chart 会渲染为 `<repository>:<tag>@<digest>`；若 `image.tag` 为空，则渲染为 `<repository>@<digest>`。实际拉取的镜像字节由 digest 而非 tag 决定；若 digest 格式非法，模板渲染直接失败，不会静默回退到可变 tag：

```sh
helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe \
  --set secret.existingSecret=lazy-observe \
  --set image.tag=0.1.0 \
  --set image.digest=sha256:...
```

尽量避免手工复制 digest。每次服务端发版都会在 GitHub Release 附带 `helm-values-lazy-observe-<version>.yaml`，内容由 `docker/build-push-action` 将多架构镜像推送到 `ghcr.io/eimsound/lazy-observe` 后返回的 digest 生成，与该次发布的镜像字节完全对应：

```sh
gh release download v0.1.0 --pattern 'helm-values-lazy-observe-*.yaml'
helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe \
  -f deploy/helm/lazy-observe/values-standalone-ha-cluster.yaml \
  -f helm-values-lazy-observe-0.1.0.yaml
```

YAML 中的 `image.tag` 务必加引号（`tag: "0.1"`）。不加引号时，YAML 解析器会把 `0.10` 当作浮点数并抹去末尾的零变成 `0.1`；尽管 Chart 会把数字型 tag 转回字符串避免渲染报错，但版本号已与预期不符。

本 Chart 不打包发布为 `.tgz` 或 OCI 制品，而是直接从仓库 `deploy/helm/lazy-observe` 目录使用。因此发布流程不会把 digest 回写进仓库的 `values.yaml`：签入仓库的 digest 永远落后一个版本，且回填还需要发布流水线向 `main` 提交代码。

`standalone-ha` 模式必须具备平台加密密钥 Secret，可以是 `encryptionKey.existingSecret`，或使用默认 `encryptionKey.autoGenerate=true` 由 Chart 自动生成。由于密钥寿命必须长于 Pod，缺少密钥时 Chart 在任意副本数下都会拒绝渲染 `standalone-ha`；带 PVC 的 `allinone` 模式则把密钥留在 `data_dir` 内。详见上文“平台加密密钥”一节。

`/livez` 仅报告进程存活，不探测外部依赖。`/readyz` 启动初期返回 `503`，仅在初始化、数据库迁移与依赖探针全部通过后转为 `200`，一旦进入关停流程即刻回落为 `503`。`/healthz` 返回便于人工排查的组件聚合报告。`pre-stop` 生命周期钩子通过调用二进制内置的 pre-stop 辅助命令适配 distroless 镜像：先向 PID 1 发信号，再留出短暂的端点摘流窗口。默认 45 秒的 `terminationGracePeriodSeconds` 足以覆盖 20 秒网关排空、15 秒子进程关停超时以及钩子等待窗口。

可选 HPA（`hpa.enabled=true`）仅在 `standalone-ha` 下渲染并基于 CPU 利用率扩缩容，Chart 已默认配置 CPU requests 以确保指标可用。PDB 默认开启且 `minAvailable: 1`。请协同设置 HPA 最小副本数与 PDB，避免主动驱逐同时下线所有就绪副本。Ingress 默认关闭，可通过 `ingress.*` 开启。

[ADR 0001](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0001-nightingale-style-alert-engine.md) 定义了单主告警引擎实现。每个应用副本都运行评估候选者，通过一致性哈希分配与 PostgreSQL 租约/纪元栅栏，保证同一条规则的状态提交和 Outbox 通知始终只由唯一主副本执行。调整网关副本数只会转移规则归属，不会产生并发重复告警。该机制要求所有副本共享同一可写 PostgreSQL。

应用变更前，先校验两种模式的模板：

```sh
mise run helm-lint
mise run helm-template-test
```

PR CI 的 `go` 任务会在改动涉及服务端、Chart 或脚本时自动执行这两项 Helm 校验（`.github/workflows/ci.yml` 通过 `scripts/ci-changes.sh` 计算改动范围）。

多副本高可用门禁在独立的 `e2e-ha` 任务中执行，由同一分类脚本的 `ha` 标记触发。该标记覆盖服务端代码（`cmd/**`、`internal/**`、`go.mod`、`go.sum`、`sdk/go/**`）、HA 门禁脚本及其编译辅助程序、`scripts/download-storage-bins.sh`、`bench/performance-gates.json`、Ent 生成链路（`entc.*`、`tools/entgen/**`、`scripts/generate-ent.sh`），以及全量触发路径（`.github/**`、`mise.toml`、`scripts/ci-*`、`Dockerfile*`）。仅修改 `deploy/**` 下的 Chart 文件不会触发 `ha` 标记；如果 Chart 改动涉及多副本行为，请在本地执行 `mise run e2e-ha`。该门禁全部针对真实组件运行，而非 Mock：包含带副本的 VictoriaMetrics 集群（并演练单个 `vmstorage` 宕机）、分拆角色的 VictoriaLogs/VictoriaTraces 与双向租户隔离、基于 Apache-2.0 SeaweedFS 的 S3 跨副本会话回放读写、共享 PostgreSQL 告警状态与通知 Outbox、Redis 会话与限流、带栅栏的告警主权切换、就绪探针以及优雅摘流。运行需要 Docker；缺少 Docker 前置条件会返回 `77`（`SKIP`），CI 视其为失败，绝不算通过。

Chart 核心配置入口包括：`mode`、`replicaCount`、`image.{repository,tag,digest,pullPolicy}`、`externalURL`、`external.{postgres,redis,s3,metrics,logs,traces}`、`secret.*`、`resources`、`hpa.*`、`podDisruptionBudget.*`、`persistence.*`、`ingress.*`、`process.*`、`encryptionKey.*`（对应 `security.secret_key_file`）和 `adminPassword.*`（对应 `security.admin_password_file`）。

## PostgreSQL 元数据

在 YAML 或环境变量 `LO_META_DB_URL` 中配置 PostgreSQL 连接串：

```yaml
meta:
  db_url: "postgres://lazy_observe:password@postgres:5432/lazy_observe?sslmode=require"
  migration_lock_timeout: 60s
```

面对空库时，服务端会通过内置的单一 PostgreSQL 基线建表。请为 lazy-observe 分配独立数据库与账号。部署多个应用副本时，还必须配合 Redis 提供共享会话与限流。Issue 批次在缓冲期间保留在各进程内存，落库时在 PostgreSQL 中原子合并。

启动路径通过 PostgreSQL 咨询锁、有界等待、加锁后重读与带校验和的基线标记来协调建表。出现锁超时、连接中断、解锁失败、校验和漂移或建表失败时，实例会在就绪前中止启动。这套机制确保多副本首次并发启动安全，但不承担跨版本数据库升级。

迁移协调完成后，第二把 PostgreSQL 咨询锁会串行化首次启动的 `admin`、`default` 项目与自监控初始化。多副本因此可以同时连接空库启动，不会在种子数据或凭据上发生竞态。

首个受支持版本发布前，SQLite 与 PostgreSQL 各只维护一份可变的 `001_baseline.sql`。遇到不兼容 Schema 变更后需重建开发库；早期提交生成的数据库不属于受支持的升级来源。详见 [ADR 0003](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0003-pre-release-single-database-baseline.md)。

## S3 兼容对象存储

```yaml
blob:
  backend: s3
  s3:
    bucket: "lazy-observe"
    region: "us-east-1"
    endpoint: "https://s3.example.com"
    access_key: "..."
    secret_key: "..."
```

静态密钥留空时，服务端走 AWS SDK 默认凭据链。配置自定义 `endpoint` 后自动启用 path-style 请求。配置校验只检查 `bucket` 非空以及静态 `access_key`/`secret_key` 成对填写；存储桶连通性在实际读写时验证。

## 数据保留策略

保留时长按信号全局生效，不按项目区分。单机嵌入式（All-in-One）模式下，三项 Victoria 保留时长会作为启动参数传给对应子进程。平台内置的 1 小时清理任务会删除过期的平台元数据与 Blob。开启 `retention.issues_closed` 时，已关闭 Issue 的保留期固定为 90 天。

调短保留时长会让更早的数据进入引擎或清理器删除范围；若历史数据仍有用，请先备份。

## 预览期版本替换与备份

> 单机嵌入式（All-in-One）与 HA 模式的冷/热备份、`lazy-observe doctor --ha` 恢复自检、升级回滚、密钥轮换（`admin reset-key-fingerprint`）与密码应急重置（`admin reset-password`）完整步骤见[运维手册](operations.md)。

`v0.x` 属于公开预览版本。升级可能需要重建元数据数据库，协议与 SDK 在版本之间也可能出现不兼容调整。在首个受支持版本发布前，数据库基线保持可变；首个受支持版本不是 `v0.1.0`，目前也没有排期。

默认单机嵌入式（All-in-One）部署的替换步骤：

1. 优雅停止服务，等待全部受管子进程退出。
2. 完整拷贝配置好的 `data_dir` 作为一致性备份。
3. 替换单一 `lazy-observe` 可执行文件，该文件已打包 Web UI、存储清单和四套固定版本的平台存储可执行文件。
4. 若新版本变更了数据库基线，重建开发用 `data_dir`；否则直接启动网关。启动时会自动校验并原子更新 `data_dir/bin` 下解压的二进制，同时核对元数据基线。
5. 检查 `/readyz` 与 `/healthz`，登录控制台并验证信号查询正常后，再清理备份。

显式使用开发覆写项 `storage_bin_dir` 的部署，须把目录内四套可执行文件作为配套组合与网关一并更新。

切勿把运行中的 SQLite 和存储目录直接拷贝当作一致性备份。预览期版本间不支持元数据原地升级或降级。旧数据目录只能与创建它的精确二进制版本成对保留。

外部状态需分别备份：外部元数据走 PostgreSQL 原生备份，外部 Blob 依赖 S3 版本控制或快照，外部信号存储按各自支持的备份方案处理。在这些架构下，仅备份 `data_dir` 是不够的。

PostgreSQL 遇到基线变更后需启用全新的开发库。原有数据库备份可用于排查，但不保证能被新版本直接升级。首个受支持版本在开放用户升级前，必须先引入并文档化追加式迁移契约。

Helm 的 `startupProbe` 总时长必须大于 `meta.migration_lock_timeout` 与预期建表耗时之和。等待与初始化期间的实例保持未就绪状态，启动探针可防止存活探针在初始化完成前误杀容器。

## 维护者发布流程

发布与性能门禁入口：

| Mise 任务 | 门禁内容 |
| --- | --- |
| `mise run perf-release` | 构建并校验四套内嵌平台二进制的体积，随后执行原生零配置启动冒烟测试 |
| `mise run perf-ha` | 基于校验接收端验证中等语料查询 P95 与受控容量下的单副本弱扩展预算，并附带真实存储镜像回读校验 |
| `mise run performance-gates` | 按顺序执行全部必跑性能门禁 |

`mise run snapshot` 在生成本地不发布归档前会先跑 `perf-release`。`mise run release` 在调用 GoReleaser 前会执行迁移校验与 `perf-release`，且仅当 `HEAD` 存在精确 `v*` 标签时才会继续。内嵌制品与零配置冒烟检查在两条打包路径中都是硬前置条件，而非可选项。

推送 `v*` 标签会触发 `.github/workflows/release.yml`，在 GitHub Releases 发布 Linux `amd64`/`arm64` 归档与校验和，并把多架构容器镜像推至 `ghcr.io/eimsound/lazy-observe`。镜像推送完成后，工作流会用 `scripts/render-helm-release-values.sh` 按刚推送的镜像 digest 生成 `helm-values-lazy-observe-<version>.yaml` 并附到同一 Release 下，让 Helm 部署无需手工拷贝 digest。在构建归档之前，工作流要求双架构容器冒烟（含容器镜像与内嵌 Victoria 二进制的 Trivy 漏洞扫描）以及全套性能门禁（`bench`、`perf-release`、`perf-ha`、`perf-results-check`）全部通过；维护者也可手动触发该工作流重跑门禁而不发布。PR CI 不跑性能门禁（共享 Runner 抖动大），性能门禁只在手动触发 `bench.yml`、其每周定时运行与正式发版前执行。

PR CI 采用增量执行：`changes` 任务按 diff 分类，只跑受影响的任务。`ci-ok` 汇总所有任务（跳过计为通过），是 `main` 分支规则集里唯一的必过检查。纯文档改动（`*.md`、`docs/**`、`.agents/**`、`LICENSE`）不触发业务任务；服务端改动会加跑 `e2e-ha`，`internal/**` 或 `cmd/**` 改动会触发单副本 `e2e` 与 `e2e-restart`。分类器的全量回退规则由 `mise run ci-changes-test` 验证。

全部工作流运行在 GitHub 托管 Runner（`ubuntu-24.04` 与原生 `ubuntu-24.04-arm`）上。仓库刻意不注册自托管 Runner，防止 Fork PR 在维护者硬件上执行代码。发布流程使用工作流内置的 `GITHUB_TOKEN`（`contents: write` 与 `packages: write`），不需要个人访问令牌。四端 SDK 在同一仓库内以带前缀的标签发版：`sdk/go/vX.Y.Z`（Go 嵌套模块约定）、`sdk-node-vX.Y.Z`、`sdk-browser-vX.Y.Z` 和 `sdk-python-vX.Y.Z`。

## 网络与安全说明

- 默认只对外暴露 HTTP 网关端口（`:3100`，同时提供内嵌 Web UI）。可选的原生 OTLP/gRPC 端口（`:4317`）默认关闭（`otlp_grpc.enabled: false`）。严禁对外暴露内嵌子进程的本机回环端口（`18428`、`19428`、`20428`、`22428`）。
- 配置成对的 `security.tls_cert` 与 `security.tls_key`（或 `--tls-cert` 与 `--tls-key`）可开启原生 TLS 1.2+ 终止。在 Kubernetes 环境中，建议在可信反向代理处终止 TLS，同时启用 `security.session_secure_cookie: true` 并配置 `security.trusted_proxies`。
- 将所有凭据视作机密：项目上报令牌（`lo_...`）、Source Map 上传令牌（`los_...`）、用户 API 令牌（`lou_...`）与会话 Cookie（`lss_...`）。含项目 Token 的 DSN 属于敏感配置，不得打印到日志或提交到仓库。
- 互不信任的安全租户应部署各自独立的 lazy-observe 实例。
- 多副本 Kubernetes 部署请使用 `standalone-ha` 并接入共享的 PostgreSQL、Redis、S3 与外部信号存储。
