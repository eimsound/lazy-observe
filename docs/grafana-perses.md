# Grafana 与 Perses 对接指南

本文介绍如何将 lazy-observe 作为 **Grafana** 或 **Perses** 的项目隔离数据源，覆盖指标（PromQL / MetricsQL）、日志（VictoriaLogs / LogsQL）与链路（Jaeger 兼容查询）三类信号。

> **架构定位说明（[ADR 0017](../.agents/docs/adr/0017-no-built-in-dashboards.md)）**
>
> 按 ADR 0017，lazy-observe **不自建可编辑 Dashboard 系统，也不内置框架、主机或中间件（如 `node_exporter`、PostgreSQL、Redis 等）的 Dashboard 模板与视图**。产品内固定诊断页面（Overview、Application Performance、主机列表与详情、LLM 用量与成本、Metrics/Logs/Traces 查询工作区、Continuous Profiling）保持内置；需要自定义监控大盘、复用社区或存量看板时，推荐通过 **Grafana** 或 **Perses** 接入。

---

## 1. 鉴权契约与项目隔离（[ADR 0010](../.agents/docs/adr/0010-project-api-tokens-are-ingest-only.md)）

lazy-observe 对所有读接口强制执行用户鉴权与项目隔离：

- **必须使用用户 API Token（前缀 `lou_`）**，通过 `Authorization: Bearer lou_...` 发送；
- **必须通过 `X-LO-Project: <project-slug>` 请求头指定项目**（未传时默认查询 `default` 项目）；
- **一个数据源实例对应一个项目**：如需在 Grafana / Perses 中查看多个项目，请为每个项目分别配置一条数据源（更换 `X-LO-Project` 即可，可复用同一只读用户的 `lou_` Token）；
- **严禁使用项目 API Token（`lo_`）或 Source Map 上传 Token（`los_`）**：
  - 项目 API Token 会嵌入浏览器页面，只允许遥测写入；任何读接口收到 `lo_` Token 均返回 `403 Forbidden`（`code: ingest_only_token`）；
  - Source Map 上传 Token 收到读请求时返回 `403 Forbidden`（`code: upload_only_token`）。

### 创建只读用户 API Token

1. 建议管理员先创建一个 `viewer` 角色的专用集成账号（例如 `grafana-reader`），避免将 `admin` 权限暴露给外部看板工具；
2. 以该账号登录 Web UI，进入 **Settings > Personal API tokens**（设置 → 个人 API Token）创建 Token；
   - 或在已登录会话中附带 `X-CSRF-Token` 调用 `POST /api/auth/tokens` 创建；
3. 妥善保存仅展示一次的 `lou_...` 密钥，通过环境变量或 Secret 注入 Grafana / Perses，切勿明文提交到 Git 仓库。

---

## 2. 网关真实暴露的三类查询端点

下表对照 `internal/gateway/gateway.go` 与 `internal/gateway/proxy.go` 中当前真实注册的查询路由、鉴权策略与上游路径映射：

| 信号类型 | 网关暴露路径 | HTTP 方法 | 上游存储路径 | 隔离机制 | 外部工具直连说明 |
| --- | --- | --- | --- | --- | --- |
| **指标（PromQL / MetricsQL）** | `/api/v1/query`<br>`/api/v1/query_range`<br>`/api/v1/series`<br>`/api/v1/labels`<br>`/api/v1/label/:name/values`<br>`/api/v1/prettify-query` | `GET`/`POST`（`prettify-query` 仅 `GET`） | 同路径（VM 模式）；`prettify-query` → `/prettify-query` | 网关强制注入 `project_id` 过滤并剥离客户端 `extra_filters` / `extra_label` | **开箱直连**：Grafana `prometheus` 与 Perses `PrometheusDatasource` 直接填网关根 URL（如 `http://lazy-observe:3100`） |
| **日志（VictoriaLogs / LogsQL）** | `/api/v1/logs/query`<br>`/api/v1/logs/hits`<br>`/api/v1/logs/stats_query_range`<br>`/api/v1/logs/query_time_range`<br>`/api/v1/logs/field_names`<br>`/api/v1/logs/field_values`<br>`/api/v1/logs/stream_field_names`<br>`/api/v1/logs/stream_field_values`<br>`/api/v1/logs/tail`（实时流） | `GET`/`POST` | `/select/logsql/<op>` | 网关覆盖上游 `AccountID` / `ProjectID` 租户头 | 网关不暴露 `/select/logsql/*`（直接请求返回 `404`）；对接官方 VictoriaLogs 插件需经第 3 节轻量路径重写代理 |
| **链路（Jaeger 兼容 API）** | `/api/traces/services`<br>`/api/traces/services/:svc/operations`<br>`/api/traces/search`<br>`/api/traces/:traceID` | `GET` | `/select/jaeger/api/services`<br>`/select/jaeger/api/services/:svc/operations`<br>`/select/jaeger/api/traces`<br>`/select/jaeger/api/traces/:traceID` | 网关覆盖上游 `AccountID` / `ProjectID` 租户头；搜索仅放行白名单参数 | 详情 `/api/traces/:traceID` 与 Jaeger 一致，但服务列表与搜索路径不同；对接 Grafana `jaeger` 数据源需经第 3 节轻量路径重写代理 |

