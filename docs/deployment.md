# Deployment

lazy-observe ships all-in-one release archives, a Linux container image, and a
Helm chart for single-replica embedded or multi-replica standalone deployment.

## Install a release

Install the latest archive from GitHub Releases (set `PREFIX` to override
`/usr/local`):

```sh
curl -fsSL https://raw.githubusercontent.com/eimsound/lazy-observe/main/scripts/install.sh | sh
lazy-observe server
```

Install a specific archive release by setting `VERSION=v1.2.3` when running the
installation script. Package-manager distribution is not part of the planned
release scope.

Release archives contain one binary with the web UI and the four managed
executables embedded. On first start they are verified and extracted under the
configured data directory.

## Container

```sh
docker run --rm -p 3100:3100 -v lazy-observe-data:/data \
  ghcr.io/eimsound/lazy-observe:latest
```

Only port 3100 is public. Persist `/data`; the image runs as a non-root user.

## All-in-one

Supported build targets are Linux on amd64 and arm64. CGO must remain disabled.

```sh
mise run dist
./bin/lazy-observe server --data-dir ./data
```

`mise run dist` is the production-shaped local build: it embeds the current
platform's verified storage manifest and executables. Leave `storage_bin_dir`
empty (the default) so the gateway extracts those payloads under `data_dir/bin`.
The gateway listens on `:3100`. Storage children listen on fixed loopback ports
and must not be exposed. The first run logs a generated admin password and
default-project API token. Capture that output securely: they are not
printed again after bootstrap.

Each managed child has an isolated restart loop. Killing one storage child does
not change the gateway PID or stop the other signal stores; health and readiness
name that component as restarting until its `/health` probe succeeds. Restart
delays use equal jitter between half and all of the current exponential delay,
bounded by `process.restart_max`. Normal shutdown disables those loops before it
drains vmagent and then the storage writers.

Managed Metrics, Logs, and Traces flush in-memory data at a one-second interval;
Trace ID indexing uses the same interval. The same-directory restart gate waits
across this bounded persistence horizon before deliberately killing the full
process group.

For frontend development without rebuilding the embedded UI:

```sh
cd web
mise exec -- pnpm install --frozen-lockfile
mise exec -- pnpm dev
```

Vite serves <http://localhost:5173> and proxies `/api` to the gateway.

## Configuration

Copy [the example configuration](../config.example.yaml) and start with `--config`:

```sh
cp config.example.yaml config.yaml
./bin/lazy-observe server --config config.yaml
```

Precedence is command-line flags, then `LO_*` environment variables, then YAML, then defaults. Unknown YAML fields are rejected. Durations accept Go duration syntax such as `30s`, `4h`, plus whole-day values such as `14d`.

### Server and storage fields

| YAML field | Default | Meaning |
| --- | --- | --- |
| `listen` | `:3100` | Public gateway address |
| `data_dir` | `./data` | Metadata, local blobs, generated files, and embedded-storage root |
| `standalone` | `false` | Use external signal storage |
| `storage_bin_dir` | empty | Optional development override; empty uses verified embedded Victoria binaries |
| `meta.db_url` | empty | Empty uses `data_dir/meta/lazy-observe.db`; otherwise `sqlite://...`, `postgres://...`, or `postgresql://...` |
| `storage.metrics.kind` | `vm` | `vm` or `prometheus`; controls query isolation strategy |
| `storage.metrics.cluster` | `false` | Use VictoriaMetrics cluster vmselect/vminsert paths; requires standalone mode and `kind: vm` |
| `storage.metrics.tenant_account_id` | empty | Platform-level unsigned VictoriaMetrics account ID; required when cluster mode is enabled |
| `storage.metrics.query_url` | empty | Required in standalone; base for Prometheus-compatible queries |
| `storage.metrics.ingest_url` | empty | Optional standalone write base; `/api/v1/write` is remote-write 1.0, while OTLP/Web Vitals require VictoriaMetrics-compatible endpoints; enables vmagent |
| `storage.logs.url` | empty | Required standalone VictoriaLogs base URL |
| `storage.traces.url` | empty | Required standalone VictoriaTraces base URL |
| `redis.enabled` | `false` | Enable Redis-backed shared sessions and rate limits |
| `redis.addr` | `127.0.0.1:6379` | Redis address used when enabled |
| `redis.password` | empty | Optional Redis password; supply through a secret in production |
| `redis.db` | `0` | Redis logical database number |
| `redis.key_prefix` | `lazy-observe` | Namespace for keys shared by all gateway replicas |
| `self_monitoring.enabled` | `true` | Observe lazy-observe through its own Go SDK and built-in project |
| `self_monitoring.profiling` | `false` | Enable continuous profiling for the long-running gateway process |

