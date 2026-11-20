# lazy-observe

[English](README.md)

lazy-observe 是一套自托管的可观测性平台，整个服务是一个二进制文件。它收集应用的指标、日志、链路、错误、浏览器会话和性能剖析数据，在同一个 Web 界面里展示。不同项目的数据相互隔离。

二进制里内嵌了 Web 界面和四个存储程序：VictoriaMetrics、VictoriaLogs、VictoriaTraces 和 vmagent。首次启动时，lazy-observe 会把它们解压出来，作为只监听本机地址的子进程运行，子进程退出后会自动重启。元数据默认存在 SQLite 里，所以试用时不需要另外准备数据库。

> **公开预览。** v0.x 是预览版：升级可能需要重建数据库，协议和 SDK 可能出现不兼容变化。

lazy-observe 还没有发布过正式版本。想试用的话，请按下面的快速开始从源码构建。支持的平台是 Linux 的 amd64 和 arm64。

## 功能

- 指标、日志和链路。应用通过 OTLP/HTTP 或 Prometheus remote write 上报数据。指标用 PromQL 或 MetricsQL 查询，日志用 LogsQL 查询。
- 错误追踪。异常按堆栈归并成 Issue。lazy-observe 能识别回归，并用上传的 Source Map 把浏览器错误还原到源码位置。
- 浏览器监控。浏览器 SDK 记录 Web Vitals 和会话回放，并把每个浏览器请求关联到对应的后端链路。
- 性能剖析。为 Go 的 CPU、heap、goroutine 剖析和 Node 的 CPU、heap 剖析生成火焰图。
- 告警。告警规则用 PromQL 或 LogsQL 编写，通知可以发到 Webhook、飞书、钉钉、企业微信或 Slack。分组、去重、静默、抑制和按渠道限流可以减少重复通知。
- AI 分析。管理员配置好大模型服务后，lazy-observe 会结合相关的日志、链路和版本信息分析每个新 Issue。它也会统计你的应用调用大模型时的 token 用量和费用。
- MCP。AI 助手可以通过只读的 MCP 端点读取项目数据。MCP 读取要以用户身份鉴权：用 `lou_...` 用户 API token 加 `X-LO-Project` 请求头，`lo_...` 项目 API token 不能读。
- 基础设施。vmagent 负责抓取 Prometheus exporter。需要仪表盘时，把 Grafana 或 Perses 接到 Prometheus 兼容的查询 API 上，用用户 API token 加 `X-LO-Project` 请求头鉴权。

## 快速开始

需要 mise 2026.10.13 或更高版本、`curl`、`tar`，以及 `sha256sum` 或其他 SHA-256 工具。mise 会安装本仓库锁定版本的 Go、Node.js 和 pnpm。

```sh
git clone https://github.com/eimsound/lazy-observe.git
cd lazy-observe
mise install
mise run dist
./bin/lazy-observe server
```

`mise run dist` 会构建 Web 界面，下载并校验存储程序，再把它们一起嵌入 `bin/lazy-observe`。

首次启动时，服务会把 `admin` 用户的随机密码写入 `<data_dir>/initial-admin-password`（权限 0600），日志里只记这个路径；同时为默认项目打印一次摄取用的 API token。读完该文件后打开 <http://localhost:3100>，用 `admin` 登录；登录后必须先改掉这个随机密码才能做别的事。想自己指定初始密码，就设置 `security.admin_password_file`（见[部署文档](docs/deployment.md)），这样给出的密码不会被强制修改。

### 从 Go 应用上报数据

在 Fiber 应用里接入 Go SDK：

```go
lo := lazyobserve.Init(lazyobserve.Config{DSN: "http://127.0.0.1:3100/p/default@TOKEN"})
app.Use(lo.Fiber())
slog.SetDefault(lo.Slog())
```

把 `TOKEN` 换成服务启动时打印的 `lo_...` token。项目 token 只能上报数据；在界面、MCP 或 Grafana 里读同一个项目，要用用户 API token（`lou_...`），见[部署文档](docs/deployment.md)的 Query API, MCP, and Grafana 一节。

不想写代码的话，可以直接运行仓库里的示例应用：

```sh
cd sdk/go
LO_DSN='http://127.0.0.1:3100/p/default@TOKEN' mise exec -- go run ./examples/fiber
```

再开一个终端，同样进入 `sdk/go`，先发一个带链路的请求，再持续 30 秒发送快、慢、失败混合的请求：