> **安全边界提醒**：切勿为了省略路径重写而直接将底层 VictoriaMetrics（`18428`）、VictoriaLogs（`19428`）或 VictoriaTraces（`20428`）端口暴露给 Grafana / Perses——那会完全绕过 lazy-observe 的用户鉴权与项目租户隔离。

### 已知协议边界

- **指标（Prometheus API）**：除上表 5 个标准查询端点与 `/api/v1/prettify-query` 外，`/api/v1/metadata`、`/api/v1/query_exemplars`、`/api/v1/rules`、`/api/v1/alerts`、`/api/v1/status/buildinfo` 等扩展端点未在网关注册，请求会返回 `404`，不影响常规 PromQL/MetricsQL 面板与变量查询。POST 请求体须使用 `application/x-www-form-urlencoded`。
- **链路搜索参数白名单**：`/api/traces/search` 仅转发 `service`、`operation`、`start`、`end`、`limit`、`minDuration`、`maxDuration`、`lookback`、`tags` 九个 Jaeger 查询参数。
- **实时日志流**：`/api/v1/logs/tail` 返回带 `X-Accel-Buffering: no` 与 `Cache-Control: no-cache` 的 NDJSON 流，受网关全局/每用户/每项目并发上限约束（超限返回 `429 live_tail_limit_exceeded`）。

---

## 3. 日志与链路插件的路径重写代理（Nginx / Caddy 样例）

Grafana 官方 VictoriaLogs 插件（`victoriametrics-logs-datasource`）与 Perses `VictoriaLogsDatasource` 默认请求 `<url>/select/logsql/*`；Grafana 内置 Jaeger 数据源（`type: jaeger`）默认请求 `<url>/api/services`、`<url>/api/services/<svc>/operations`、`<url>/api/traces`（搜索）与 `<url>/api/traces/<traceID>`（详情）。

在网关与 Grafana / Perses 之间放置一层轻量内网反向代理，即可无侵入完成路径映射，同时 **100% 透传 `Authorization: Bearer lou_...` 与 `X-LO-Project`**，不绕过网关鉴权：

### Nginx 示例

```nginx
server {
    listen 8080;

    # 1. Prometheus / MetricsQL（也可直接连网关 :3100，此处一并兼容）
    location /api/v1/ {
        proxy_pass http://lazy-observe:3100;
        proxy_set_header Host $host;
    }

    # 2. VictoriaLogs / LogsQL：/select/logsql/<op> -> /api/v1/logs/<op>
    location = /select/logsql/tail {
        proxy_pass http://lazy-observe:3100/api/v1/logs/tail$is_args$args;
        proxy_set_header Host $host;
        proxy_http_version 1.1;
        proxy_buffering off;
        proxy_read_timeout 3600s;
    }
    location ~ ^/select/logsql/(query|hits|stats_query_range|query_time_range|field_names|field_values|stream_field_names|stream_field_values)$ {
        proxy_pass http://lazy-observe:3100/api/v1/logs/$1$is_args$args;
        proxy_set_header Host $host;
    }

    # 3. Jaeger Traces：
    #    /api/services                  -> /api/traces/services
    #    /api/services/<svc>/operations -> /api/traces/services/<svc>/operations
    #    /api/traces?service=...        -> /api/traces/search?service=...
    #    /api/traces/<traceID>          -> /api/traces/<traceID>（原样透传）
    location = /api/services {
        proxy_pass http://lazy-observe:3100/api/traces/services$is_args$args;
        proxy_set_header Host $host;
    }
    location ~ ^/api/services/([^/]+)/operations$ {
        proxy_pass http://lazy-observe:3100/api/traces/services/$1/operations$is_args$args;
        proxy_set_header Host $host;
    }
    location = /api/traces {
        proxy_pass http://lazy-observe:3100/api/traces/search$is_args$args;
        proxy_set_header Host $host;
    }
    location ~ ^/api/traces/([^/]+)$ {
        proxy_pass http://lazy-observe:3100/api/traces/$1$is_args$args;
        proxy_set_header Host $host;
    }
}
```