### Retention, blob, alert, and process fields

| YAML field | Default | Meaning |
| --- | --- | --- |
| `retention.metrics` | `90d` | Embedded VictoriaMetrics retention |
| `retention.logs` | `14d` | Embedded VictoriaLogs retention |
| `retention.traces` | `7d` | Embedded VictoriaTraces retention |
| `retention.replay` | `3d` | Replay metadata/blob cleanup age |
| `retention.sourcemaps` | `90d` | Source-map cleanup age |
| `retention.alert_events` | `30d` | Alert-event cleanup age |
| `retention.profiles` | `7d` | Profile metadata/blob cleanup age |
| `retention.issues_closed` | `true` | Delete resolved/ignored Issues after 90 inactive days |
| `replay.cleanup_interval` | `1h` | Legacy replay cleaner interval field; platform retention janitor currently uses its own one-hour default |
| `blob.backend` | `local` | `local` or `s3` for replay, source-map, and profile bodies |
| `blob.s3.bucket` | empty | Required with S3 |
| `blob.s3.region` | `us-east-1` | AWS region |
| `blob.s3.endpoint` | empty | Optional S3-compatible base URL; enables path-style access |
| `blob.s3.access_key` | empty | Optional static credential; must be paired with secret key |
| `blob.s3.secret_key` | empty | Optional static credential; must be paired with access key |
| `alert.storm_threshold` | `10` | Alerts in a group before storm behavior |
| `alert.group_wait` | `30s` | Initial group collection delay |
| `alert.repeat_interval` | `4h` | Repeat-notification interval |
| `process.startup_timeout` | `20s` | Per-child health deadline |
| `process.shutdown_timeout` | `15s` | Supervised shutdown deadline |
| `process.restart_initial` | `1s` | Initial managed-child restart delay before equal jitter |
| `process.restart_max` | `1m` | Maximum managed-child restart delay |
| `process.drain_timeout` | `20s` | Gateway in-flight request and Issue-batch drain deadline |
| `process.component_port_offset` | `0` | Managed child port offset for local isolation tests; leave zero in normal deployments |

### Optional AI access layer

AI is disabled by default and is configured exclusively by administrators in **Settings > AI**. Provider settings and the API key are stored in metadata; the key is sealed with `security.secret_key_file` and is never returned to the browser. There is no `ai:` server YAML section or `LO_AI_*` environment interface.

When enabled, the gateway provides admin connectivity testing, natural-language MetricsQL/LogsQL generation and explanation, and asynchronous analysis for new Issues. Generated queries still execute through the project-isolated gateway with response/time limits. Issue analysis uses bounded cross-signal evidence and never delays error ingestion; prompt/completion content is not collected unless an SDK explicitly opts in.

The example file lists the canonical path-shaped environment names, such as `LO_STORAGE_METRICS_QUERY_URL` and `LO_RETENTION_REPLAY`. Short compatibility aliases are not accepted.

## Standalone

Standalone still runs the gateway, in-process alert engine, metadata database, blob store, and optionally vmagent. A minimal configuration is:

```yaml
standalone: true
storage:
  metrics:
    kind: prometheus
    query_url: "http://prometheus:9090"
    ingest_url: "http://victoria-metrics:8428"
  logs:
    url: "http://victoria-logs:9428"
  traces:
    url: "http://victoria-traces:10428"
```

