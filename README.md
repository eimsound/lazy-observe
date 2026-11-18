# lazy-observe

[简体中文](README.zh-CN.md)

lazy-observe is a self-hosted observability platform that runs as a single
binary. It collects metrics, logs, traces, errors, browser sessions, and
profiles from your applications and shows them in one web UI. Each project's
data is kept separate from every other project.

The binary contains the web UI and four storage programs: VictoriaMetrics,
VictoriaLogs, VictoriaTraces, and vmagent. On first start, lazy-observe extracts
them, runs them as child processes that listen only on localhost, and restarts
any that exit. Metadata is stored in SQLite by default, so you do not need to
set up a database to try it.

lazy-observe has no published release yet. To try it, build it from source as
shown in the quick start. It runs on Linux, on amd64 and arm64.

## Features

- Metrics, logs, and traces. Applications send data over OTLP/HTTP or
  Prometheus remote write. You query metrics with PromQL or MetricsQL and logs
  with LogsQL.
- Error tracking. Exceptions are grouped into issues by stack trace. lazy-observe
  detects regressions and uses uploaded source maps to show original source
  locations for browser errors.
- Browser monitoring. The browser SDK records Web Vitals and session replays and
  links each browser request to its backend trace.
- Profiling. Flame graphs for Go CPU, heap, and goroutine profiles and for Node
  CPU and heap profiles.
- Alerts. Rules are written in PromQL or LogsQL. Notifications go to webhooks,
  Feishu, DingTalk, WeCom, or Slack. Grouping, deduplication, silences,
  inhibition, and per-channel rate limits reduce repeated notifications.
- AI analysis. When an admin configures an LLM provider, lazy-observe analyzes
  each new issue with its related logs, traces, and release. It also reports
  token usage and cost for the LLM calls your own applications make.
- MCP. AI assistants can read project data through a read-only MCP endpoint.
  MCP reads authenticate as a user with a `lou_...` user API token and the
  `X-LO-Project` header; a `lo_...` project API token cannot read.
- Infrastructure. vmagent scrapes Prometheus exporters. For dashboards, connect
  Grafana or Perses to the Prometheus-compatible query API with a user API token
  and the `X-LO-Project` header.

## Quick start

You need mise 2025.8.4 or newer, `curl`, `tar`, and `sha256sum` or another
SHA-256 tool. mise installs the Go, Node.js, and pnpm versions this repository
pins.

```sh
git clone https://github.com/eimsound/lazy-observe.git
cd lazy-observe
mise install
mise run dist
./bin/lazy-observe server
```

`mise run dist` builds the web UI, downloads and verifies the storage programs,
and embeds both into `bin/lazy-observe`.

On the first start the server writes a generated password for the `admin` user
to `<data_dir>/initial-admin-password` (mode 0600) and logs that path, and it
logs an ingest API token once for the default project. Read the file, then open
<http://localhost:3100> and sign in as `admin`; the first sign-in must replace
the generated password before anything else works. To choose the first password
yourself, set `security.admin_password_file` (see
[Deployment](docs/deployment.md)); such a password is kept as given.

### Send data from a Go application

Add the Go SDK to a Fiber application:

```go
lo := lazyobserve.Init(lazyobserve.Config{DSN: "http://127.0.0.1:3100/p/default@TOKEN"})
app.Use(lo.Fiber())
slog.SetDefault(lo.Slog())
```

Replace `TOKEN` with the `lo_...` token that the server printed. That project
token can only send data: reading the same project in the UI, over MCP, or from
Grafana needs a user API token (`lou_...`), as described in
[Query API, MCP, and Grafana](docs/deployment.md#query-api-mcp-and-grafana).

To see data without writing code, run the example application in this
repository:

```sh
cd sdk/go
LO_DSN='http://127.0.0.1:3100/p/default@TOKEN' mise exec -- go run ./examples/fiber
```

In a second terminal, also in `sdk/go`, send one traced request and then 30
seconds of mixed fast, slow, and failing requests:

```sh
curl http://127.0.0.1:3320/trace/order-42
mise exec -- go run ./examples/fiber load --duration 30s --concurrency 8
```

The example exports metrics and logs every second. The Performance page shows
throughput, latency percentiles, and error rate. The Traces page shows each
request. The example's routes are listed in
[`sdk/go/examples/fiber/README.md`](sdk/go/examples/fiber/README.md).

## SDKs

| Language | Package | Integrations | Guide |
| --- | --- | --- | --- |
| Go | `github.com/eimsound/lazy-observe/sdk/go` | Fiber v2, `slog`, `database/sql`, runtime metrics, CPU, heap, and goroutine profiles | [Go SDK](docs/sdk/go.md) |
| Node.js | `@lazy-observe/node` | Express, Pino, Winston, runtime metrics, errors, CPU and heap profiles | [Node SDK](docs/sdk/node.md) |
| Browser | `@lazy-observe/browser`, `@lazy-observe/react` | Errors, Web Vitals, fetch and XHR tracing, session replay, React error boundary and route tracking | [Browser SDK](docs/sdk/browser.md) |
| Browser builds | `@lazy-observe/bundler-plugin` | Source map upload from Vite and Webpack | [Source maps](docs/sdk/browser.md#source-maps) |
| Python | `lazyobserve` | Django, Django REST Framework, logging, database queries, LLM calls | [Python SDK](docs/sdk/python.md) |

The SDK source is in [`sdk/`](sdk/). You can install the Go SDK with `go get`.
The Node, browser, and Python packages are not on npm or PyPI yet. Each guide
shows how to install them from this repository.

The SDKs never patch modules on the server side. You add each integration in
your code, as in the Go example above. Which SDK versions work with which server
versions is listed in [docs/version-matrix.md](docs/version-matrix.md).

## How it runs

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

Port 3100 is the only port that accepts outside connections. The gateway adds
the project ID to every read and write it sends to storage, so one project
cannot query another project's data. If a storage program exits, the gateway
reports that component as not ready and restarts it with exponential backoff.
The other components keep working.

The default setup runs everything in one process group. Two other setups are
available:

- Standalone mode. The gateway connects to VictoriaMetrics, VictoriaLogs, and
  VictoriaTraces that you run yourself. Signal storage is always the Victoria
  family; Loki and Tempo are not supported by design (ADR 0013).
- High availability. Several gateway replicas share PostgreSQL, Redis, and
  S3-compatible storage. Exactly one replica at a time evaluates each alert
  rule, chosen through a lease in the database. The Helm chart includes a
  three-replica profile.

See [Deployment](docs/deployment.md) for both setups and
[Architecture](.agents/docs/architecture.md) for process and security details.

## Development

```sh
mise install
mise run dev
```

`mise run dev` starts the Vite dev server on port 5173, the gateway on port 3100,
and the storage programs. Air rebuilds and restarts the gateway when Go or SQL
files change. Open <http://localhost:5173>, which forwards `/api` to the
gateway. Self-monitoring is off in development. To turn it on, run
`LO_DEV_SELF_MONITORING=true mise run dev`.

See [CONTRIBUTING.md](CONTRIBUTING.md) for tests, checks, and pull request rules.

## Documentation

- [Architecture](.agents/docs/architecture.md)
- [Deployment](docs/deployment.md)
- [SDK and server compatibility](docs/version-matrix.md)
- [Architecture decision records](.agents/docs/adr/)
- [Contributing](CONTRIBUTING.md)
- [Changelog](CHANGELOG.md)

## License

lazy-observe is licensed under [MIT](LICENSE). Release archives and container
images also include the Apache-2.0 licenses of the bundled VictoriaMetrics
programs and the notices for third-party Go and JavaScript dependencies.