### Caddy 示例

```caddyfile
:8080 {
    @logs_ops path_regexp logs ^/select/logsql/(query|hits|stats_query_range|query_time_range|field_names|field_values|stream_field_names|stream_field_values|tail)$
    handle @logs_ops {
        rewrite * /api/v1/logs/{re.logs.1}
        reverse_proxy http://lazy-observe:3100 {
            flush_interval -1
        }
    }

    handle /api/services {
        rewrite * /api/traces/services
        reverse_proxy http://lazy-observe:3100
    }

    @trace_ops path_regexp ops ^/api/services/([^/]+)/operations$
    handle @trace_ops {
        rewrite * /api/traces/services/{re.ops.1}/operations
        reverse_proxy http://lazy-observe:3100
    }

    handle /api/traces {
        rewrite * /api/traces/search
        reverse_proxy http://lazy-observe:3100
    }

    handle /api/traces/* {
        reverse_proxy http://lazy-observe:3100
    }

    handle /api/v1/* {
        reverse_proxy http://lazy-observe:3100
    }
}
```

---

## 4. Grafana 接入与 Provisioning

仓库提供即用的 Grafana Provisioning 样例：[deploy/integrations/grafana/datasources.yaml](../deploy/integrations/grafana/datasources.yaml)。

### 前置准备

1. **指标（`prometheus`）** 与 **链路（`jaeger`）** 为 Grafana 内置数据源类型，无需额外插件。
2. **日志（`victoriametrics-logs-datasource`）** 需安装官方插件（例如在 Grafana 容器环境变量中设置 `GF_INSTALL_PLUGINS=victoriametrics-logs-datasource`）。
3. 将样例文件挂载到 Grafana 的 `/etc/grafana/provisioning/datasources/lazy-observe.yaml`，并通过环境变量注入凭据（避免把 `lou_` Token 写死在文件中）：

```sh
export LAZY_OBSERVE_URL="http://lazy-observe:3100"
export LAZY_OBSERVE_PROXY_URL="http://lazy-observe-grafana-proxy:8080"
export LAZY_OBSERVE_PROJECT="default"
export LAZY_OBSERVE_USER_TOKEN="lou_<replace-with-viewer-token>"
```

### 在 Grafana UI 中手工添加要点

- **指标（Prometheus）**：
  - URL：`http://lazy-observe:3100`（网关根地址，**不加** `/api/v1`）
  - HTTP Method：`POST` 或 `GET`
  - Custom HTTP Headers：
    - `Authorization` = `Bearer lou_...`
    - `X-LO-Project` = `<project-slug>`
- **日志（VictoriaLogs）**：
  - URL：路径重写代理根地址（如 `http://lazy-observe-grafana-proxy:8080`）
  - Custom HTTP Headers：同上两个头
- **链路（Jaeger）**：
  - URL：路径重写代理根地址（如 `http://lazy-observe-grafana-proxy:8080`）
  - Custom HTTP Headers：同上两个头
  - 可在 **Trace to logs** 中绑定上面的 VictoriaLogs 数据源，实现从链路 Span 直接关联同项目日志。

---

## 5. Perses 接入与声明式配置

仓库提供即用的 Perses 声明式配置样例：[deploy/integrations/perses/datasources.yaml](../deploy/integrations/perses/datasources.yaml)，包含 `Project`、`Secret`（承载 `Bearer lou_...` 凭据）与项目级 `Datasource` 定义。

### 使用 `percli` 下发配置

1. 复制 [deploy/integrations/perses/datasources.yaml](../deploy/integrations/perses/datasources.yaml)，替换其中的 `lou_...` Token（生产环境推荐改用 `credentialsFile` 挂载只读 Secret 文件）与目标地址、`X-LO-Project`；
2. 使用 `percli` 应用配置：

   ```sh
   percli login http://perses:8080
   percli apply -f deploy/integrations/perses/datasources.yaml
   ```

