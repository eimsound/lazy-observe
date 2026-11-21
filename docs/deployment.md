# Deployment

lazy-observe ships all-in-one release archives, a Linux container image,
[Docker Compose](../deploy/compose/README.md) and
[systemd](../deploy/systemd/README.md) reference manifests, and a Helm chart
for single-replica embedded or multi-replica standalone deployment. For
day-two backup, restore, upgrade/rollback, and secret-rotation runbooks, see
[Operations](operations.md); for connecting external dashboards, see
[Grafana and Perses](grafana-perses.md).

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

Only port 3100 is public. Persist `/data`; the image runs as a non-root user
(`65532:65532`). For hardened non-Kubernetes stacks, see the
[Docker Compose reference](../deploy/compose/README.md) (all-in-one and
external-storage profiles) and the
[systemd service unit](../deploy/systemd/README.md).

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
and must not be exposed. The first start that creates the `admin` account writes
a generated password to `<data_dir>/initial-admin-password` (mode 0600) and logs
only that path; the first sign-in must replace it before anything else works.
The same startup logs the default project's ingest API token once. The generated
password is never logged, and neither secret is printed again after bootstrap.

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
| `storage.metrics.kind` | `vm` | `vm` (supported) or `prometheus` (generic Prometheus-compatible API, not officially supported); controls query isolation strategy |
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

### First admin password

The `admin` account is created while no user named `admin` exists, that is, on
the first start against an empty metadata database (the `default` project is
created at the same point if it is missing). A deleted `admin` user is therefore
recreated with a new first password.

- `security.admin_password_file` (environment name `LO_SECURITY_ADMIN_PASSWORD_FILE`)
  names a file holding the password to use. Surrounding whitespace and a trailing
  newline are ignored, and at least 8 characters are required. A password supplied
  this way counts as chosen by the operator and is not forced to change.
- Without that field the server generates a random password, writes it to
  `<data_dir>/initial-admin-password` with mode 0600 before the account exists,
  and logs the absolute path instead of the password. The generated password must
  be replaced at the first sign-in: a user flagged `must_change_password` can only
  read `/api/auth/me`, change the password, or log out, and every other route
  answers `403 password_change_required`.

Multi-replica deployments must set `security.admin_password_file` (or the Helm
`adminPassword` Secret): the generated file exists only on the replica that won
the bootstrap lock, and its `data_dir` is usually an emptyDir. A later start that
finds an existing `admin` account writes no password file and keeps the current
password.

To replace the password of an existing account without knowing the current one,
run the CLI with the server's configuration and metadata database:

```bash
lazy-observe admin reset-password --config /etc/lazy-observe/config.yaml
lazy-observe admin reset-password --username operator --password-file /run/secrets/operator-password
```

The target defaults to `admin`; `--username` selects another account. Only the
`admin` account can use a generated password, and only while
`security.admin_password_file` is unset. A generated password is written to
`<data_dir>/initial-admin-password` with mode 0600, only the path is printed, and
the account is flagged `must_change_password` — the same semantics as a
generated first password. When that file already exists the command prints
`replacing existing <path>` before overwriting it, without showing its contents.
Every other account, and every deployment that sets
`security.admin_password_file` (which includes all HA deployments), must pass
`--password-file`; the command refuses to generate a password instead of
touching a deployer-owned file. A file passed with `--password-file` follows the
`security.admin_password_file` rules (surrounding whitespace ignored, at least 8
characters) and does not force a change. The command talks to the metadata
database directly, revokes every session of the account (with `redis.enabled`
it uses the same Redis as the replicas, so pass `redis.*` too), and exits
non-zero when the account does not exist. It also writes an
`auth.password_reset` audit event with actor `cli`.

With `redis.enabled` the command also clears the Redis-backed login lockout of
that account, so the new password works after repeated failed sign-ins. A
single-process deployment keeps the lockout counter in the gateway's memory,
which the CLI cannot reach: restart the gateway if that account is locked out.

Once the admin replaces a generated password through the UI or
`POST /api/auth/password`, the gateway deletes `<data_dir>/initial-admin-password`.
Only a real removal failure is logged as a warning; an already absent file is
silent so repeated password changes stay quiet, and neither case fails the
change. A file named by `security.admin_password_file` is never deleted, so the
shared password of an HA deployment survives an admin password change.

### Platform encryption key

