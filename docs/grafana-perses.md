# Grafana and Perses integration

This guide explains how to connect lazy-observe as a project-isolated data source in Grafana or Perses across three telemetry signals: metrics (PromQL and MetricsQL), logs (VictoriaLogs and LogsQL), and traces (Jaeger-compatible queries).

> **Architecture note ([ADR 0017](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0017-no-built-in-dashboards.md))**
>
> Under ADR 0017, lazy-observe does not include an editable dashboard engine and does not ship built-in dashboard templates or views for frameworks, hosts, or middleware such as `node_exporter`, PostgreSQL, or Redis. Fixed diagnostic views stay built into the product: Overview, Application Performance, host list and details, LLM usage and cost, Metrics/Logs/Traces query workspaces, and Continuous Profiling. Connect Grafana or Perses when you need custom dashboards or want to reuse community or existing dashboards.

## 1. Authentication contract and project isolation

See [ADR 0010](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0010-project-api-tokens-are-ingest-only.md) for the token separation design. lazy-observe enforces user authentication and project isolation on every read endpoint:

- **Use a user API token.** Send a token with the `lou_` prefix in the `Authorization: Bearer lou_...` header.
- **Select the project explicitly.** Pass the `X-LO-Project: <project-slug>` header on every request. When omitted, the gateway queries the `default` project.
- **Map one data source instance to one project.** To query multiple projects in Grafana or Perses, create one data source per project with a different `X-LO-Project` value. Those data sources can share the same read-only user's `lou_` token.
- **Reject ingest and upload tokens on read routes.** Project API tokens (`lo_`) are embedded in browser pages and allow telemetry ingestion only. Any read endpoint that receives a `lo_` token returns `403 Forbidden` with `code: ingest_only_token`. Any read endpoint that receives a Source Map upload token (`los_`) returns `403 Forbidden` with `code: upload_only_token`.

### Create a read-only user API token

1. Create a dedicated integration user with the `viewer` role, such as `grafana-reader`, so external dashboard tools do not hold `admin` privileges.
2. Sign in to the web UI as that user, open Settings, and create a token under Personal API tokens. You can also call `POST /api/auth/tokens` with `X-CSRF-Token` from an authenticated session.
3. Save the one-time `lou_...` secret and inject it into Grafana or Perses through environment variables or a secret manager. Do not commit the token to Git in plaintext.

## 2. Query endpoints exposed by the gateway

The table below reflects the query routes, isolation rules, and upstream mappings registered in `internal/gateway/gateway.go` and `internal/gateway/proxy.go`.

| Signal | Gateway path | HTTP methods | Upstream storage path | Isolation mechanism | Direct connection notes |
| --- | --- | --- | --- | --- | --- |
| Metrics (PromQL and MetricsQL) | `/api/v1/query`<br>`/api/v1/query_range`<br>`/api/v1/series`<br>`/api/v1/labels`<br>`/api/v1/label/:name/values`<br>`/api/v1/prettify-query` | `GET`, `POST` (`prettify-query` accepts `GET` only) | Same path in VictoriaMetrics mode; `/api/v1/prettify-query` maps to `/prettify-query` | The gateway enforces a `project_id` filter and strips client `extra_filters` and `extra_label` parameters. | Works out of the box. Point Grafana `prometheus` and Perses `PrometheusDatasource` at the gateway root URL, such as `http://lazy-observe:3100`. |
| Logs (VictoriaLogs and LogsQL) | `/api/v1/logs/query`<br>`/api/v1/logs/hits`<br>`/api/v1/logs/stats_query_range`<br>`/api/v1/logs/query_time_range`<br>`/api/v1/logs/field_names`<br>`/api/v1/logs/field_values`<br>`/api/v1/logs/stream_field_names`<br>`/api/v1/logs/stream_field_values`<br>`/api/v1/logs/tail` (live stream) | `GET`, `POST` | `/select/logsql/<op>` | The gateway overwrites upstream `AccountID` and `ProjectID` tenant headers. | The gateway does not expose `/select/logsql/*` and returns `404` for direct requests. Official VictoriaLogs plugins need the path-rewriting proxy in Section 3. |
| Traces (Jaeger-compatible API) | `/api/traces/services`<br>`/api/traces/services/:svc/operations`<br>`/api/traces/search`<br>`/api/traces/:traceID` | `GET` | `/select/jaeger/api/services`<br>`/select/jaeger/api/services/:svc/operations`<br>`/select/jaeger/api/traces`<br>`/select/jaeger/api/traces/:traceID` | The gateway overwrites upstream `AccountID` and `ProjectID` tenant headers. Search forwards only allowlisted parameters. | Trace detail at `/api/traces/:traceID` matches Jaeger, while the service list and search paths differ. Grafana `jaeger` data sources need the path-rewriting proxy in Section 3. |

