# 五分钟快速上手

当前尚未发布 `v0.1.0` 正式 tag，因此可以先从源码构建单二进制，或在本地构建镜像后用 Docker Compose 启动。待 `v0.1.0` 发布后，GitHub Releases 静态归档、`scripts/install.sh` 以及 `ghcr.io/eimsound/lazy-observe` 签名多架构镜像都可以直接运行，无需本地构建。

## 1. 启动 lazy-observe

### 从源码构建并运行单二进制

受支持的目标平台为 Linux `amd64` 与 `arm64`。前置依赖包括 `mise` `>= 2026.10.13`、`curl`、`tar` 以及 `sha256sum`（或 `shasum`）。`mise` 会自动安装仓库锁定的 Go、Node.js 和 pnpm 版本。

```sh
git clone https://github.com/eimsound/lazy-observe.git
cd lazy-observe
mise install
mise run dist
./bin/lazy-observe server
```

`mise run dist` 会编译 Web UI、下载并校验四个 Victoria 存储二进制的 SHA-256，并将两者嵌入 `bin/lazy-observe`。

### 使用 Docker Compose 启动

先在本地构建镜像，再用 POSIX `sed` 基于 [`deploy/compose/.env.example`](https://github.com/eimsound/lazy-observe/blob/main/deploy/compose/.env.example) 生成 `.env` 并启动服务：

```sh
git clone https://github.com/eimsound/lazy-observe.git
cd lazy-observe
docker build -t lazy-observe:local .
cd deploy/compose
sed 's|^LO_IMAGE=.*|LO_IMAGE=lazy-observe:local|' .env.example > .env
docker compose up -d
```

外置存储参考栈（`--profile standalone`，包含 PostgreSQL、Valkey、SeaweedFS 与外部 Victoria 服务）见 [Docker Compose 指南](https://github.com/eimsound/lazy-observe/blob/main/deploy/compose/README.md)，Linux 物理机或虚拟机部署见 [systemd 指南](https://github.com/eimsound/lazy-observe/blob/main/deploy/systemd/README.md)。

## 2. 登录并修改初始管理员密码

首次面对空 `data_dir`（默认 `./data`）启动时，服务端会执行两项初始化操作：

- 生成随机 `admin` 密码并写入 `<data_dir>/initial-admin-password`（权限 `0600`），日志只记录文件路径，不打印密码明文。
- 在日志中打印一次 `default` 项目的摄取 API Token（`lo_...`）。

查看初始密码：

```sh
# 本地单二进制：
cat ./data/initial-admin-password

# Docker Compose（distroless 镜像不含 shell）：
docker compose logs lazy-observe
docker run --rm --user 65532:65532 \
  -v compose_lazy-observe-data:/data:ro \
  debian:trixie-slim cat /data/initial-admin-password
```

打开 <http://localhost:3100> 并以 `admin` 登录。首次登录必须先替换这个随机密码，改密完成前其他路由都不可访问；改密后网关会自动删除 `initial-admin-password`。如果希望自行指定初始密码而不强制改密，可配置 `security.admin_password_file`（`LO_SECURITY_ADMIN_PASSWORD_FILE`），详见[部署指南](deployment.md)。

## 3. 获取项目摄取 Token

你可以直接使用首次启动日志中打印的 `default` 项目 `lo_...` Token，或在 Web UI 的 **Settings** 下打开 **Projects** 新建项目并生成项目 API Token。

- `lo_...` 令牌仅限数据摄取，访问任何读接口都会返回 `403 ingest_only_token`。
- 接入 Grafana、Perses、MCP 或查询 API 时，请在 **Settings** 下的 **Personal API tokens** 创建个人 API Token（`lou_...`），并在请求头附带 `X-LO-Project: <slug>`，配置方式见[部署指南](deployment.md)。

## 4. 发送遥测数据并在 Web UI 查看

### 用 `curl` 发送一条错误事件

先通过 `POST /api/events/errors` 发送一条原生 JSON 错误事件验证链路：

```sh
NOW_ISO=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EVENT_ID="quickstart-$(date -u +%Y%m%d%H%M%S)"
curl -fsS -X POST http://127.0.0.1:3100/api/events/errors \
  -H "Authorization: Bearer lo_<YOUR_PROJECT_TOKEN>" \
  -H "Content-Type: application/json" \
  -d "{\"events\":[{\"event_id\":\"${EVENT_ID}\",\"timestamp\":\"${NOW_ISO}\",\"platform\":\"go\",\"level\":\"error\",\"message\":\"hello from lazy-observe quickstart\",\"exception\":{\"type\":\"QuickstartError\",\"value\":\"hello from lazy-observe quickstart\"}}]}"
```

接口返回 `{"accepted":1}` 后，事件会立刻出现在 **Issues** 页面。任意标准 OpenTelemetry SDK 也可以通过 OTLP/HTTP 直接向 `http://127.0.0.1:3100` 上报数据，请使用 `http/protobuf`（`application/x-protobuf`）：`/v1/metrics` 与 `/v1/logs` 仅接受 Protobuf，`/v1/traces` 同时支持 Protobuf 与 JSON。开启 `LO_OTLP_GRPC_ENABLED=true` 后，也可通过 OTLP/gRPC 向 `127.0.0.1:4317` 上报，请求头携带 `Authorization: Bearer lo_<YOUR_PROJECT_TOKEN>`。

### 运行 Go Fiber v3 示例应用

如需一次性体验指标、日志、链路、异常与后端接口性能，可以运行仓库自带的 [Go Fiber v3 示例应用](https://github.com/eimsound/lazy-observe/blob/main/sdk/go/examples/fiber/README.md)：

```sh
cd sdk/go/examples/fiber
LO_DSN='http://127.0.0.1:3100/p/default@lo_<YOUR_PROJECT_TOKEN>' \
  mise exec -- go run .
```

打开第二个终端，触发带链路的请求、一次 panic 以及 30 秒压测流量：

```sh
cd sdk/go/examples/fiber
curl http://127.0.0.1:3320/trace/order-42
curl http://127.0.0.1:3320/panic
mise exec -- go run . load --url http://127.0.0.1:3320 --duration 30s --concurrency 8
```

### 在 Web UI 查看实时数据

打开 <http://localhost:3100>，按模块核对数据：

- **Logs（日志）。** 在 LogsQL 中检索 `lazy-observe-fiber-example` 或 `"order trace complete"`，查看带 `trace_id` 关联的日志。
- **Metrics（指标）。** 用可视化 PromQL 查询构建器（原生支持点分指标名）或 CodeMirror 编辑器查询 `http.server.request.duration_count` 与 `http.server.request.duration_bucket`。
- **Traces（链路）。** 打开 `/trace/order-42` 的瀑布图，查看 HTTP 服务端 Span 与对应的 SQL 子 Span。
- **Issues（问题）。** 查看前面的 `QuickstartError` 冒烟事件以及 `/panic` 自动归并后的异常 Issue、关联日志与链路上下文。
- **Performance（性能）。** 切换到 **后端接口（Backend endpoints）** 标签页查看 Fiber 示例产生的路由吞吐量、P50/P95/P99 延迟与错误率；默认的 **Web Vitals** 标签页会在接入浏览器 SDK 或向 `POST /api/events/vitals` 上报数据后展示地理与设备维度分布。

## 下一步

- 查阅[部署指南](deployment.md)，配置外置存储、TLS 或 Kubernetes 高可用集群。
- 按业务技术栈接入 [Go](sdk/go.md)、[Node.js](sdk/node.md)、[Browser 与 React](sdk/browser.md) 或 [Python](sdk/python.md) SDK。
- 查阅 [Grafana 与 Perses 对接指南](grafana-perses.md)接入外部看板，并在[运维手册](operations.md)中了解备份与恢复流程。