```sh
curl http://127.0.0.1:3320/trace/order-42
mise exec -- go run ./examples/fiber load --duration 30s --concurrency 8
```

示例应用每秒导出一次指标和日志。「性能」页面显示吞吐量、延迟分位数和错误率，「链路」页面可以查看单个请求。示例的全部路由见 [`sdk/go/examples/fiber/README.md`](sdk/go/examples/fiber/README.md)。

## SDK

| 语言 | 包 | 集成 | 文档 |
| --- | --- | --- | --- |
| Go | `github.com/eimsound/lazy-observe/sdk/go` | Fiber v2、`slog`、`database/sql`、运行时指标、CPU/heap/goroutine 剖析 | [Go SDK](docs/sdk/go.md) |
| Node.js | `@lazy-observe/node` | Express、Pino、Winston、运行时指标、错误、CPU/heap 剖析 | [Node SDK](docs/sdk/node.md) |
| 浏览器 | `@lazy-observe/browser`、`@lazy-observe/react` | 错误、Web Vitals、fetch 与 XHR 链路、会话回放、React 错误边界与路由追踪 | [浏览器 SDK](docs/sdk/browser.md) |
| 浏览器构建 | `@lazy-observe/bundler-plugin` | 通过 Vite 插件或 CLI 上传 Source Map | [Source Map](docs/sdk/browser.md#source-maps) |
| Python | `lazyobserve` | Django、Django REST Framework、logging、数据库查询、大模型调用 | [Python SDK](docs/sdk/python.md) |

SDK 源码在 [`sdk/`](sdk/) 目录下。Go SDK 可以直接用 `go get` 安装。Node、浏览器和 Python 的包还没有发布到 npm 或 PyPI，各自的文档里写了如何从本仓库安装。

SDK 不会在服务端修改任何模块，每个集成都要在代码里显式接入，就像上面的 Go 示例。各个 SDK 版本与服务端版本的兼容关系见 [docs/version-matrix.md](docs/version-matrix.md)。

## 运行方式

```text
   SDKs, OpenTelemetry, Prometheus exporters
                     |
                     v
       lazy-observe gateway, port 3100
   sign-in, projects, API, issues, alerts, MCP
         |              |              |
         v              v              v
  VictoriaMetrics  VictoriaLogs  VictoriaTraces

  Metadata: SQLite or PostgreSQL
  Files such as replays and source maps: local disk or S3
```

只有 3100 端口对外提供服务。网关发给存储的每一次读写都会带上项目 ID，因此一个项目查不到另一个项目的数据。某个存储程序退出时，网关会把这个组件标记为未就绪，并按指数退避重启它，其他组件照常工作。

默认部署把所有组件放在一个进程组里运行。另有两种部署方式：

- Standalone 模式。网关连接你自己运维的 VictoriaMetrics、VictoriaLogs 和 VictoriaTraces。信号存储固定为 Victoria 系，按设计不支持 Loki 和 Tempo（ADR 0013）。
- 高可用。多个网关副本共享 PostgreSQL、Redis 和 S3 兼容存储。每条告警规则同一时间只由一个副本评估，由数据库里的租约决定是哪一个。Helm chart 里带有三副本配置。

两种方式的部署说明见 [部署文档](docs/deployment.md)，进程和安全边界的细节见 [架构文档](.agents/docs/architecture.md)。

## 开发

```sh
mise install
mise run dev
```

`mise run dev` 会启动 5173 端口的 Vite 开发服务器、3100 端口的网关和存储程序。Go 或 SQL 文件改动后，Air 会重新构建并重启网关。开发时打开 <http://localhost:5173>，它会把 `/api` 转发给网关。开发模式默认关闭自监控，需要时用 `LO_DEV_SELF_MONITORING=true mise run dev` 打开。

测试、检查项和 PR 规则见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 文档

- [架构](.agents/docs/architecture.md)
- [部署](docs/deployment.md)
- [SDK 与服务端兼容性](docs/version-matrix.md)
- [架构决策记录](.agents/docs/adr/)
- [贡献指南](CONTRIBUTING.md)
- [变更日志](CHANGELOG.md)

## 许可证

lazy-observe 使用 [MIT](LICENSE) 许可证。发布归档和容器镜像里还附带内嵌的 VictoriaMetrics 系列程序的 Apache-2.0 许可证，以及 Go 和 JavaScript 第三方依赖的声明。
