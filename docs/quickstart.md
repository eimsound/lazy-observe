# Five-minute quickstart

No tagged release (`v0.1.0`) has been published yet, so start either by building the single binary from source or by building the local Docker image and running it with Docker Compose. Once `v0.1.0` is released, prebuilt static archives on GitHub Releases, `scripts/install.sh`, and signed multi-arch images on `ghcr.io/eimsound/lazy-observe` will run directly without a local build.

## 1. Start lazy-observe

### Build a single binary from source

Supported targets are Linux on `amd64` and `arm64`. You need `mise` `>= 2026.10.13`, `curl`, `tar`, and `sha256sum` or `shasum`. `mise` installs the pinned Go, Node.js, and pnpm versions automatically.

```sh
git clone https://github.com/eimsound/lazy-observe.git
cd lazy-observe
mise install
mise run dist
./bin/lazy-observe server
```

`mise run dist` builds the web UI, downloads and SHA-256-verifies the four Victoria storage binaries, and embeds both into `bin/lazy-observe`.

### Run with Docker Compose

Build the local image and copy the example environment file [`deploy/compose/.env.example`](https://github.com/eimsound/lazy-observe/blob/main/deploy/compose/.env.example) using POSIX `sed`:

```sh
git clone https://github.com/eimsound/lazy-observe.git
cd lazy-observe
docker build -t lazy-observe:local .
cd deploy/compose
sed 's|^LO_IMAGE=.*|LO_IMAGE=lazy-observe:local|' .env.example > .env
docker compose up -d
```

See the [Docker Compose guide](https://github.com/eimsound/lazy-observe/blob/main/deploy/compose/README.md) for the `--profile standalone` external-storage stack with PostgreSQL, Valkey, SeaweedFS, and external Victoria services, and see the [systemd guide](https://github.com/eimsound/lazy-observe/blob/main/deploy/systemd/README.md) for Linux hosts.

## 2. Sign in and replace the initial admin password

On the first start against an empty `data_dir`, which defaults to `./data`, the server performs two bootstrap steps:

- It writes a randomly generated `admin` password to `<data_dir>/initial-admin-password` with mode `0600` and logs only the file path, never the password.
- It logs a one-time `lo_...` project ingest token for the `default` project.

Read the generated password:

```sh
# Local single binary:
cat ./data/initial-admin-password

# Docker Compose (the distroless image has no shell):
docker compose logs lazy-observe
docker run --rm --user 65532:65532 \
  -v compose_lazy-observe-data:/data:ro \
  debian:trixie-slim cat /data/initial-admin-password
```

Open <http://localhost:3100> and sign in as `admin`. You must replace the generated password on first sign-in before any other route works. After you change the password, the gateway deletes `initial-admin-password` automatically. To supply an operator-managed password without forced rotation, set `security.admin_password_file` (`LO_SECURITY_ADMIN_PASSWORD_FILE`); see [Deployment, First admin password](deployment.md#first-admin-password).

## 3. Get a project ingest token

Use the `lo_...` token printed for the `default` project on first startup, or open **Settings**, then **Projects** in the web UI to create a new project and ingest token.

- `lo_...` tokens are ingest-only. Any read route returns `403 ingest_only_token`.
- For Grafana, Perses, MCP, or Query API reads, create a Personal API token (`lou_...`) under **Settings**, then **Personal API tokens** and send it with `X-LO-Project: <slug>`. See [Deployment, Query API, MCP, and Grafana](deployment.md#query-api-mcp-and-grafana).

## 4. Send telemetry and inspect it in the web UI

### Send a quick error event with `curl`

Run a quick check by sending one JSON error event:

```sh
NOW_ISO=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EVENT_ID="quickstart-$(date -u +%Y%m%d%H%M%S)"
curl -fsS -X POST http://127.0.0.1:3100/api/events/errors \
  -H "Authorization: Bearer lo_<YOUR_PROJECT_TOKEN>" \
  -H "Content-Type: application/json" \
  -d "{\"events\":[{\"event_id\":\"${EVENT_ID}\",\"timestamp\":\"${NOW_ISO}\",\"platform\":\"go\",\"level\":\"error\",\"message\":\"hello from lazy-observe quickstart\",\"exception\":{\"type\":\"QuickstartError\",\"value\":\"hello from lazy-observe quickstart\"}}]}"
```

The endpoint returns `{"accepted":1}`, and the event appears on the **Issues** page. Any standard OpenTelemetry SDK can also export directly to `http://127.0.0.1:3100` over OTLP/HTTP using `http/protobuf` (`application/x-protobuf`). `/v1/metrics` and `/v1/logs` accept Protobuf only, while `/v1/traces` accepts both Protobuf and JSON. When `LO_OTLP_GRPC_ENABLED=true`, OpenTelemetry SDKs can also export over OTLP/gRPC to `127.0.0.1:4317` with `Authorization: Bearer lo_<YOUR_PROJECT_TOKEN>`.

### Run the Go Fiber v3 example

To populate metrics, logs, traces, errors, and backend endpoint performance at once, run the bundled [Go Fiber v3 example](https://github.com/eimsound/lazy-observe/blob/main/sdk/go/examples/fiber/README.md):

```sh
cd sdk/go/examples/fiber
LO_DSN='http://127.0.0.1:3100/p/default@lo_<YOUR_PROJECT_TOKEN>' \
  mise exec -- go run .
```

In a second terminal, trigger a traced request, a panic, and 30 seconds of load:

```sh
cd sdk/go/examples/fiber
curl http://127.0.0.1:3320/trace/order-42
curl http://127.0.0.1:3320/panic
mise exec -- go run . load --url http://127.0.0.1:3320 --duration 30s --concurrency 8
```

### Inspect live data in the web UI

Open <http://localhost:3100> and inspect each workspace:

- **Logs.** Search `lazy-observe-fiber-example` or `"order trace complete"` in LogsQL, including logs linked to trace IDs.
- **Metrics.** Query `http.server.request.duration_count` or `http.server.request.duration_bucket` using the visual PromQL query builder, which supports dotted metric names natively, or switch to the CodeMirror editor.
- **Traces.** Open the `/trace/order-42` waterfall to inspect the HTTP server span and its SQL child span.
- **Issues.** Inspect the `QuickstartError` smoke event and the grouped `/panic` exception with correlated logs and trace context.
- **Performance.** Switch to the **Backend endpoints** tab to view route throughput, P50/P95/P99 latency, and error rate from the Fiber example. The default **Web Vitals** tab displays geography and device breakdowns once you connect the Browser SDK or post events to `POST /api/events/vitals`.

## Next steps

- Configure external storage, TLS, or Kubernetes HA in [Deployment](deployment.md).
- Instrument your own applications with the [Go](sdk/go.md), [Node.js](sdk/node.md), [Browser and React](sdk/browser.md), or [Python](sdk/python.md) SDK.
- Connect external dashboards in [Grafana and Perses](grafana-perses.md) and review backup procedures in [Operations](operations.md).