Release archives already embed the verified `vmagent-prod` payload, so the
minimal standalone configuration leaves `storage_bin_dir` empty. If a
development/offline override is explicitly configured, that directory must
contain `vmagent-prod` when `ingest_url` is present. Without `ingest_url`, metric OTLP ingest and scrape management
are disabled. The query URL may be VictoriaMetrics or a Prometheus-compatible
API. Logs and traces currently require VictoriaLogs and VictoriaTraces
semantics; Loki and Tempo adapters are not implemented.

The supervisor never starts, signals, or restarts the configured external
metrics, logs, or traces services. If lazy-observe starts the optional vmagent,
that process remains managed and is automatically restarted.

The gateway uses one storage interface in both modes. In standalone mode the
configured endpoints are routed as follows:

| Setting | Gateway traffic |
| --- | --- |
| `storage.metrics.query_url` | Prometheus-compatible query and metadata APIs |
| `storage.metrics.ingest_url` | OTLP metrics, Prometheus remote-write, Web Vitals import, and optional vmagent remote-write |
| `storage.logs.url` | VictoriaLogs OTLP/JSON-line writes and LogsQL reads |
| `storage.traces.url` | VictoriaTraces OTLP writes and Jaeger-compatible reads |

`/api/v1/write` accepts Prometheus remote-write 1.0
(`prometheus.WriteRequest`) and rewrites the protobuf labels to enforce the
authenticated `project_id`, so it does not depend on the VictoriaMetrics-only
`extra_label` query parameter. The configured receiver must expose the standard
`/api/v1/write` path. `/v1/metrics` requires the ingest endpoint to support
VictoriaMetrics' `/opentelemetry/v1/metrics`; Web Vitals require its
`/api/v1/import/prometheus` endpoint. If `ingest_url` is omitted these write
features return `501` instead of falling back to the query endpoint. A separate
write endpoint is included in health/readiness checks.

VictoriaLogs and VictoriaTraces requests use their documented single-node
paths and the gateway replaces incoming `AccountID`/`ProjectID` headers with
the authenticated project tenant on both writes and reads.

External storage owns metrics/logs/traces retention in standalone mode. Platform retention still applies to replay, source maps, alert events, profiles, and closed Issues.

## Kubernetes with Helm

The chart is under `deploy/helm/lazy-observe`. Its default `standalone-ha` mode
runs two replicas and expects PostgreSQL, Redis, S3-compatible object storage,
VictoriaMetrics, VictoriaLogs, and VictoriaTraces to be reachable. Put
credentials in a Kubernetes Secret; do not commit them to a values file:

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

For the production-oriented three-replica cluster topology, start from the
checked-in profile and override its example endpoints and Secret references:

```sh
helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe \
  --set secret.existingSecret=lazy-observe \
  -f deploy/helm/lazy-observe/values-standalone-ha-cluster.yaml
```

That profile sends metrics queries to vmselect and writes to vminsert under one
platform account; the gateway supplies the second `project_id` boundary. Its
VictoriaLogs and VictoriaTraces URLs must be path-aware Services or vmauth
frontends that route `/insert/*` and `/select/*` to the corresponding split
roles while preserving `AccountID`/`ProjectID`. Split roles provide cluster
routing and sharding; the external operator must provision redundant
frontends, storage replication where supported, and persistent volumes.

For a single-pod installation with embedded storage, switch modes. The chart
forces one replica, uses a `Recreate` Deployment strategy, and mounts a PVC at
`/data`:

```sh
helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe --create-namespace \
  --set mode=allinone \
  --set persistence.size=50Gi
```

`/livez` reports process liveness without probing dependencies. `/readyz`
starts at 503, becomes 200 only after initialization/migrations and dependency
checks succeed, and returns 503 as soon as shutdown begins. `/healthz` remains
the human-readable aggregate component report. The lifecycle hook works with
the distroless image by invoking the binary's internal pre-stop helper: it
signals PID 1 and leaves a short endpoint-removal window. The default 45-second
termination grace covers the 20-second gateway drain plus the 15-second child
shutdown deadline and hook allowance.