`security.secret_key_file` (environment name `LO_SECURITY_SECRET_KEY_FILE`)
names the file holding the platform's 256-bit AES-256-GCM key; the variable
carries a path, never the key material. It seals sensitive metadata values: the
AI provider API key entered in **Settings > AI** is stored encrypted and never
returned to the browser. It also derives the built-in self-monitoring project
token instead of storing that credential recoverably.

The file holds 32 random bytes as standard base64, padded or not; surrounding
whitespace and line breaks are ignored, so `openssl rand -base64 32` output can
be provisioned as-is. The bytes must come from a cryptographically secure
random source, never from a passphrase or a KDF over one: a human-chosen key
can be attacked with a dictionary, which would recover both the key behind the
recorded fingerprint and every sealed value. Empty uses `<data_dir>/secret.key`:
the first start generates a fresh key and writes it with mode 0600, an existing
file is only read and never rewritten, and the key is held in memory for the
process lifetime.

Every replica that shares a metadata database must read the same key, and the
server now enforces it. The first start records a fingerprint of the key — 128
bits of HMAC-SHA256 over a fixed label, never key material — in the
`platform_key_fingerprints` row of the metadata database and logs the key file
path with that fingerprint. Each later started replica computes the fingerprint
of the key it loaded and compares it with the recorded one: a match starts
normally, a mismatch aborts the start before the gateway, storage, or bootstrap
runs, exits non-zero, and prints the key file path, both fingerprints, the
likely cause, and the recovery step. Concurrent first starts on an empty
database record exactly one fingerprint: the insert is conditional, so every
other replica reads that value and compares against it instead of overwriting
it. This turns the previously silent failure — a regenerated Secret or a
replica with its own key disabling AI and re-deriving the self-monitoring token
— into a refused start.

The fingerprint is a truncated one-way digest, so it never exposes the key; it
is safe to include in tickets and logs. Keep in mind that anybody holding a
database backup can still check a complete key candidate against it, which is
why the key itself must stay 32 random bytes.

The Helm chart therefore mounts one Secret read-only at
`/etc/lazy-observe-key/secret.key` — `encryptionKey.existingSecret`, or the
chart-managed `<fullname>-encryption-key` when `encryptionKey.autoGenerate` is
true. A `standalone-ha` release with neither refuses to render, at any replica
count including one: its `/data` is an emptyDir while the metadata database is
external, so a key generated in the pod would disappear when the pod is
recreated and the replacement pod would be refused for the fingerprint
mismatch — with no way back, because the original key is gone with the old pod.
Set `encryptionKey.existingSecret`, or leave `encryptionKey.autoGenerate=true`.
The `allinone` profile with persistence keeps the key inside the PVC's
`data_dir`; `allinone` without a PVC keeps the key in the same emptyDir as its
metadata database, so it loses both together and cannot hit this mismatch.

A mismatch means the configured key differs from the key this deployment
started with. Recovery has two paths:

1. **Restore the original key** (normal case). Put the previous key file back —
   for GitOps, the Secret content that the deployment used before — and
   restart. Nothing has been re-encrypted, so every sealed value is readable
   again.
2. **Confirm an intentional rotation.** Only when the old key is genuinely gone
   and you accept losing the sealed values, record the current key explicitly.
   Rotation is a whole-deployment operation: a server only checks the
   fingerprint at startup, so stop every replica first (scale to zero in
   Kubernetes). A replica that stays up keeps sealing with the key it loaded
   and writes values no other replica can read after the rotation.

   ```sh
   lazy-observe admin reset-key-fingerprint --config /etc/lazy-observe/config.yaml
   ```

   The command requires the key file to exist — it never creates or rewrites
   one, so a mistyped path or an unmounted Secret fails instead of recording a
   fingerprint no replica can match. It replaces the recorded fingerprint,
   prints the previous and the new fingerprint, and records a
   `security.key_fingerprint_reset` audit event with actor `cli`. Running it
   again with the same key prints `already recorded` and changes nothing.
   Nothing rotates automatically: the record only changes when an operator runs
   this command. Start the replicas again with the new key afterwards.

   In Kubernetes, run it in a Job that mounts the same configuration ConfigMap
   and the same encryption key Secret as the Deployment. Use the same image and
   tag as the Deployment — the rotation has to run the binary that reads the
   same configuration layout:

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

   `kubectl run` with the same `--env` and `--overrides` volume mounts works
   too; the command has to reach the metadata database and read the key file.

