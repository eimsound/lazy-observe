# Deployment

lazy-observe ships all-in-one release archives, a Linux container image, [Docker Compose](https://github.com/eimsound/lazy-observe/blob/main/deploy/compose/README.md) and [systemd](https://github.com/eimsound/lazy-observe/blob/main/deploy/systemd/README.md) reference manifests, and a Helm chart for single-replica embedded or multi-replica standalone clusters. See [Operations](operations.md) for backup, restore, upgrade, rollback, and secret-rotation runbooks. See [Grafana and Perses](grafana-perses.md) to connect external dashboards.

## Install a release

Install the latest archive from GitHub Releases. Set `PREFIX` to override `/usr/local`:

```sh
curl -fsSL https://raw.githubusercontent.com/eimsound/lazy-observe/main/scripts/install.sh | sh
lazy-observe server
```

Set `VERSION=v1.2.3` when running the installation script to install a specific release. Package-manager distribution is not planned.

Each release archive contains one binary with the web UI and the four managed executables embedded. On first start, the gateway verifies and extracts those executables under the configured data directory.

## Container

```sh
docker run --rm -p 3100:3100 -v lazy-observe-data:/data \
  ghcr.io/eimsound/lazy-observe:latest
```

Only the HTTP gateway port `3100` is public by default. The optional native OTLP/gRPC port `4317` stays disabled unless `otlp_grpc.enabled` is set to `true`. Persist `/data`. The image runs on distroless `nonroot` as UID/GID `65532:65532` with no shell. For hardened non-Kubernetes stacks, use the [Docker Compose reference](https://github.com/eimsound/lazy-observe/blob/main/deploy/compose/README.md) for all-in-one or external-storage profiles, or use the [systemd service unit](https://github.com/eimsound/lazy-observe/blob/main/deploy/systemd/README.md).

## All-in-one

Supported build targets are Linux on `amd64` and `arm64`, with `CGO_ENABLED=0`.

```sh
mise run dist
./bin/lazy-observe server --data-dir ./data
```

`mise run dist` builds the production-shaped local binary and embeds the current platform's verified storage manifest and executables. Leave `storage_bin_dir` empty so the gateway extracts those payloads under `data_dir/bin`.

The HTTP gateway listens on `:3100`. The optional OTLP/gRPC server on `:4317` defaults to `otlp_grpc.enabled: false`. Managed storage children listen on fixed loopback addresses and must never be exposed:

- VictoriaMetrics: `127.0.0.1:18428`
- VictoriaLogs: `127.0.0.1:19428`
- VictoriaTraces: `127.0.0.1:20428`
- `vmagent`: `127.0.0.1:22428`

When the first start creates the `admin` account, the server writes a generated password to `<data_dir>/initial-admin-password` with mode `0600` and logs only that path. The administrator must replace this password on the first sign-in before other routes work. The same startup logs the `default` project's ingest API token once. The server never logs the generated password, and neither secret prints again after bootstrap.

Each managed child runs in an isolated restart loop. Killing one storage child does not change the gateway PID or stop the other signal stores. Health and readiness endpoints report that component as restarting until its `/health` probe succeeds. Restart delays apply equal jitter between half and all of the current exponential backoff delay, capped at `process.restart_max`. Normal shutdown disables those restart loops before draining `vmagent` and then stopping the storage writers.

Managed VictoriaMetrics, VictoriaLogs, and VictoriaTraces flush in-memory data every 1 second. Trace ID indexing uses the same 1-second interval. The same-directory restart gate waits across this persistence window before terminating the process group.

Run the frontend dev server without rebuilding the embedded UI:

```sh
cd web
mise exec -- pnpm install --frozen-lockfile
mise exec -- pnpm dev
```

Vite serves <http://localhost:5173> and proxies `/api` to the gateway.

## Configuration

Copy [the example configuration](https://github.com/eimsound/lazy-observe/blob/main/config.example.yaml) and start the server with `--config`:

```sh
cp config.example.yaml config.yaml
./bin/lazy-observe server --config config.yaml
```

Configuration precedence runs from highest to lowest: command-line flags, `LO_*` environment variables, YAML file, and built-in defaults. The server rejects unknown YAML fields. Duration fields accept Go duration syntax such as `30s` and `4h`, plus whole-day values such as `14d`.

The example configuration lists canonical path-shaped environment variable names, such as `LO_STORAGE_METRICS_QUERY_URL` and `LO_RETENTION_REPLAY`. Short compatibility aliases are not accepted.

### Server and storage fields

| YAML field | Default | Meaning |
| --- | --- | --- |
| `listen` | `:3100` | Public HTTP gateway address |
| `otlp_grpc.enabled` | `false` | Enable native OTLP/gRPC ingest for traces, metrics, and logs |
| `otlp_grpc.listen` | `:4317` | Native OTLP/gRPC listen address when enabled |
| `otlp_grpc.max_recv_bytes` | `54525952` | Maximum decoded OTLP/gRPC message size in bytes (52 MiB) |
| `ingest_rate_limit.enabled` | `false` | Enable per-project token-bucket ingest rate limiting for HTTP and OTLP/gRPC |
| `ingest_rate_limit.requests_per_second` | `500` | Per-project ingest request rate limit |
| `ingest_rate_limit.burst` | `1000` | Per-project ingest burst size |
| `ingest_rate_limit.bytes_per_second` | `0` | Optional per-project byte throughput cap in bytes per second (`0` disables byte limiting) |
| `ingest_rate_limit.max_tracked_projects` | `4096` | Upper bound on per-project limiter buckets held in memory |
| `metrics_endpoint.enabled` | `false` | Expose Prometheus text exposition at `/metrics` for gateway operational metrics |
| `metrics_endpoint.bearer_token` | empty | Optional static `Authorization: Bearer <token>` credential for `/metrics` scrapers |
| `metrics_endpoint.allow_unauthenticated` | `false` | Allow unauthenticated `/metrics` scraping on private networks; forbidden when `bearer_token` is set |
| `data_dir` | `./data` | Root directory for metadata, local blobs, generated files, and extracted binaries |
| `serve_web_ui` | `true` | Serve the embedded web UI from the gateway |
| `external_url` | empty | Public console base URL (`http(s)://host[:port]`, no path, query, or fragment). When set, alert notifications include absolute console links; when empty, notifications carry no links |
| `topology` | empty | Explicit deployment topology: empty, `all-in-one`, `standalone`, or `ha`. Setting `ha` fails closed unless PostgreSQL, Redis, S3, a shared secret key file, and external signal endpoints are configured |
| `standalone` | `false` | Use external signal storage |
| `storage_bin_dir` | empty | Optional development or offline override; empty uses verified embedded Victoria binaries |
| `meta.db_url` | empty | Empty uses `data_dir/meta/lazy-observe.db`; otherwise `sqlite://...`, `postgres://...`, or `postgresql://...` |
| `meta.migration_lock_timeout` | `60s` | PostgreSQL migration advisory-lock wait limit |
| `storage.metrics.kind` | `vm` | `vm` (supported) or `prometheus` (generic Prometheus-compatible API, not officially supported); sets the query isolation strategy |
| `storage.metrics.cluster` | `false` | Use VictoriaMetrics cluster `vmselect`/`vminsert` paths; requires standalone mode and `kind: vm` |
| `storage.metrics.tenant_account_id` | empty | Platform-level unsigned 32-bit VictoriaMetrics account ID; required when cluster mode is enabled |
| `storage.metrics.query_url` | empty | Required in standalone mode; base URL for Prometheus-compatible queries or `vmselect` root |
| `storage.metrics.ingest_url` | empty | Optional standalone write base URL or `vminsert` root; `/api/v1/write` uses remote-write 1.0, while OTLP and Web Vitals require VictoriaMetrics endpoints; enables `vmagent` |
| `storage.logs.url` | empty | Required standalone VictoriaLogs base URL unless both split URLs are set |
| `storage.logs.query_url` | empty | Optional `vlselect` query override; falls back to `storage.logs.url` |
| `storage.logs.ingest_url` | empty | Optional `vlinsert` ingest override; falls back to `storage.logs.url` |
| `storage.traces.url` | empty | Required standalone VictoriaTraces base URL unless both split URLs are set |
| `storage.traces.query_url` | empty | Optional `vtselect` query override; falls back to `storage.traces.url` |
| `storage.traces.ingest_url` | empty | Optional `vtinsert` ingest override; falls back to `storage.traces.url` |
| `redis.enabled` | `false` | Enable Redis-backed shared sessions and rate limits |
| `redis.addr` | `127.0.0.1:6379` | Redis address used when enabled |
| `redis.password` | empty | Optional Redis password; supply through a secret in production |
| `redis.db` | `0` | Redis logical database number |
| `redis.key_prefix` | `lazy-observe` | Key prefix shared by all gateway replicas |
| `self_monitoring.enabled` | `true` | Observe lazy-observe through its own Go SDK and built-in project |
| `self_monitoring.web` | `true` | Enable session-authenticated browser self-monitoring for signed-in web UI users (errors, Web Vitals, and same-origin fetch tracing) when `self_monitoring.enabled` is `true` |
| `self_monitoring.web_replay` | `false` | Enable error-only Session Replay (`onErrorOnly`) for web self-monitoring when `self_monitoring.enabled` and `self_monitoring.web` are `true` |
| `self_monitoring.profiling` | `false` | Enable continuous profiling for the long-running gateway process |

> In multi-replica HA deployments (including during rolling upgrades), configure `self_monitoring.enabled`, `self_monitoring.web`, and `self_monitoring.web_replay` identically across every replica sharing the same metadata database. Starting any replica with `self_monitoring.web: false` (or `self_monitoring.enabled: false`) revokes the shared `self-monitoring-web` token that other running replicas serve to signed-in browsers.

### Retention, blob, alert, and process fields

| YAML field | Default | Meaning |
| --- | --- | --- |
| `retention.metrics` | `90d` | Embedded VictoriaMetrics retention |
| `retention.logs` | `14d` | Embedded VictoriaLogs retention |
| `retention.traces` | `7d` | Embedded VictoriaTraces retention |
| `retention.replay` | `3d` | Replay metadata and blob cleanup age |
| `retention.sourcemaps` | `90d` | Source-map cleanup age |
| `retention.alert_events` | `30d` | Alert-event cleanup age |
| `retention.profiles` | `7d` | Profile metadata and blob cleanup age |
| `retention.issues_closed` | `true` | Delete resolved or ignored Issues after 90 inactive days |
| `replay.cleanup_interval` | `1h` | Legacy replay cleaner interval field; the platform retention janitor uses its own 1-hour default |
| `blob.backend` | `local` | `local` or `s3` for replay, source-map, and profile bodies |
| `blob.s3.bucket` | empty | Required when `blob.backend` is `s3` |
| `blob.s3.region` | `us-east-1` | AWS region |
| `blob.s3.endpoint` | empty | Optional S3-compatible base URL; enables path-style access |
| `blob.s3.access_key` | empty | Optional static credential; must be paired with `blob.s3.secret_key` |
| `blob.s3.secret_key` | empty | Optional static credential; must be paired with `blob.s3.access_key` |
| `alert.storm_threshold` | `10` | Alerts in a group before storm handling triggers |
| `alert.group_wait` | `30s` | Initial group collection delay |
| `alert.repeat_interval` | `4h` | Repeat-notification interval |
| `scrape.mode` | `auto` | Embedded `vmagent` scraping mode: `auto` (active on single node, PostgreSQL leader-elected in HA) or `disabled` |
| `error_inbox.max_items` | `100000` | Durable error-inbox item capacity before rejecting with `503` and `Retry-After` |
| `error_inbox.max_bytes` | `1073741824` | Durable error-inbox byte capacity (1 GiB) before rejecting with `503` and `Retry-After` |
| `error_inbox.readiness_items` | `80000` | Error-inbox item threshold where readiness degrades |
| `error_inbox.readiness_bytes` | `838860800` | Error-inbox byte threshold (800 MiB) where readiness degrades |
| `error_inbox.receipt_retention` | empty (inherits `retention.logs`) | How long completed error receipts are kept to deduplicate at-least-once redelivery; must be positive when set. In standalone/HA mode, set it to the external VictoriaLogs retention |
| `process.startup_timeout` | `20s` | Per-child health deadline |
| `process.shutdown_timeout` | `15s` | Supervised shutdown deadline |
| `process.restart_initial` | `1s` | Initial managed-child restart delay before equal jitter |
| `process.restart_max` | `1m` | Maximum managed-child restart delay |
| `process.drain_timeout` | `20s` | Gateway in-flight request and Issue-batch drain deadline |
| `process.component_port_offset` | `0` | Managed child port offset for local isolation tests; leave `0` in normal deployments |
| `security.session_secure_cookie` | `false` | Mark session and CSRF cookies `Secure` when TLS terminates in front of the gateway |
| `security.tls_cert` | empty | Native TLS 1.2+ certificate path; pair with `security.tls_key` |
| `security.tls_key` | empty | Native TLS 1.2+ private key path; pair with `security.tls_cert` |
| `security.trusted_proxies` | `[]` | Reverse proxy IPs or CIDRs trusted for `X-Forwarded-For` client IP rate limiting |
| `security.secret_key_file` | empty | Platform AES-256-GCM key file path; empty uses `<data_dir>/secret.key` |
| `security.admin_password_file` | empty | Operator-managed initial `admin` password file path |
| `ai.pricing_file` | empty | Optional static LLM token pricing override file path |

### First admin password

The server creates the `admin` account whenever no user named `admin` exists, such as on the first start against an empty metadata database. It creates the `default` project at the same time if that project is missing. If you delete the `admin` user, the next startup recreates it with a new first password.

- `security.admin_password_file` (environment variable `LO_SECURITY_ADMIN_PASSWORD_FILE`) points to a file containing the initial password. The server ignores surrounding whitespace and trailing newlines and requires at least 8 characters. Passwords supplied through this file count as operator-chosen and do not force a password change.
- When `security.admin_password_file` is unset, the server generates a random password, writes it to `<data_dir>/initial-admin-password` with mode `0600` before creating the account, and logs the absolute path instead of the password. The administrator must replace the generated password at the first sign-in. A user flagged `must_change_password` can only call `/api/auth/me`, change their password, or log out; every other route returns `403 password_change_required`.

Multi-replica deployments must set `security.admin_password_file` or the Helm `adminPassword` Secret. Otherwise the generated file exists only on the replica that won the bootstrap lock, and in `standalone-ha`, pod `data_dir` volumes use `emptyDir`. A later startup that finds an existing `admin` account writes no password file and keeps the existing password.

Run the CLI directly against the server configuration and metadata database to replace an existing account's password without knowing the current one:

```bash
lazy-observe admin reset-password --config /etc/lazy-observe/config.yaml
lazy-observe admin reset-password --username operator --password-file /run/secrets/operator-password
```

The command targets `admin` by default. Pass `--username` to choose another account. Only the `admin` account can use a generated password, and only when `security.admin_password_file` is unset. In generated mode, the command writes the password to `<data_dir>/initial-admin-password` with mode `0600`, prints only the path, and sets `must_change_password`. If that file already exists, the command prints `replacing existing <path>` before overwriting it and never prints its contents.

Every other account, and every deployment that sets `security.admin_password_file` (including all HA deployments), must pass `--password-file`. The command refuses to generate a password rather than modify a deployer-owned file. A file passed to `--password-file` follows the same rules as `security.admin_password_file`. The CLI ignores surrounding whitespace, requires at least 8 characters, and does not force a password change. The command connects directly to the metadata database, revokes every session for that account, and exits non-zero if the account does not exist. When `redis.enabled` is true, pass the same `redis.*` configuration so the CLI revokes Redis sessions as well. It also records an `auth.password_reset` audit event with actor `cli`.

With `redis.enabled`, the command also clears the Redis-backed login lockout for that account so the new password works after repeated failed sign-ins. A single-process deployment holds the lockout counter in gateway memory, which the CLI cannot reach. Restart the gateway if that account is locked out.

When the administrator replaces a generated password through the UI or `POST /api/auth/password`, the gateway deletes `<data_dir>/initial-admin-password`. The gateway logs a warning only on an actual removal error. An already absent file produces no warning, and neither case fails the password change. The gateway never deletes a file configured in `security.admin_password_file`, so the shared password of an HA deployment survives an admin password change.

### Platform encryption key

`security.secret_key_file` (environment variable `LO_SECURITY_SECRET_KEY_FILE`) names the file holding the platform's 256-bit AES-256-GCM key. The variable holds a file path, never the key bytes. The key encrypts sensitive metadata values, such as the AI provider API key configured in **Settings > AI**, which is never returned to the browser. The server also uses this key to derive the built-in self-monitoring project token instead of storing that credential in recoverable form.

The key file holds 32 random bytes encoded as standard base64, padded or unpadded. The server ignores surrounding whitespace and line breaks, so `openssl rand -base64 32` output works directly. Generate the bytes from a cryptographically secure random source, never from a passphrase or a KDF over a passphrase. A human-chosen key is vulnerable to dictionary attacks, which would recover both the key matching the recorded fingerprint and every encrypted value. Leaving the field empty defaults to `<data_dir>/secret.key`. On first start, the server creates a fresh key with mode `0600`. It reads an existing file without rewriting it and keeps the key in memory for the process lifetime.

Every replica sharing a metadata database must load the same key, and the server enforces this at startup. The first startup stores a 128-bit HMAC-SHA256 fingerprint over a fixed label in the `platform_key_fingerprints` table and logs the key file path with that fingerprint. No key material is stored in the table. Every later startup computes the fingerprint of its loaded key and compares it against the stored value. A match proceeds normally. A mismatch aborts startup before the gateway, storage, or bootstrap initializes, exits non-zero, and prints the key file path, both fingerprints, the likely cause, and the recovery step. Concurrent first starts on an empty database insert one fingerprint conditionally, so other replicas read and verify against that row rather than overwrite it. A regenerated Secret or mismatched replica key therefore fails fast instead of silently breaking AI settings and deriving a conflicting self-monitoring token.

The fingerprint is a truncated one-way digest and never exposes the key, so it is safe to paste in tickets and logs. Anyone holding a database backup can still test a full key candidate against the fingerprint, which is why the key must remain 32 cryptographically random bytes.

The Helm chart mounts one Secret read-only at `/etc/lazy-observe-key/secret.key`, using `encryptionKey.existingSecret` or the chart-managed `<fullname>-encryption-key` when `encryptionKey.autoGenerate` is `true`. A `standalone-ha` release without either refuses to render at any replica count, including one. Its `/data` volume is an `emptyDir` while the metadata database is external, so a key generated inside a pod disappears when the pod is recreated and the replacement pod fails with a fingerprint mismatch and no copy of the original key. Set `encryptionKey.existingSecret` or keep `encryptionKey.autoGenerate=true`. The `allinone` profile with persistence stores the key inside the PVC's `data_dir`. The `allinone` profile without a PVC keeps both the key and its metadata database in the same `emptyDir`, so both reset together and cannot mismatch.

A fingerprint mismatch means the configured key differs from the key this deployment initialized with. Recover through one of two paths:

1. **Restore the original key.** Put the previous key file back, such as the prior Secret content in GitOps, and restart. Nothing has been re-encrypted, so every sealed value becomes readable again.
2. **Confirm an intentional rotation.** Use this path only when the old key is permanently lost and you accept losing sealed values. Rotation applies to the entire deployment. Because a server checks the fingerprint only at startup, stop every replica first, or scale the Deployment to zero in Kubernetes. Any replica that stays running keeps sealing values with the old key that other replicas cannot decrypt.

   ```sh
   lazy-observe admin reset-key-fingerprint --config /etc/lazy-observe/config.yaml
   ```

   The command requires the key file to exist and never creates or rewrites one, so a mistyped path or unmounted Secret fails instead of recording an unusable fingerprint. It updates the recorded fingerprint, prints the old and new fingerprints, and writes a `security.key_fingerprint_reset` audit event with actor `cli`. Running it again with the same key prints `already recorded` and makes no change. No automatic rotation exists; the record changes only when an operator runs this command. Start the replicas with the new key afterward.

   In Kubernetes, run the command in a Job that mounts the same configuration ConfigMap and encryption key Secret as the Deployment, using the exact image and tag from the Deployment:

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

   `kubectl run` with the same `--env` and `--overrides` volume mounts works as well, provided the pod can reach the metadata database and read the key file.

Key rotation does not re-encrypt existing ciphertext, so values sealed with the previous key remain unreadable. Re-enter the AI provider API key in **Settings > AI** to seal it under the new key. A new key also derives a new self-monitoring project token on the next startup. The previous derived credential stays in metadata and remains a valid ingest credential for the self-monitoring project; revoke it from that project's token list if the old key was exposed.

Back up the key with the same care as the metadata database, because losing it makes sealed values unrecoverable. Because the `platform_key_fingerprints` table is part of the metadata baseline, a SQLite or PostgreSQL database created by an earlier commit refuses to start with `metadata migration 1 checksum mismatch` before the fingerprint check runs. Recreate the development database per [ADR 0003](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0003-pre-release-single-database-baseline.md) while keeping the same key file so the first startup records the matching fingerprint.

### Optional AI access layer

AI is disabled by default. Administrators configure the provider (`openai`, `anthropic`, `deepseek`, or `ollama`), optional `base_url`, model, and API key exclusively in **Settings > AI**. The server stores provider settings and the API key in metadata, encrypts the API key with `security.secret_key_file`, and never returns the key to the browser. There is no server YAML or `LO_AI_*` interface for provider configuration or API keys. Only static LLM token pricing overrides live in server configuration under `ai.pricing_file` (`LO_AI_PRICING_FILE`).

When enabled, the gateway supports admin connectivity testing, natural-language MetricsQL and LogsQL generation and explanation, and asynchronous analysis for new Issues. Generated queries run through the project-isolated gateway with response and time limits. Issue analysis gathers bounded cross-signal evidence and never delays error ingestion. Prompt and completion content is not collected unless an SDK opts in.

### Console links in alert notifications

Set `external_url` (`LO_EXTERNAL_URL` or `--external-url`; Helm value `externalURL`) to the public console base URL (such as `https://observe.example.com`, with no path, query, or fragment) so outbound alert notifications include clickable absolute links. Leaving `external_url` empty disables links and keeps notification payloads free of console URLs.

Link targets by notification type:

- **Issue alerts** (`new_issue`, `regression`): `<external_url>/issues/<id>?project=<slug>`
- **Rule alerts**: `<external_url>/alerts?fingerprint=<fp>&project=<slug>`, or `<external_url>/alerts?project=<slug>&rule=<rule>` for grouped multi-alert notifications
- **Alert storm**: `<external_url>/alerts?project=<slug>&status=firing`
- **Error rate spike**: `<external_url>/issues?project=<slug>`
- **Channel test**: `<external_url>/alerts/channels`

Per-channel rendering:

- **Feishu**: primary card action button (`View in lazy-observe`) plus per-alert `lark_md` links (`View`) when multiple alerts are grouped.
- **DingTalk & WeCom**: markdown links (`[View in lazy-observe](...)`, plus per-alert `[View](...)` for multi-alert groups).
- **Slack**: `mrkdwn` links (`<url|View in lazy-observe>`, plus per-alert `<url|View>` for multi-alert groups).
- **Email (`smtp`)**: `View in lazy-observe: <url>` on the first line of the message body, followed by the JSON payload.
- **Webhook**: optional `url` and `alerts[].url` JSON fields.

Opening a link switches the browser tab to the linked project. If that project does not exist or the user cannot access it, the console shows an error page instead of another project's data.

Links are derived at send time rather than enqueue time, so changing `external_url` applies to queued and retried notifications immediately.

## Standalone

Standalone mode still runs the gateway, in-process alert engine, metadata database, blob store, and optional `vmagent`. Here is a minimal configuration:

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

Release archives embed the verified `vmagent-prod` binary, so leave `storage_bin_dir` empty in minimal standalone setups. If you set a development or offline `storage_bin_dir` override, that directory must contain `vmagent-prod` whenever `ingest_url` is set. Without `ingest_url`, metric OTLP ingest and scrape management are disabled. External signal storage must use the Victoria family (VictoriaMetrics, VictoriaLogs, and VictoriaTraces); Loki and Tempo are not supported by design ([ADR 0013](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0013-signal-storage-is-victoria-only.md)). Setting `kind: prometheus` routes queries to a generic Prometheus-compatible API, but that mode is not officially supported or covered by release verification.

The supervisor never starts, signals, or restarts external metrics, logs, or traces services. If lazy-observe starts the optional `vmagent`, the supervisor manages and restarts that child process.

Because the supervisor does not manage external services, their query latency depends on your external configuration. Embedded storage runs VictoriaMetrics and VictoriaTraces with `-search.latencyOffset=0s`, so new data is queryable right away.

VictoriaTraces hides spans newer than `-search.latencyOffset` (default 30 seconds) from its Jaeger and Tempo search APIs, and from v0.12 onward from LogsQL queries as well. The gateway sends `disable_latency_offset=true` on every LogsQL request it makes to VictoriaTraces for LLM call statistics. Single-node VictoriaTraces and `vtselect` v0.12 and later honor this parameter, so external deployments need no flag for current LLM statistics. Earlier versions apply no LogsQL offset and ignore unknown query arguments. This behavior was verified in upstream source code rather than tested against old binaries.

The Jaeger API has no per-request switch. On an external VictoriaTraces instance, the trace list omits spans newer than `-search.latencyOffset`, and trace detail lookups, which Issue AI analysis also uses, return not found until the trace-ID index flushes. That flush takes up to `-insert.indexFlushInterval` (default 20 seconds on single-node or `vtinsert`). Lower both flags on the external VictoriaTraces service if trace views need fresh spans. Embedded VictoriaTraces runs with `-search.latencyOffset=0s` and `-insert.indexFlushInterval=1s`.

The gateway uses one storage interface in both modes. In standalone mode, it routes traffic as follows:

| Setting | Gateway traffic |
| --- | --- |
| `storage.metrics.query_url` | Prometheus-compatible query and metadata APIs |
| `storage.metrics.ingest_url` | OTLP metrics, Prometheus remote-write, Web Vitals import, and optional `vmagent` remote-write |
| `storage.logs.url` (or split `query_url` / `ingest_url`) | VictoriaLogs OTLP and JSON-line writes and LogsQL reads |
| `storage.traces.url` (or split `query_url` / `ingest_url`) | VictoriaTraces OTLP writes and Jaeger-compatible reads |

`/api/v1/write` accepts Prometheus remote-write 1.0 (`prometheus.WriteRequest`) and rewrites protobuf labels to enforce the authenticated `project_id`, so it does not rely on the VictoriaMetrics-only `extra_label` query parameter. The configured receiver must expose `/api/v1/write`. `/v1/metrics` requires VictoriaMetrics' `/opentelemetry/v1/metrics` endpoint, and Web Vitals require `/api/v1/import/prometheus`. If `ingest_url` is empty, these write routes return `501` instead of falling back to `query_url`. Health and readiness checks also probe a separate write endpoint.

VictoriaLogs and VictoriaTraces traffic uses their documented single-node paths, and the gateway replaces incoming `AccountID` and `ProjectID` headers with the authenticated project tenant on both writes and reads.

External storage owns metrics, logs, and traces retention in standalone mode, so `retention.metrics`, `retention.logs`, and `retention.traces` only apply to embedded storage and are ignored here. Platform retention still cleans replay, source maps, alert events, profiles, and closed Issues. The gateway also prunes completed error receipts after `error_inbox.receipt_retention` (default: `retention.logs`); set it to the external VictoriaLogs `-retentionPeriod` so the error deduplication window matches how long the raw error events stay queryable. Startup logs a warning with the effective value.

## Query API, MCP, and Grafana

Every read authenticates as a user, never as a project. A project API token (`lo_...`) can only ingest telemetry. Any read route receiving one returns `403` with `code: ingest_only_token`, because SDK DSNs sit inside public browser pages ([ADR 0010](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0010-project-api-tokens-are-ingest-only.md)). This applies to `/mcp` and `/api/system/info` as well. Source map upload tokens (`los_...`) are restricted to source-map uploads.

Reads accept one of two user credentials:

- The browser session cookie (`lss_...`).
- A user API token (`lou_...`) sent in `Authorization: Bearer ...`, with the project chosen by `X-LO-Project: <slug>`. Without that header, the gateway queries the `default` project.

Create a user API token in **Settings > Personal API tokens**; the secret displays once. The HTTP equivalent is `POST /api/auth/tokens` with a session cookie and the `X-CSRF-Token` double-submit header. A user API token inherits the issuing user's current role (`admin`, `member`, or `viewer`), so an admin token reaches admin-only routes.

### Prometheus-compatible queries

| Endpoint | Purpose |
| --- | --- |
| `GET`/`POST /api/v1/query` | Instant PromQL or MetricsQL query |
| `GET`/`POST /api/v1/query_range` | Range query |
| `GET`/`POST /api/v1/series` | Series metadata |
| `GET`/`POST /api/v1/labels` | Label names |
| `GET`/`POST /api/v1/label/<name>/values` | Label values |

The gateway injects the authenticated project's `project_id` filter into every query and strips client-supplied isolation parameters so one project cannot read another project's metrics. Writes remain on the ingest path. `/api/v1/write` handles Prometheus remote write and requires a project API token (`lo_...`).

```sh
curl -fsS -G http://localhost:3100/api/v1/query \
  -H 'Authorization: Bearer lou_...' \
  -H 'X-LO-Project: default' \
  --data-urlencode 'query=up'
```

LogsQL, trace, Replay, Issue, alert, and performance reads live under `/api` (such as `/api/v1/logs/query`, `/api/traces/search`, and `/api/v1/issues`) and use the same credential pair. They are outside the Prometheus data source endpoints.

### Grafana

Add a Prometheus data source pointing at the gateway root URL, such as `http://lazy-observe:3100`, with no path prefix. Because Grafana's Prometheus data source has no bearer-token field, send both headers as custom HTTP headers:

- `Authorization: Bearer lou_...`
- `X-LO-Project: <slug>`

For file provisioning, store header names under `jsonData.httpHeaderNameN` and values under `secureJsonData.httpHeaderValueN`, following [Grafana provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/), so tokens stay out of plain-text config files. Each data source fixes one `X-LO-Project` value and queries one project. Grafana panels calling other Prometheus endpoints, such as metadata, exemplars, and rules, hit unregistered routes and return `404`. Only the five endpoints in the table above are supported.

See [Grafana and Perses](grafana-perses.md), [`deploy/integrations/grafana/datasources.yaml`](https://github.com/eimsound/lazy-observe/blob/main/deploy/integrations/grafana/datasources.yaml), and [`deploy/integrations/perses/datasources.yaml`](https://github.com/eimsound/lazy-observe/blob/main/deploy/integrations/perses/datasources.yaml) for provisioning examples covering metrics, logs, and traces, including the path-rewrite proxy and the [ADR 0017](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0017-no-built-in-dashboards.md) dashboard boundary.

### MCP

`lazy-observe mcp` runs a project-scoped stdio proxy for MCP clients:

```sh
lazy-observe mcp --dsn 'http://localhost:3100/p/default@lou_...'
```

The DSN uses the SDK shape (`/p/<slug>@<token>`) with two differences. First, `<token>` must be a user API token (`lou_...`), which the CLI validates at startup instead of waiting for the first tool call. Second, `<slug>` is sent in the `X-LO-Project` header. Startup validation checks only the prefix, so a revoked `lou_...` token still starts the proxy and fails when a tool runs. `/mcp` rejects `lo_...` tokens with `403 ingest_only_token`. Exposed tools are read-only and include bounded Replay metadata; raw Replay events and URL queries are excluded.

## Kubernetes with Helm

The chart lives in `deploy/helm/lazy-observe`. Its default `standalone-ha` mode runs two replicas and expects reachable PostgreSQL, Redis, S3-compatible object storage, VictoriaMetrics, VictoriaLogs, and VictoriaTraces services. Store credentials in a Kubernetes Secret rather than a values file:

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

For the three-replica cluster topology, start from the checked-in cluster profile and override its example endpoints and Secret references:

```sh
helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe \
  --set secret.existingSecret=lazy-observe \
  -f deploy/helm/lazy-observe/values-standalone-ha-cluster.yaml
```

That profile routes metrics queries to `vmselect` and writes to `vminsert` under one platform account, while the gateway enforces the `project_id` boundary. Its VictoriaLogs and VictoriaTraces URLs must point to path-aware Services or `vmauth` frontends that route `/insert/*` and `/select/*` to the split roles while preserving `AccountID` and `ProjectID`. Split roles provide routing and sharding; the operator must provision redundant frontends, storage replication where supported, and persistent volumes.

Switch modes for a single-pod deployment with embedded storage. The chart enforces one replica, uses a `Recreate` Deployment strategy, and mounts a PVC at `/data`:

```sh
helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe --create-namespace \
  --set mode=allinone \
  --set persistence.size=50Gi
```

### Pinning the gateway image by digest

`image.digest` accepts a registry digest formatted as `sha256:` followed by 64 lowercase hexadecimal characters. When set, the chart renders `<repository>:<tag>@<digest>`, or `<repository>@<digest>` when `image.tag` is empty. The digest selects the exact image bytes that run. A malformed digest fails template rendering rather than silently falling back to a mutable tag:

```sh
helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe \
  --set secret.existingSecret=lazy-observe \
  --set image.tag=0.1.0 \
  --set image.digest=sha256:...
```

Avoid copying digests by hand. Every server release attaches `helm-values-lazy-observe-<version>.yaml` to its GitHub Release, generated from the digest that `docker/build-push-action` returns after pushing the multi-architecture image to `ghcr.io/eimsound/lazy-observe`:

```sh
gh release download v0.1.0 --pattern 'helm-values-lazy-observe-*.yaml'
helm upgrade --install lazy-observe deploy/helm/lazy-observe \
  --namespace lazy-observe \
  -f deploy/helm/lazy-observe/values-standalone-ha-cluster.yaml \
  -f helm-values-lazy-observe-0.1.0.yaml
```

Always quote `image.tag` in YAML (`tag: "0.1"`). Without quotes, YAML parses `0.10` as a number and drops the trailing zero (`0.1`). The chart stringifies numeric tags so templates still render, but the rendered tag no longer matches the intended release.

The chart is consumed directly from `deploy/helm/lazy-observe` in the repository and is not published as a `.tgz` or OCI artifact. The release workflow does not write the published digest back into `values.yaml`, because a checked-in digest would lag one release behind and require publishing runs to commit to `main`.

`standalone-ha` requires a platform encryption key Secret, either `encryptionKey.existingSecret` or the chart-generated Secret when `encryptionKey.autoGenerate=true`. The chart refuses to render `standalone-ha` without one at any replica count because the key must outlive pods; `allinone` with a PVC stores the key inside `data_dir`. See [Platform encryption key](#platform-encryption-key).

`/livez` reports process liveness without probing dependencies. `/readyz` starts at `503`, switches to `200` only after initialization, migrations, and dependency checks pass, and returns `503` as soon as shutdown starts. `/healthz` returns the human-readable aggregate component report. The pre-stop lifecycle hook works on the distroless image by calling the binary's built-in pre-stop helper, which signals PID 1 and waits through a short endpoint-removal window. The default 45-second termination grace covers the 20-second gateway drain, the 15-second child shutdown deadline, and the pre-stop hook window.

The optional HPA (`hpa.enabled=true`) renders only in `standalone-ha` mode and scales on CPU utilization; default CPU requests are set so the metric works out of the box. The PDB is enabled by default with `minAvailable: 1`. Size the HPA minimum and PDB together so voluntary disruptions never remove all ready replicas. Ingress is opt-in under `ingress.*`.

[ADR 0001](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0001-nightingale-style-alert-engine.md) defines the single-owner alert engine. Every application replica runs an evaluator candidate, while rendezvous assignment and a PostgreSQL lease and fencing epoch let only one candidate commit each rule's state and Outbox work. Changing gateway replica count shifts ownership without creating duplicate notification authority. This mechanism requires one shared writable PostgreSQL database.

Validate both chart modes before applying changes:

```sh
mise run helm-lint
mise run helm-template-test
```

Pull request CI runs both Helm checks in the `go` job whenever a diff modifies server, chart, or script files. `.github/workflows/ci.yml` selects jobs using `scripts/ci-changes.sh`.

The multi-replica HA gate runs in a separate `e2e-ha` job triggered by the `ha` flag in `scripts/ci-changes.sh`. That flag covers server code (`cmd/**`, `internal/**`, `go.mod`, `go.sum`, `sdk/go/**`), the HA gate script and its compiled helpers, `scripts/download-storage-bins.sh`, `bench/performance-gates.json`, the Ent generation chain (`entc.*`, `tools/entgen/**`, `scripts/generate-ent.sh`), and full-run paths (`.github/**`, `mise.toml`, `scripts/ci-*`, `Dockerfile*`). Chart-only edits under `deploy/**` do not set `ha`, so run `mise run e2e-ha` locally when chart changes affect multi-replica behavior. The gate runs against real components, including a replicated VictoriaMetrics cluster (with a `vmstorage` loss), split VictoriaLogs and VictoriaTraces roles with bidirectional tenant isolation, an Apache-2.0 SeaweedFS S3 endpoint for cross-replica Replay access, shared PostgreSQL alert state and notification Outbox, Redis sessions and rate limits, fenced alert ownership, readiness, and drain. Docker is required; missing Docker prerequisites exit with `77` (`SKIP`), which CI treats as a failure.

The primary chart value groups are `mode`, `replicaCount`, `image.{repository,tag,digest,pullPolicy}`, `externalURL`, `external.{postgres,redis,s3,metrics,logs,traces}`, `secret.*`, `resources`, `hpa.*`, `podDisruptionBudget.*`, `persistence.*`, `ingress.*`, `process.*`, `encryptionKey.*` (backing `security.secret_key_file`), and `adminPassword.*` (backing `security.admin_password_file`).

## PostgreSQL metadata

Configure a PostgreSQL URL in YAML or `LO_META_DB_URL`:

```yaml
meta:
  db_url: "postgres://lazy_observe:password@postgres:5432/lazy_observe?sslmode=require"
  migration_lock_timeout: 60s
```

On an empty database, the server creates the schema from one embedded PostgreSQL baseline. Use a dedicated database and credential. Running multiple application replicas also requires Redis for shared sessions and rate limits. Issue batches stay process-local while pending and merge atomically in PostgreSQL on flush.

Startup coordinates schema initialization using a PostgreSQL advisory lock, bounded waiting, a post-lock re-read, and a checksummed baseline marker. Lock timeout, connection loss, unlock failure, checksum drift, or baseline creation failure aborts startup before readiness becomes `200`. This coordination makes concurrent first starts safe but does not perform version-to-version schema upgrades.

After migration coordination finishes, a second PostgreSQL advisory lock serializes first-run `admin`, `default` project, and self-monitoring bootstrap. Multiple replicas can therefore start together against an empty database without racing seed records or credentials.

Before the first supported release, SQLite and PostgreSQL each maintain one mutable `001_baseline.sql`. Recreate the development database after an incompatible schema change; databases created by earlier commits are not a supported upgrade source. See [ADR 0003](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0003-pre-release-single-database-baseline.md).

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

When static keys are empty, the server uses the AWS SDK default credential chain. Custom endpoints use path-style requests. Configuration validation checks only that `bucket` is set and that static access and secret keys are paired; actual object operations verify bucket reachability.

## Retention

Retention applies globally per signal, not per project. In all-in-one mode, the gateway passes the three Victoria retention values as child-process flags. A 1-hour janitor deletes expired platform-owned metadata and blobs. Closed-Issue retention is fixed at 90 days when `retention.issues_closed` is `true`. Completed error receipts, which deduplicate at-least-once error redelivery, are deleted after `error_inbox.receipt_retention`; when unset it inherits `retention.logs`, which keeps all-in-one behavior unchanged.

Lowering retention makes older data eligible for storage engine or janitor deletion. Back up data before shortening retention windows.

## Pre-release replacement and backup

> See [Operations](operations.md) for the day-two runbook covering All-in-one and HA hot and cold backups, restore verification with `lazy-observe doctor --ha`, upgrades and rollbacks, key rotation (`admin reset-key-fingerprint`), and password recovery (`admin reset-password`).

`v0.x` releases are public previews. Upgrading may require recreating the metadata database, and protocols and SDKs may change incompatibly between versions. The database baseline remains mutable until the first supported release, which is not `v0.1.0` and has no scheduled date.

For a default all-in-one local deployment:

1. Stop the server cleanly and wait for all supervised children to exit.
2. Copy the entire configured `data_dir` as one consistent backup.
3. Replace the single `lazy-observe` binary, which bundles the web UI, storage manifest, and four pinned platform executables as one unit.
4. If the new build changed the database baseline, recreate the development `data_dir`. Otherwise start the gateway normally; it validates and atomically refreshes extracted binaries under `data_dir/bin` and checks the metadata baseline.
5. Check `/readyz` and `/healthz`, sign in, and verify signal queries before deleting the backup.

Deployments using the development `storage_bin_dir` override must replace all four executables in that directory as a matching set alongside the gateway.

Never copy live SQLite and storage directories as a consistent backup. In-place metadata upgrades and downgrades between preview builds are unsupported. Keep an old data directory paired with the exact binary that created it.

Back up external state separately. Use PostgreSQL-native backups for metadata, S3 versioning or snapshots for blobs, and each external Victoria backend's backup tools. Copying `data_dir` alone does not cover external backends.

For PostgreSQL, create a fresh database after a baseline change. Database-native backups remain useful for forensics, but newer preview builds are not guaranteed to upgrade them. The first supported release will introduce an append-only migration contract before user upgrades are supported.

Size the Helm startup probe longer than `meta.migration_lock_timeout` plus expected schema creation time. Initializing replicas remain unready, and the startup probe holds off liveness checks until initialization completes.

## Maintainer release workflow

The release and performance validation entry points are:

| Mise task | Gate |
| --- | --- |
| `mise run perf-release` | Build and size-check all four embedded platform binaries, then smoke-test native zero-configuration startup |
| `mise run perf-ha` | Enforce exact medium-corpus query P95 and capacity-controlled per-replica weak-scaling budgets against a validating sink, with real-storage mirrored readback |
| `mise run performance-gates` | Run all required performance tasks sequentially |

`mise run snapshot` runs `perf-release` before producing local archives without publishing. `mise run release` runs migration validation and `perf-release` before GoReleaser, and proceeds only when `HEAD` carries an exact `v*` tag. The embedded-payload and zero-configuration smoke check is mandatory in both packaging paths.

Pushing a `v*` tag triggers `.github/workflows/release.yml`, which builds Linux `amd64` and `arm64` archives and checksums on GitHub Releases and pushes the multi-architecture container image to `ghcr.io/eimsound/lazy-observe`. After the image push, the workflow attaches `helm-values-lazy-observe-<version>.yaml`, rendered by `scripts/render-helm-release-values.sh` from the published digest, so Helm users can pin the image without copying digests manually. Before building archives, the workflow requires passing dual-architecture container smoke tests (including Trivy scans of the container image and embedded Victoria binaries) and the full performance gates (`bench`, `perf-release`, `perf-ha`, `perf-results-check`). Maintainers can also trigger the workflow manually to rerun gates without publishing. Pull request CI skips the performance gates because shared runners add noise; they run on manual `bench.yml` dispatch, on its weekly schedule, and during releases.

Pull request CI runs incrementally. A `changes` job classifies modified paths and runs only the matching jobs. `ci-ok` aggregates all jobs, treats skipped jobs as passed, and is the single required check on `main`. Pure documentation changes (`*.md`, `docs/**`, `.agents/**`, `LICENSE`) run no business jobs. Server changes also run `e2e-ha`, and `internal/**` or `cmd/**` changes run the single-replica `e2e` and `e2e-restart` jobs. `mise run ci-changes-test` tests the classifier's full-run fallback rules.

All workflows run on GitHub-hosted runners (`ubuntu-24.04` and native `ubuntu-24.04-arm`). The repository registers no self-hosted runners so fork pull requests cannot run code on maintainer hardware. Release publication uses the workflow's built-in `GITHUB_TOKEN` with `contents: write` and `packages: write`; no personal access token is needed. Client SDKs release from the same repository using prefixed tags: `sdk/go/vX.Y.Z`, `sdk-node-vX.Y.Z`, `sdk-browser-vX.Y.Z`, and `sdk-python-vX.Y.Z`.

## Network and security notes

- Expose only the HTTP gateway port `:3100` by default; it also serves the embedded UI. The optional native OTLP/gRPC port (`:4317`) is disabled by default (`otlp_grpc.enabled: false`). Never expose the internal loopback child ports (`18428`, `19428`, `20428`, `22428`).
- Set both `security.tls_cert` and `security.tls_key` (or `--tls-cert` and `--tls-key`) for native TLS 1.2+ termination. In Kubernetes, terminate TLS at a trusted reverse proxy and enable `security.session_secure_cookie: true` with `security.trusted_proxies`.
- Treat all credentials as secrets: project ingest tokens (`lo_...`), source map upload tokens (`los_...`), user API tokens (`lou_...`), and session cookies (`lss_...`). Project-token DSNs are configuration values and must never be logged or committed.
- Run separate lazy-observe instances for unrelated security tenants.
- Use `standalone-ha` with shared PostgreSQL, Redis, S3, and signal backends for multi-replica Kubernetes deployments.