### Perses 三类数据源支持说明

- **指标（`PrometheusDatasource`）**：通过 `HTTPProxy` 直连 `http://lazy-observe:3100`，`allowedEndpoints` 精确收敛至网关已注册的 5 个 `/api/v1/*` 路由，开箱即用。
- **日志（`VictoriaLogsDatasource`）**：需在 Perses 侧启用官方 VictoriaLogs 插件，并将 `HTTPProxy.spec.url` 指向第 3 节的路径重写代理，复用同一个 `Secret` 与 `X-LO-Project` 头。
- **链路（Traces）**：Perses 内置追踪插件主要面向 Grafana Tempo 的 TraceQL（`TempoDatasource`），而 lazy-observe 按 [ADR 0013](../.agents/docs/adr/0013-signal-storage-is-victoria-only.md) 仅使用 VictoriaTraces 并暴露 Jaeger 兼容查询接口，不提供 Tempo TraceQL 协议。在 Perses 看板中建议通过面板链接（Links）直接跳转至 lazy-observe Web UI 的链路详情页，或使用 Grafana `jaeger` 数据源做链路检索。

---

## 6. 连通性验证与常见故障排查

### 使用 `curl` 逐项验证网关端点

```sh
export LO_URL="http://127.0.0.1:3100"
export LO_TOKEN="lou_<your-user-api-token>"
export LO_PROJECT="default"

# 1. 指标（PromQL 瞬时查询）
curl -fsS -G "${LO_URL}/api/v1/query" \
  -H "Authorization: Bearer ${LO_TOKEN}" \
  -H "X-LO-Project: ${LO_PROJECT}" \
  --data-urlencode 'query=up'

# 2. 日志（VictoriaLogs LogsQL 查询）
curl -fsS -G "${LO_URL}/api/v1/logs/query" \
  -H "Authorization: Bearer ${LO_TOKEN}" \
  -H "X-LO-Project: ${LO_PROJECT}" \
  --data-urlencode 'query=*' \
  --data-urlencode 'limit=5'

# 3. 链路（Jaeger 服务列表与检索）
curl -fsS "${LO_URL}/api/traces/services" \
  -H "Authorization: Bearer ${LO_TOKEN}" \
  -H "X-LO-Project: ${LO_PROJECT}"

curl -fsS -G "${LO_URL}/api/traces/search" \
  -H "Authorization: Bearer ${LO_TOKEN}" \
  -H "X-LO-Project: ${LO_PROJECT}" \
  --data-urlencode 'limit=5'
```

### 常见错误排查表

| HTTP 状态 / 现象 | 原因 | 解决方法 |
| --- | --- | --- |
| `403` 且响应含 `code: ingest_only_token` | 误用了项目摄取 Token（`lo_...`） | 改为在 **Settings > Personal API tokens** 生成 `lou_...` 用户 API Token |
| `403` 且响应含 `code: upload_only_token` | 误用了 Source Map 上传 Token（`los_...`） | 改为使用 `lou_...` 用户 API Token |
| `401 unauthorized` | 缺失 `Authorization` 头、Token 已被吊销或拼写错误 | 检查是否带 `Bearer ` 前缀及 Token 是否有效 |
| `404`（访问 `/select/logsql/...` 或 `/api/services`） | 插件直连网关未经过路径重写代理 | 按第 3 节部署轻量反向代理映射至 `/api/v1/logs/*` 与 `/api/traces/*` |
| `404`（Grafana 测试连接时部分扩展探测报错） | Grafana 调用了未注册的 `/api/v1/status/buildinfo`、`/api/v1/metadata` 或 `/api/v1/rules` | 属于预期行为；直接在 Explore 执行 `up` 等查询验证即可 |
| 查不到目标项目数据 | 未传 `X-LO-Project`（落到 `default` 项目）或项目 slug 拼错 | 核对 `X-LO-Project` 与 Web UI 中项目的 slug 一致 |
| 外部 VictoriaTraces 查不到最近 30 秒内的新链路 | 外部 VictoriaTraces 默认 `-search.latencyOffset=30s`、`-insert.indexFlushInterval=20s` | 按 [deployment.md](deployment.md#standalone) 调低外部 VictoriaTraces 的这两个启动参数 |