A rotation is not a transparent re-encryption — nothing re-seals existing
ciphertext, so values sealed with the previous key stay unreadable. In practice
the AI provider API key has to be entered again in **Settings > AI**, which
re-seals it under the current key. A different key also derives a different
self-monitoring token; the derived token is re-created at the next start. The
previous derived credential is not revoked: it stays in metadata and remains a
valid ingest credential for the self-monitoring project, so revoke it from that
project's token list if it must stop working.

Back up the key with the same care as the database: a lost key makes the sealed
values unrecoverable. This release adds the `platform_key_fingerprints` table
to the metadata baseline, so a metadata database (SQLite file or PostgreSQL
database) created by an earlier commit refuses to start with
`metadata migration 1 checksum mismatch`; the baseline marker is validated
before the fingerprint check ever runs. Recreate the database for that build
(a development database per
[ADR 0003](../.agents/docs/adr/0003-pre-release-single-database-baseline.md))
while keeping the same key file: the first start then records the current
fingerprint, which is exactly the key the database is sealed with.

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
    kind: vm
    query_url: "http://victoria-metrics:8428"
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
are disabled. External signal storage must be the Victoria family
(VictoriaMetrics, VictoriaLogs, VictoriaTraces); Loki and Tempo are not
supported by design ([ADR 0013](../.agents/docs/adr/0013-signal-storage-is-victoria-only.md)).
`kind: prometheus` still routes queries to a generic Prometheus-compatible API,
but it is not officially supported or covered by release verification.

The supervisor never starts, signals, or restarts the configured external
metrics, logs, or traces services. If lazy-observe starts the optional vmagent,
that process remains managed and is automatically restarted.

Because the supervisor does not start external services, their query latency is
your configuration. The embedded storage runs VictoriaMetrics and VictoriaTraces
with `-search.latencyOffset=0s`, so fresh data is queryable immediately.

VictoriaTraces hides spans newer than `-search.latencyOffset` (default 30 seconds)
from its Jaeger and Tempo search APIs, and from v0.12 on also from LogsQL queries.
The gateway sends `disable_latency_offset=true` with every LogsQL request it makes
to VictoriaTraces (the LLM call statistics). Single-node VictoriaTraces and `vtselect`
v0.12 and later honor it, so an external deployment needs no flag for the LLM
statistics to be current. Before v0.12 LogsQL applies no offset, and those versions
read only the query arguments they know, so the extra one is ignored (checked in the
upstream source, not against old binaries).

The Jaeger API has no per-request switch. On an external VictoriaTraces the trace
list leaves out spans newer than `-search.latencyOffset`, and a trace's detail view
(also used by Issue AI analysis) returns not found until the trace-ID index is
flushed, which takes up to `-insert.indexFlushInterval` (default 20 seconds, set on
the single node or `vtinsert`). Lower both flags on the external service if the trace
views must show fresh traces. The embedded VictoriaTraces runs with
`-search.latencyOffset=0s` and `-insert.indexFlushInterval=1s` for this reason.

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

## Query API, MCP, and Grafana

Every read is authenticated as a user, never as a project. A project API token
(`lo_...`) can only send telemetry: any read route that receives one answers
`403` with `code: ingest_only_token`, because SDK DSNs are embedded in public
browser pages (ADR 0010). This includes `/mcp` and `/api/system/info`.

A read uses one of two credentials:

- the browser session cookie, or
- a user API token (`lou_...`) as `Authorization: Bearer ...`, with the project
  selected by `X-LO-Project: <slug>`. Without that header the gateway serves the
  `default` project.

Create a user API token in **Settings > Personal API tokens**; the value is
shown once. The HTTP equivalent is `POST /api/auth/tokens` with a session
cookie and the `X-CSRF-Token` double-submit header. A user API token keeps the
issuing user's current role, so an admin's token can reach admin-only routes.

### Prometheus-compatible queries

| Endpoint | Purpose |
| --- | --- |
| `GET`/`POST /api/v1/query` | Instant PromQL/MetricsQL query |
| `GET`/`POST /api/v1/query_range` | Range query |
| `GET`/`POST /api/v1/series` | Series metadata |
| `GET`/`POST /api/v1/labels` | Label names |
| `GET`/`POST /api/v1/label/<name>/values` | Label values |