The optional HPA (`hpa.enabled=true`) is rendered only for `standalone-ha` and
uses CPU utilization; CPU requests are set by default so the metric is usable.
The PDB is enabled by default with `minAvailable: 1`. Tune the HPA minimum and
PDB together so voluntary disruption cannot remove all ready replicas. Ingress
is opt-in under `ingress.*`.

[ADR 0001](../.agents/docs/adr/0001-nightingale-style-alert-engine.md) defines the implemented
single-owner alert engine. Every application replica hosts an evaluator
candidate, while rendezvous assignment and a PostgreSQL lease/fencing epoch
allow only one candidate to commit each rule's state and Outbox work. Gateway
replica count can therefore change ownership, but cannot create concurrent
notification authority. This requires one shared writable PostgreSQL.

Validate both modes before applying changes:

```sh
mise run helm-lint
mise run helm-template-test
mise run e2e-ha
```

`e2e-ha` is a required local Docker gate, not a mock: it exercises a
replicated VictoriaMetrics cluster (including a vmstorage loss), split
VictoriaLogs/VictoriaTraces roles with bidirectional tenant isolation, an
Apache-2.0 SeaweedFS S3 endpoint for cross-replica Replay access, shared
PostgreSQL alert state and notification Outbox, Redis sessions and rate limits,
fenced alert ownership, readiness, and drain.
Missing Docker prerequisites return `77`/`SKIP`, which is not a pass.

The principal values surfaces are `mode`, `replicaCount`, `image.*`,
`external.{postgres,redis,s3,metrics,logs,traces}`, `secret.*`, `resources`,
`hpa.*`, `podDisruptionBudget.*`, `persistence.*`, `ingress.*`, and `process.*`.

## PostgreSQL metadata

Set a PostgreSQL URL in YAML or `LO_META_DB_URL`:

```yaml
meta:
  db_url: "postgres://lazy_observe:password@postgres:5432/lazy_observe?sslmode=require"
  migration_lock_timeout: 60s
```

On an empty database, the server creates the complete schema from one embedded
PostgreSQL baseline. Use a dedicated database and credential. Running multiple lazy-observe application replicas
also requires Redis for shared sessions and rate limits. Issue batches are
process-local while pending but merge atomically in PostgreSQL during flush.

The startup path uses a PostgreSQL advisory lock, bounded waiting, post-lock
re-read, and a checksummed baseline marker. Lock timeout, connection loss,
unlock failure, checksum drift, or baseline creation failure aborts startup
before the instance becomes ready. This makes concurrent first startup safe;
it does not provide version-to-version database upgrades.

After migration coordination completes, a separate PostgreSQL advisory lock
serializes first-run admin, default-project, and self-monitoring bootstrap.
Consequently, multiple application replicas may start together on an empty
database without racing seed records or credentials.

The project is pre-release. SQLite and PostgreSQL each have one mutable
`001_baseline.sql`; after an incompatible schema change, recreate the
development database. Existing databases created by earlier commits are not a
supported upgrade source. See [ADR 0003](../.agents/docs/adr/0003-pre-release-single-database-baseline.md).

## S3-compatible blob storage

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

If static keys are empty, the AWS SDK default credential chain applies. Custom endpoints use path-style requests. The server validates only that a bucket exists in configuration and that static access/secret keys are paired; bucket reachability is established by actual operations.

## Retention

Retention is global by signal, not per project. In all-in-one mode the three VM retention values become child-process flags. A one-hour janitor removes expired platform-owned metadata and blobs. Closed-Issue retention is fixed at 90 days when enabled.

Reducing retention makes older data eligible for engine or janitor deletion; take a backup first if the data may be needed.

## Pre-release replacement and backup

For the default all-in-one/local deployment:

1. Stop the server cleanly and wait for all supervised children to exit.
2. Copy the entire configured `data_dir` as one consistent backup.
3. Replace the single `lazy-observe` release executable; it carries the web UI, storage manifest, and four pinned platform executables as one versioned unit.
4. If the new build changes the database baseline, recreate the development `data_dir`; otherwise start the gateway normally. It validates and atomically refreshes the extracted payloads under `data_dir/bin` and verifies the metadata baseline.
5. Check `/readyz` and `/healthz`, sign in, and verify signal queries before removing the backup.

Deployments that explicitly use the development `storage_bin_dir` override must
instead update that directory's four executables as a compatible set with the
gateway.

Do not copy live SQLite and storage directories as an assumed consistent backup. There is currently no supported in-place metadata upgrade or downgrade between development builds. Preserve an old data directory only together with the exact binary that created it.

External state must be backed up separately: use PostgreSQL-native backup for external metadata, S3 versioning/snapshots for external blobs, and each standalone signal backend's supported backup process. A `data_dir` copy alone is insufficient in those configurations.

For PostgreSQL, use a fresh development database after a baseline change. A
database-native backup remains useful for investigation, but the project does
not currently promise that a newer build can upgrade it. The first supported
release must introduce and document an append-only migration contract before
user upgrades are accepted.

The Helm startup probe must allow more time than the baseline-lock wait plus
the expected schema creation duration. Waiting and initializing instances stay unready;
startup probing keeps liveness checks disabled until initialization completes.

## Maintainer release workflow

The release and performance entry points are:

| Mise task | Gate |
| --- | --- |
| `mise run perf-release` | Build and size-check all four embedded platform binaries, then smoke-test native zero-configuration startup |
| `mise run perf-ha` | Enforce exact medium-corpus query P95 and capacity-controlled per-replica weak-scaling budgets against a validating sink, with real-storage mirrored readback |
| `mise run performance-gates` | Run all required performance tasks sequentially |

`mise run snapshot` runs `perf-release` before creating local archives without
publishing. `mise run release` likewise runs migration validation and
`perf-release` before GoReleaser, and only proceeds when HEAD has an exact `v*`
tag. The embedded-payload and zero-configuration smoke gate is therefore a
mandatory precondition of either packaging path, not a separate optional
check. Pushing a `v*` tag runs `.github/workflows/release.yml`, which creates
Linux amd64/arm64 archives and checksums as a GitHub Release, and pushes the
amd64/arm64 container image to `ghcr.io/eimsound/lazy-observe`. Before archives
are built, that workflow requires the amd64/arm64 container smoke and the full
performance gates (`bench`, `perf-release`, `perf-ha`, `perf-results-check`) to
pass. Pull request CI does not run the performance gates; they are noisy on
shared runners and run only on manual dispatch (`bench.yml`) and before a
release.

All workflows run on GitHub-hosted runners (`ubuntu-24.04` and the native
`ubuntu-24.04-arm`). The public repository deliberately registers no
self-hosted runner, because a fork pull request could otherwise execute code on
maintainer hardware. Release publication uses the workflow's built-in
`GITHUB_TOKEN` with `contents: write` and `packages: write`; no personal access
token is required. Client SDKs are released from the same repository with
prefixed tags: `sdk/go/vX.Y.Z` (the Go nested-module convention),
`sdk-node-vX.Y.Z`, `sdk-browser-vX.Y.Z`, and `sdk-python-vX.Y.Z`.

## Network and security notes

- Expose only the gateway (which serves the embedded UI); do not expose loopback child ports.
- Set both `security.tls_cert` and `security.tls_key` (or `--tls-cert` and `--tls-key`) for native TLS 1.2+ termination. A trusted reverse proxy remains the recommended Kubernetes edge and may terminate TLS instead.
- Treat project and user API tokens as secrets. Project-token DSNs are configuration values and must not be logged or committed.
- Use separate lazy-observe instances for unrelated security tenants.
- Use `standalone-ha` plus shared PostgreSQL, Redis, S3, and signal backends for multi-replica Kubernetes deployments.