> **Security boundary.** Never expose the underlying VictoriaMetrics (`18428`), VictoriaLogs (`19428`), or VictoriaTraces (`20428`) ports directly to Grafana or Perses to avoid path rewriting. Direct access bypasses lazy-observe user authentication and project isolation completely.

### Known protocol boundaries

- **Metrics (Prometheus API).** Beyond the five standard query endpoints and `/api/v1/prettify-query`, extended endpoints such as `/api/v1/metadata`, `/api/v1/query_exemplars`, `/api/v1/rules`, `/api/v1/alerts`, and `/api/v1/status/buildinfo` are not registered on the gateway and return `404`. Standard PromQL and MetricsQL panels and variable queries work without them. `POST` request bodies must use `application/x-www-form-urlencoded`.
- **Trace search allowlist.** `/api/traces/search` forwards only these nine Jaeger query parameters: `service`, `operation`, `start`, `end`, `limit`, `minDuration`, `maxDuration`, `lookback`, and `tags`.
- **Live log stream.** `/api/v1/logs/tail` returns an NDJSON stream with `X-Accel-Buffering: no` and `Cache-Control: no-cache`. The gateway enforces global, per-user, and per-project concurrency caps and returns `429 live_tail_limit_exceeded` when a cap is reached.

## 3. Path-rewriting proxy for logs and traces

The official Grafana VictoriaLogs plugin (`victoriametrics-logs-datasource`) and Perses `VictoriaLogsDatasource` request `<url>/select/logsql/*` by default. Grafana's built-in Jaeger data source (`type: jaeger`) requests `<url>/api/services`, `<url>/api/services/<svc>/operations`, `<url>/api/traces` for search, and `<url>/api/traces/<traceID>` for trace details.

Place a lightweight internal reverse proxy between the gateway and Grafana or Perses to rewrite those paths. The proxy forwards `Authorization: Bearer lou_...` and `X-LO-Project` unchanged, so gateway authentication and tenant isolation stay in force.

### Nginx example

```nginx
server {
    listen 8080;

    # 1. Prometheus and MetricsQL (you can also connect directly to the gateway on :3100)
    location /api/v1/ {
        proxy_pass http://lazy-observe:3100;
        proxy_set_header Host $host;
    }

    # 2. VictoriaLogs and LogsQL: map /select/logsql/<op> to /api/v1/logs/<op>
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

    # 3. Jaeger Traces:
    #    /api/services maps to /api/traces/services
    #    /api/services/<svc>/operations maps to /api/traces/services/<svc>/operations
    #    /api/traces?service=... maps to /api/traces/search?service=...
    #    /api/traces/<traceID> passes through unchanged
    location = /api/services {
        proxy_pass http://lazy-observe:3100/api/traces/services$is_args$args;
        proxy_set_header Host $host;
    }
    # Works for standard service.name values without '/' or whitespace.
    # For names with special characters, URL-encode on the client or call the gateway directly.
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

### Caddy example

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

## 4. Grafana setup and provisioning

The repository includes a ready-to-use Grafana provisioning file at [deploy/integrations/grafana/datasources.yaml](https://github.com/eimsound/lazy-observe/blob/main/deploy/integrations/grafana/datasources.yaml).

### Prerequisites

1. Metrics (`prometheus`) and traces (`jaeger`) are built-in Grafana data source types and need no extra plugins.
2. Logs (`victoriametrics-logs-datasource`) require the official plugin. For container deployments, set `GF_INSTALL_PLUGINS=victoriametrics-logs-datasource`.
3. Mount the sample file at `/etc/grafana/provisioning/datasources/lazy-observe.yaml` and pass credentials through environment variables instead of hardcoding `lou_` tokens in the file:

```sh
export LAZY_OBSERVE_URL="http://lazy-observe:3100"
export LAZY_OBSERVE_PROXY_URL="http://lazy-observe-grafana-proxy:8080"
export LAZY_OBSERVE_PROJECT="default"
export LAZY_OBSERVE_USER_TOKEN="lou_<replace-with-viewer-token>"
```

### Manual configuration in the Grafana UI

- **Metrics (Prometheus).** Set URL to `http://lazy-observe:3100` without `/api/v1`, choose `POST` or `GET` as the HTTP method, and add two custom HTTP headers: `Authorization` set to `Bearer lou_...` and `X-LO-Project` set to `<project-slug>`.
- **Logs (VictoriaLogs).** Set URL to the path-rewriting proxy root URL, such as `http://lazy-observe-grafana-proxy:8080`, and add the same two custom HTTP headers.
- **Traces (Jaeger).** Set URL to the path-rewriting proxy root URL, such as `http://lazy-observe-grafana-proxy:8080`, and add the same two custom HTTP headers. You can bind the VictoriaLogs data source in Trace to logs to jump from a trace span to logs in the same project.