The gateway adds the authenticated project's `project_id` filter to every one
of these queries and drops client-supplied isolation parameters, so one project
cannot read another project's metrics. Writes stay on the ingest side:
`/api/v1/write` is Prometheus remote write and keeps using a project token.

```sh
curl -fsS -G http://localhost:3100/api/v1/query \
  -H 'Authorization: Bearer lou_...' \
  -H 'X-LO-Project: default' \
  --data-urlencode 'query=up'
```

LogsQL, trace, Replay, Issue, alert, and performance reads live under `/api`
(such as `/api/v1/logs/query`, `/api/traces/search`, `/api/v1/issues`) and use
the same credential pair; they are not part of the Prometheus data source surface.

### Grafana

Add a Prometheus data source whose URL is the gateway root, for example
`http://lazy-observe:3100`; the query endpoints sit directly under it, with no
path prefix. Grafana's Prometheus data source has no bearer-token field, so send
both headers as custom HTTP headers:

- `Authorization: Bearer lou_...`
- `X-LO-Project: <slug>`

When provisioning, Grafana stores custom header names under
`jsonData.httpHeaderNameN` and their values under `secureJsonData.httpHeaderValueN`
(a Grafana-side convention documented in
[Grafana provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/)),
which keeps the token out of the provisioned file's plain text.
The `X-LO-Project` value is fixed per data source, so one data source serves one
project. Grafana features that call other Prometheus endpoints — metadata,
exemplars, and rules — have no registered gateway route and answer `404`; the
endpoints in the table above are the supported surface.

For ready-to-use Grafana and Perses provisioning examples covering metrics,
logs, and traces (including the path-rewrite proxy for LogsQL and Jaeger
endpoints and the [ADR 0017](../.agents/docs/adr/0017-no-built-in-dashboards.md)
dashboard boundary), see [Grafana and Perses](grafana-perses.md),
[`deploy/integrations/grafana/datasources.yaml`](../deploy/integrations/grafana/datasources.yaml),
and
[`deploy/integrations/perses/datasources.yaml`](../deploy/integrations/perses/datasources.yaml).

### MCP

`lazy-observe mcp` runs a project-scoped stdio proxy for MCP clients:

```sh
lazy-observe mcp --dsn 'http://localhost:3100/p/default@lou_...'
```

The DSN keeps the SDK shape (`/p/<slug>@<token>`) with two differences: `<token>`
must be a user API token, which the command rejects at startup instead of
failing on the first tool call, and `<slug>` is sent as the `X-LO-Project`
header. The check is a prefix check only, so a revoked `lou_...` token still
starts the proxy and fails later. `/mcp` itself refuses a `lo_...` token with
`403 ingest_only_token`. The exposed tools are read-only and include bounded
Replay metadata; raw Replay events and URL queries are not exposed.

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

### Pinning the gateway image by digest

`image.digest` takes a registry digest: `sha256:` plus 64 lowercase hex
characters. When it is set the chart renders `<repository>:<tag>@<digest>`, or
`<repository>@<digest>` when `image.tag` is empty. The digest, not the tag,
selects the bytes that run; a malformed digest fails rendering instead of
silently downgrading the Deployment to a mutable tag:

```sh
helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe \
  --set secret.existingSecret=lazy-observe \
  --set image.tag=0.1.0 \
  --set image.digest=sha256:...
```

Do not copy that digest by hand when you can avoid it. Every server release
attaches `helm-values-lazy-observe-<version>.yaml` to its GitHub Release; the
file is rendered from the digest `docker/build-push-action` returned after
pushing the multi-architecture image to `ghcr.io/eimsound/lazy-observe`, so it
pins exactly the bytes that release built:

```sh
gh release download v0.1.0 --pattern 'helm-values-lazy-observe-*.yaml'
helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe \
  -f deploy/helm/lazy-observe/values-standalone-ha-cluster.yaml \
  -f helm-values-lazy-observe-0.1.0.yaml
```

Always write `image.tag` as a quoted string (`tag: "0.1"`). Unquoted, YAML reads
`0.1` as a number and a trailing zero disappears (`0.10` becomes `0.1`); the
chart stringifies numeric tags so they still render, but the version in the file
stops matching the tag you meant.

The chart is neither packaged nor published as a tgz or OCI artifact; it is
consumed from the repository checkout under `deploy/helm/lazy-observe`. That is
why the release flow never writes the digest back into the checked-in
`values.yaml`: a pinned digest in the repository would always lag one release
behind, and backfilling it would need a commit on `main` from a publishing run.

