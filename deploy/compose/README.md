# Docker Compose 部署指南

本目录提供开箱可用的 Docker Compose 参考配置，支持两种形态：

1. **单机 All-in-One（默认）**：仅启动 `lazy-observe` 容器，使用 SQLite + 本地 `/data/blobs` + 内嵌 VictoriaMetrics / VictoriaLogs / VictoriaTraces / vmagent。
2. **外置存储参考栈（`--profile standalone`）**：连接独立的 PostgreSQL 16、Valkey 8（BSD-3-Clause，Redis 协议兼容）、SeaweedFS（Apache-2.0，S3 兼容对象存储）与外部 VictoriaMetrics / VictoriaLogs / VictoriaTraces（按 `LO_TOPOLOGY=ha` 运行以严格校验共享状态前提）。

---

## 1. 快速启动（All-in-One）

```sh
cd deploy/compose
cp .env.example .env
# 生产环境请将 .env 中的 LO_IMAGE 固定为发布版本 tag@sha256:<digest>
docker compose up -d
```

### 获取首次随机管理员密码与默认项目 Token

首次启动空卷时，服务端会：
- 生成随机 `admin` 密码并写入 `/data/initial-admin-password`（权限 `0600`），日志只打印文件路径，不打印明文；
- 在日志中打印一次 `default` 项目的摄取 Token（`lo_...`）。

查看日志与读取初始密码（镜像为 distroless，无 shell，通过一次性只读工具容器挂载同卷读取）：

```sh
docker compose logs lazy-observe

# 使用相同命名卷读取首次管理员密码（完成后首次登录强制改密，网关会自动删除该文件）
docker run --rm --user 65532:65532 \
  -v compose_lazy-observe-data:/data:ro \
  debian:trixie-slim cat /data/initial-admin-password
```

> 若希望自行指定初始管理员密码，可在宿主机创建密码文件（≥ 8 字符，权限对 UID `65532` 可读）并以 `:ro` 挂载进容器，配合设置 `LO_SECURITY_ADMIN_PASSWORD_FILE`（参考 `lazy-observe-standalone` 服务配置）。

---

## 2. 启动外置存储参考栈（Standalone / HA 形态）

`lazy-observe-standalone` 默认以 `LO_TOPOLOGY=ha` 启动，启动前需要准备好共享的平台加密密钥文件与初始管理员密码文件（两者均以只读方式挂载进容器，绝不通过环境变量传递明文）：

```sh
cd deploy/compose
cp .env.example .env
# 编辑 .env，替换 LO_PG_PASSWORD 与 LO_BLOB_S3_SECRET_KEY 为强随机口令

# 在受限 umask 子 shell 中创建密钥文件，避免短暂出现 0644 全局可读窗口
(
  umask 077
  install -d -m 0750 secrets
  openssl rand -base64 32 > secrets/secret.key
  openssl rand -base64 24 > secrets/admin-password
  chmod 0600 secrets/secret.key secrets/admin-password
)
# distroless 镜像以 UID/GID 65532:65532 运行；最后统一调整属主
sudo chown -R 65532:65532 secrets

# 先拉起外部依赖并等待健康检查就绪（--wait 会等待 postgres、valkey 与 seaweedfs 变为 healthy）
docker compose --profile standalone up -d --wait \
  postgres valkey seaweedfs victoria-metrics victoria-logs victoria-traces

# 启动网关前先运行只读 HA 预检
docker compose --profile standalone run --rm --entrypoint /usr/local/bin/lazy-observe \
  lazy-observe-standalone doctor --ha

# 全部 PASS 后启动网关
docker compose --profile standalone up -d lazy-observe-standalone
```

---

## 3. 安全上下文、卷权限与健康检查说明

- **非 root 与只读根文件系统**：容器固定以 `65532:65532`（distroless `nonroot`）运行，启用 `read_only: true`、`no-new-privileges:true` 与 `cap_drop: [ALL]`，仅开放 `/data` 数据卷与 `/tmp`（`noexec,nosuid`）临时文件系统。
- **`/data` 不可挂载 `noexec`**：All-in-One 模式与开启 `vmagent` 的 Standalone 模式会在启动时校验并将内嵌的 Victoria 可执行文件解压到 `/data/bin` 运行；若将 `/data` 改为宿主机 bind mount，请确保：
  1. 宿主机目录属主为 `65532:65532`（`sudo chown -R 65532:65532 /path/to/data`）；
  2. 所在挂载点允许执行（不能带 `noexec`）。
- **关于健康检查（distroless 限制）**：
  - `postgres`、`valkey` 与 `seaweedfs`（S3 端口 `8333`）已在 `docker-compose.yml` 中配置容器内 `healthcheck`；
  - `ghcr.io/eimsound/lazy-observe` 基于 `gcr.io/distroless/static-debian13:nonroot`，**镜像内不含 `sh`、`curl` 或 `wget`**，因此不在该容器上配置会必然报错的 `curl` healthcheck；
  - 请在宿主机、前置反向代理（Nginx / Caddy / Traefik）或外部监控探针上检测以下三个内置探针端点：
    - `GET http://127.0.0.1:3100/livez`（进程存活）
    - `GET http://127.0.0.1:3100/readyz`（初始化、迁移与全部依赖就绪，关停时立刻变 `503`）
    - `GET http://127.0.0.1:3100/healthz`（各组件聚合健康报告）
- **仅暴露 3100 端口**：默认绑定 `127.0.0.1:3100`，建议由前置 TLS 反向代理终止 HTTPS，并在 `.env` 中开启 `LO_SECURITY_SESSION_SECURE_COOKIE=true` 与配置 `LO_SECURITY_TRUSTED_PROXIES`。切勿暴露内嵌存储回环端口（`18428`/`19428`/`20428`/`22428`）。
- **优雅关停**：`stop_signal: SIGTERM` 与 `stop_grace_period: 45s` 覆盖了默认的 `drain_timeout`（`20s`）与 `shutdown_timeout`（`15s`）。

---

## 4. 备份、升级与应急维护

- 完整备份、恢复、升级回滚及 `admin reset-key-fingerprint` / `admin reset-password` 应急流程见 [docs/operations.md](../../docs/operations.md)。
- 在 Compose 环境中执行离线 CLI 维护（例如重置密码）：

```sh
docker compose run --rm --entrypoint /usr/local/bin/lazy-observe \
  lazy-observe admin reset-password
```

- 接入 Grafana / Perses 仪表盘见 [docs/grafana-perses.md](../../docs/grafana-perses.md)。