## 5. Perses setup and declarative configuration

The repository includes a declarative Perses configuration file at [deploy/integrations/perses/datasources.yaml](https://github.com/eimsound/lazy-observe/blob/main/deploy/integrations/perses/datasources.yaml). It defines a `Project`, a `Secret` carrying the `Bearer lou_...` credential, and project-scoped `Datasource` resources.

### Apply with `percli`

1. Copy [deploy/integrations/perses/datasources.yaml](https://github.com/eimsound/lazy-observe/blob/main/deploy/integrations/perses/datasources.yaml) and update the `lou_...` token, target URLs, and `X-LO-Project` value. In production, mount a read-only secret file and reference it with `credentialsFile`.
2. Apply the manifest with `percli`:

   ```sh
   percli login http://perses:8080
   percli apply -f deploy/integrations/perses/datasources.yaml
   ```

### Signal support in Perses

- **Metrics (`PrometheusDatasource`).** Connects directly to `http://lazy-observe:3100` through `HTTPProxy`. `allowedEndpoints` restricts access to the five registered `/api/v1/*` routes.
- **Logs (`VictoriaLogsDatasource`).** Enable the official VictoriaLogs plugin in Perses, point `HTTPProxy.spec.url` at the path-rewriting proxy from Section 3, and reuse the same `Secret` and `X-LO-Project` header.
- **Traces.** The built-in Perses tracing plugin targets Grafana Tempo TraceQL (`TempoDatasource`). Under [ADR 0013](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0013-signal-storage-is-victoria-only.md), lazy-observe uses VictoriaTraces and exposes a Jaeger-compatible query API rather than Tempo TraceQL. In Perses dashboards, add panel links to trace detail pages in the lazy-observe web UI, or use Grafana's `jaeger` data source for trace search.

## 6. Connectivity verification and troubleshooting

### Verify gateway endpoints with `curl`

```sh
export LO_URL="http://127.0.0.1:3100"
export LO_TOKEN="lou_<your-user-api-token>"
export LO_PROJECT="default"

# 1. Metrics (PromQL instant query)
curl -fsS -G "${LO_URL}/api/v1/query" \
  -H "Authorization: Bearer ${LO_TOKEN}" \
  -H "X-LO-Project: ${LO_PROJECT}" \
  --data-urlencode 'query=up'

# 2. Logs (VictoriaLogs LogsQL query)
curl -fsS -G "${LO_URL}/api/v1/logs/query" \
  -H "Authorization: Bearer ${LO_TOKEN}" \
  -H "X-LO-Project: ${LO_PROJECT}" \
  --data-urlencode 'query=*' \
  --data-urlencode 'limit=5'

# 3. Traces (Jaeger service list and search)
curl -fsS "${LO_URL}/api/traces/services" \
  -H "Authorization: Bearer ${LO_TOKEN}" \
  -H "X-LO-Project: ${LO_PROJECT}"

curl -fsS -G "${LO_URL}/api/traces/search" \
  -H "Authorization: Bearer ${LO_TOKEN}" \
  -H "X-LO-Project: ${LO_PROJECT}" \
  --data-urlencode 'limit=5'
```

### Troubleshooting table

| HTTP status or symptom | Cause | Fix |
| --- | --- | --- |
| `403` with `code: ingest_only_token` | The request used a project ingest token (`lo_...`). | Generate a `lou_...` user API token in Settings under Personal API tokens. |
| `403` with `code: upload_only_token` | The request used a Source Map upload token (`los_...`). | Switch to a `lou_...` user API token. |
| `401 unauthorized` | The `Authorization` header is missing, the token was revoked, or the value has a typo. | Verify the `Bearer ` prefix and check that the token is active. |
| `404` on `/select/logsql/...` or `/api/services` | The plugin connects straight to the gateway without the path-rewriting proxy. | Deploy the reverse proxy in Section 3 to map paths to `/api/v1/logs/*` and `/api/traces/*`. |
| `404` on extra probe requests during Grafana connection tests | Grafana probed unregistered routes such as `/api/v1/status/buildinfo`, `/api/v1/metadata`, or `/api/v1/rules`. | This behavior is expected. Run a query such as `up` in Explore to verify connectivity. |
| Target project data is missing | `X-LO-Project` was omitted, so the gateway queried `default`, or the project slug is misspelled. | Match `X-LO-Project` to the project slug shown in the web UI. |
| External VictoriaTraces misses traces from the last 30 seconds | External VictoriaTraces defaults to `-search.latencyOffset=30s` and `-insert.indexFlushInterval=20s`. | Lower both flags on external VictoriaTraces as described in [deployment.md](deployment.md#standalone). |