`standalone-ha` needs a platform encryption key Secret —
`encryptionKey.existingSecret`, or the chart-generated one with the default
`encryptionKey.autoGenerate=true`. The chart refuses to render a
`standalone-ha` release without one at any replica count, because the key must
outlive the pods; `allinone` with a PVC keeps it in `data_dir` instead. See
[Platform encryption key](#platform-encryption-key).

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
```

Pull request CI runs both Helm tasks in the `go` job, which starts whenever the
diff touches the server, chart, or script surface, so they do not have to be a
local prerequisite (`.github/workflows/ci.yml` selects jobs from the change
surface computed by `scripts/ci-changes.sh`).

The multi-replica HA gate is a separate job (`e2e-ha`) gated on the `ha` flag of
that same classifier. The flag follows server code (`cmd/**`, `internal/**`,
`go.mod`, `go.sum`, `sdk/go/**`), the HA gate script and the helpers it compiles,
`scripts/download-storage-bins.sh`, and `bench/performance-gates.json`;
`scripts/ci-changes.sh` is the authoritative list, and it also covers the Ent
generation chain (`entc.*`, `tools/entgen/**`, `scripts/generate-ent.sh`) and
full-run paths such as `.github/**`, `mise.toml`, `scripts/ci-*`, and
`Dockerfile*`. A chart-only change under `deploy/**` does not set it, so run
`mise run e2e-ha` locally when you change the chart and care about multi-replica
behavior. The gate is not a mock: it exercises a replicated VictoriaMetrics
cluster (including a vmstorage loss), split VictoriaLogs/VictoriaTraces roles
with bidirectional tenant isolation, an Apache-2.0 SeaweedFS S3 endpoint for
cross-replica Replay access, shared PostgreSQL alert state and notification
Outbox, Redis sessions and rate limits, fenced alert ownership, readiness, and
drain. Docker is required; missing Docker prerequisites return `77`/`SKIP`,
which CI treats as a failure and which is never a pass.

The principal values surfaces are `mode`, `replicaCount`,
`image.{repository,tag,digest,pullPolicy}`,
`external.{postgres,redis,s3,metrics,logs,traces}`, `secret.*`, `resources`,
`hpa.*`, `podDisruptionBudget.*`, `persistence.*`, `ingress.*`, `process.*`,
`encryptionKey.*` (the shared platform key behind `security.secret_key_file`),
and `adminPassword.*` (the first admin password behind
`security.admin_password_file`).

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

Before the first supported release, SQLite and PostgreSQL each have one mutable
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

> See [Operations](operations.md) for the complete day-two runbook covering
> All-in-One and HA hot/cold backups, restore verification with
> `lazy-observe doctor --ha`, upgrades/rollbacks, key rotation
> (`admin reset-key-fingerprint`), and password recovery
> (`admin reset-password`).

v0.x releases are public previews. Upgrading may require recreating the
database, and protocols and SDKs may change incompatibly between versions. The
database baseline stays mutable until the first supported release, which is
not v0.1.0 and has no date yet.

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
amd64/arm64 container image to `ghcr.io/eimsound/lazy-observe`. After that push
it also attaches `helm-values-lazy-observe-<version>.yaml` — the digest of the
image it just published, rendered by `scripts/render-helm-release-values.sh` —
to the same Release, so a Helm deployment can pin the released image without
hand-copying a digest. Before archives
are built, that workflow requires the dual-architecture container smoke
(including the Trivy scans of the image and of the embedded Victoria binaries)
and the full performance gates (`bench`, `perf-release`, `perf-ha`,
`perf-results-check`) to pass; the same workflow can be dispatched manually to
rerun those gates without publishing. Pull request CI does not run the
performance gates; they are noisy on shared runners and run only on manual
dispatch (`bench.yml`) and before a release.

Pull request CI is incremental: a `changes` job classifies the diff and only the
jobs whose surface it touches run. `ci-ok` aggregates every job — skipped counts
as passed — and is the only required check in the `main` ruleset. Pure documentation changes (`*.md`, `docs/**`,
`.agents/**`, `LICENSE`) run no business job, while server changes additionally
run the `e2e-ha` functional HA gate and `internal/**`/`cmd/**` changes run the
single-replica `e2e` and `e2e-restart` jobs. The detector's full-run fallbacks are tested
by `mise run ci-changes-test`.

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
