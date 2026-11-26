# Operations: backup, restore, upgrade, rollback, and emergency procedures

This runbook covers All-in-one (single-node embedded) and HA (multi-replica) deployments of lazy-observe. It describes backup, restore, read-only preflight checks, upgrades, rollbacks, and emergency key or password recovery.

> **v0.x public preview compatibility constraint ([ADR 0003](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0003-pre-release-single-database-baseline.md))**
>
> Before the first supported release, SQLite and PostgreSQL each maintain one mutable `001_baseline.sql` metadata baseline. In-place upgrade and downgrade migration chains across incompatible schema changes are not supported.
>
> - Within the same metadata baseline version, normal and crash restarts preserve committed data, and you can replace the binary or roll container images in place.
> - Across metadata baseline versions, an older database fails at startup with `metadata migration 1 checksum mismatch` and requires recreating the metadata database. Check the target release notes and [version-matrix.md](version-matrix.md) before upgrading.

## 1. Persistent state and backup inventory

Before backing up a deployment, separate authoritative persistent state from artifacts that startup rebuilds automatically.

| Asset | All-in-one mode | HA mode (`topology: ha`) | Must back up | Notes |
| --- | --- | --- | --- | --- |
| Metadata database | `<data_dir>/meta/lazy-observe.db` (SQLite WAL) | External PostgreSQL (`meta.db_url`) | Yes | Stores users, projects, tokens, alert rules/state/leases/Outbox, Issues, Error Inbox, Replay/Source Map/Profile indexes, `platform_key_fingerprints`, and `schema_version` |
| Object and blob storage | `<data_dir>/blobs/` | S3 or S3-compatible object storage (`blob.backend: s3`) | Yes | Stores Session Replay segments, Source Map artifacts, and gzipped pprof profiles |
| Signal storage | `<data_dir>/storage/{metrics,logs,traces}` (and `storage/scrape`) | External VictoriaMetrics, VictoriaLogs, and VictoriaTraces clusters | Based on retention needs | Metrics, logs (including raw error events), and trace data |
| Platform encryption key | `<data_dir>/secret.key` (or `security.secret_key_file`) | Shared read-only key file (`security.secret_key_file`) | Critical | 256-bit AES-256-GCM key (32 bytes as standard base64); seals AI provider API keys and derives the self-monitoring token. Old ciphertext cannot be recovered if lost |
| Configuration and initial password files | `config.yaml` and `security.admin_password_file` | ConfigMap, Secret, `config.yaml`, and `security.admin_password_file` | Yes | Excludes `<data_dir>/initial-admin-password`, which the gateway deletes after the admin replaces the generated first password |
| Shared sessions and rate limits | In-process memory or SQLite | Redis or Valkey (`redis.enabled: true`) | No (volatile) | Stores login sessions, rate limits, and login lockout counters; users sign in again if lost |
| Extracted embedded storage binaries | `<data_dir>/bin/` | `<data_dir>/bin/` (only when `vmagent` is enabled) | No | Verified and atomically rebuilt from the `lazy-observe` binary at startup |

## 2. Backup procedures

### 2.1 All-in-one mode

#### Option A: cold backup (recommended for full-stack consistency)

A cold backup captures SQLite, local blobs, embedded VictoriaMetrics, VictoriaLogs, VictoriaTraces, and the platform key at one consistent point in time.

1. Stop the service cleanly and wait for the gateway drain (`process.drain_timeout`, default `20s`) and supervised child shutdown (`process.shutdown_timeout`, default `15s`). The supervisor stops `vmagent` first and then closes the embedded storage engines, which flush every `1s`:

   ```sh
   # systemd deployment
   sudo systemctl stop lazy-observe

   # Docker Compose deployment
   docker compose -f deploy/compose/docker-compose.yml stop lazy-observe
   ```

2. Archive `<data_dir>` and configuration files, excluding the auto-rebuilt `bin/` directory:

   ```sh
   BACKUP_TS=$(date -u +%Y%m%dT%H%M%SZ)
   BACKUP_DIR="/var/backups/lazy-observe/${BACKUP_TS}"
   install -d -m 0700 "${BACKUP_DIR}"

   # Record the binary version that created this backup (preview rollbacks must pair backup and binary)
   lazy-observe --version > "${BACKUP_DIR}/binary-version.txt"

   # Archive data directory (meta/, blobs/, storage/, secret.key)
   tar --exclude='./bin' -czf "${BACKUP_DIR}/data.tar.gz" -C /var/lib/lazy-observe .

   # Back up external config or secret_key_file if stored outside data_dir, and restrict permissions
   tar -czf "${BACKUP_DIR}/etc.tar.gz" -C /etc/lazy-observe .
   chmod 0600 "${BACKUP_DIR}"/*.tar.gz
   ```

3. Restart the service and check readiness:

   ```sh
   sudo systemctl start lazy-observe
   curl -fsS http://127.0.0.1:3100/readyz
   ```

> Never run `cp` or `tar` on a live SQLite file or live embedded Victoria directories while the service is running. `lazy-observe.db` uses WAL mode, so raw copies can miss `-wal` or `-shm` files and corrupt the database.

#### Option B: hot backup (metadata, blobs, and key without downtime)

When downtime is not acceptable, back up each layer online:

1. Back up SQLite metadata through the SQLite Online Backup API so WAL state stays intact:

   ```sh
   BACKUP_TS=$(date -u +%Y%m%dT%H%M%SZ)
   BACKUP_DIR="/var/backups/lazy-observe/${BACKUP_TS}"
   install -d -m 0700 "${BACKUP_DIR}"

   sqlite3 /var/lib/lazy-observe/meta/lazy-observe.db \
     ".backup '${BACKUP_DIR}/lazy-observe.db'"
   sqlite3 "${BACKUP_DIR}/lazy-observe.db" "PRAGMA integrity_check;"
   ```

2. Sync local blobs. Replay, Source Map, and Profile files are immutable once written:

   ```sh
   rsync -a /var/lib/lazy-observe/blobs/ "${BACKUP_DIR}/blobs/"
   ```

3. Archive the platform encryption key and record its fingerprint:

   - Store `secret.key` (default `<data_dir>/secret.key`, or the path in `security.secret_key_file`) encrypted in an external vault or offline medium with mode `0600`.
   - Record the 128-bit key fingerprint from startup logs (for example, `platform encryption key matches the recorded fingerprint ... fingerprint=<hex>`). The fingerprint is a one-way truncated digest with no key material, so it is safe to store in backup manifests and tickets.

4. Back up embedded signal storage online:

   - Embedded VictoriaMetrics (`127.0.0.1:18428`), VictoriaLogs (`127.0.0.1:19428`), VictoriaTraces (`127.0.0.1:20428`), and `vmagent` (`127.0.0.1:22428`) bind only to loopback and must never be exposed externally.
   - To back up metrics without downtime, invoke VictoriaMetrics' local snapshot endpoint (`POST http://127.0.0.1:18428/snapshot/create`) together with `vmbackup`, or take an underlying filesystem snapshot (LVM or ZFS). Otherwise schedule a maintenance window for Option A.

### 2.2 HA mode (PostgreSQL, S3, external Victoria clusters, and Redis)

In HA mode, each replica's `<data_dir>` is an ephemeral scratch directory used to extract the optional `vmagent` binary and write `scrape.yml`. Do not treat a replica's `<data_dir>` as a backup source. Back up each external dependency online:

1. Back up PostgreSQL metadata:

   ```sh
   BACKUP_TS=$(date -u +%Y%m%dT%H%M%SZ)
   pg_dump \
     --format=custom \
     --no-owner \
     --no-privileges \
     --file="/var/backups/lazy-observe/meta-${BACKUP_TS}.dump" \
     "${LO_META_DB_URL}"
   ```

   Pair logical dumps with PostgreSQL WAL archiving and scheduled base backups (`pg_basebackup` or pgBackRest) in production to support point-in-time recovery (PITR).

2. Back up S3 or compatible object storage:

   - Enable object versioning and lifecycle rules on `blob.s3.bucket`.
   - Or sync incrementally to a disaster-recovery bucket:

   ```sh
   aws s3 sync "s3://${LO_BLOB_S3_BUCKET}" "s3://${DR_S3_BUCKET}/lazy-observe/${BACKUP_TS}" \
     --endpoint-url "${LO_BLOB_S3_ENDPOINT}"
   ```

3. Back up external Victoria clusters:

   - **VictoriaMetrics.** Create incremental snapshots of `vmstorage` (cluster) or single-node instances with `vmbackup`.
   - **VictoriaLogs and VictoriaTraces.** Follow the upstream Victoria snapshot and backup tools for each engine.

4. Back up the platform encryption key and deployment configuration:

   - Back up `security.secret_key_file` encrypted (in Kubernetes, `encryptionKey.existingSecret` or `<fullname>-encryption-key`).
   - Save `config.yaml`, Helm values, and the deployed container image's immutable digest (such as `ghcr.io/eimsound/lazy-observe:0.1.0@sha256:...`).

5. Skip Redis or Valkey for consistent backups:

   - Redis stores only login sessions, rate-limit windows, failed-login lockouts, and cross-replica auth cache invalidation messages. That state is volatile and rebuilds on demand.

## 3. Restore and verification

### 3.1 All-in-one restore

1. Stop the running instance so two processes cannot write to the same `<data_dir>`.
2. Move or clear the damaged `<data_dir>`, extract the archive, and restore ownership and permissions:

   ```sh
   sudo systemctl stop lazy-observe
   sudo mv /var/lib/lazy-observe "/var/lib/lazy-observe.broken.$(date -u +%Y%m%dT%H%M%SZ)"
   sudo install -d -m 0750 -o lazy-observe -g lazy-observe /var/lib/lazy-observe
   sudo tar -xzf /var/backups/lazy-observe/<TS>/data.tar.gz -C /var/lib/lazy-observe
   sudo chown -R lazy-observe:lazy-observe /var/lib/lazy-observe
   sudo chmod 0600 /var/lib/lazy-observe/secret.key
   ```

   > In container deployments (`deploy/compose`), the process runs as `65532:65532` (distroless `nonroot`). Set the host directory or volume owner to `65532:65532`.

3. Confirm that the installed binary matches the metadata baseline of the backup, then start the service.
4. Run the post-startup verification checks in Section 3.3.

### 3.2 HA restore and read-only preflight with `lazy-observe doctor --ha`

1. Stop all application replicas, for example with `kubectl scale deployment/<name> --replicas=0` in Kubernetes, so old replicas cannot write metadata or renew alert leases during restore.
2. Restore PostgreSQL into a fresh empty database:

   ```sh
   createdb -h <pg-host> -U <pg-user> lazy_observe_restored
   pg_restore \
     --no-owner \
     --no-privileges \
     --dbname="postgres://<pg-user>:<pass>@<pg-host>:5432/lazy_observe_restored?sslmode=require" \
     /var/backups/lazy-observe/meta-<TS>.dump
   ```

3. Restore S3 objects and external signal storage, and mount the original `security.secret_key_file`.
4. Before starting replicas, run the read-only preflight check:

   ```sh
   lazy-observe doctor --ha --config /etc/lazy-observe/config.yaml
   ```

   `lazy-observe doctor --ha` (implemented in `cmd/lazy-observe/doctor.go`) is strictly read-only. It makes no database writes, creates no key files, and uploads no test objects. It runs these checks in order:

   | Check | Verification |
   | --- | --- |
   | `standalone` | `standalone: true` or `topology: ha` is enabled |
   | `redis` | `redis.enabled: true` and `PING` succeeds |
   | `metadata-postgres` | Read-only PostgreSQL connection confirms tables and checksum match the binary's metadata baseline |
   | `secret-key` | Reads `security.secret_key_file`, checks key format, and confirms its fingerprint matches `platform_key_fingerprints` |
   | `admin-password` | When no `admin` account exists in metadata, requires a valid `security.admin_password_file` (at least 8 characters); optional once `admin` exists |
   | `blob-s3` | Confirms `blob.backend: s3`, non-empty `blob.s3.bucket`, and a read-only `List` on the `doctor/` prefix |
   | `storage-metrics` / `storage-metrics-ingest` | Probes metrics query and ingest health endpoints |
   | `storage-logs` (and split `storage-logs-ingest`) | Probes logs query and ingest health endpoints |
   | `storage-traces` (and split `storage-traces-ingest`) | Probes traces query and ingest health endpoints |

   Start the application replicas only after every check prints `PASS` and the final line prints `HA ready: replicas can use shared PostgreSQL, Redis, S3, and signal storage`.

### 3.3 Post-startup verification

```sh
# 1. Liveness, readiness, and aggregate component health (each should return HTTP 200)
curl -fsS http://127.0.0.1:3100/livez
curl -fsS http://127.0.0.1:3100/readyz
curl -fsS http://127.0.0.1:3100/healthz

# 2. Verify system info and all three signal query paths with a user API token (lou_...)
curl -fsS http://127.0.0.1:3100/api/system/info \
  -H 'Authorization: Bearer lou_<your-token>' \
  -H 'X-LO-Project: default'

curl -fsS -G http://127.0.0.1:3100/api/v1/query \
  -H 'Authorization: Bearer lou_<your-token>' \
  -H 'X-LO-Project: default' \
  --data-urlencode 'query=up'

curl -fsS -G http://127.0.0.1:3100/api/v1/logs/query \
  -H 'Authorization: Bearer lou_<your-token>' \
  -H 'X-LO-Project: default' \
  --data-urlencode 'query=*' \
  --data-urlencode 'limit=1'

curl -fsS http://127.0.0.1:3100/api/traces/services \
  -H 'Authorization: Bearer lou_<your-token>' \
  -H 'X-LO-Project: default'
```

With an administrator account, also check `GET /api/system/error-inbox` to confirm there is no unexpected backlog, and spot-check historical Issues, Replay sessions, and Source Map lists in the web UI.

## 4. Upgrades and rollbacks

### 4.1 Pre-upgrade checklist

1. Read the target release notes and [version-matrix.md](version-matrix.md):
   - Check whether `001_baseline.sql` changed. During the `v0.x` preview phase, baseline changes require recreating the metadata database and do not support in-place upgrades.
   - Check whether protocol changes require upgrading client SDKs together with the server.
2. Complete the Section 2 backup and record the current binary version or container image `tag@sha256:<digest>`.
3. For container and Helm deployments, use the `helm-values-lazy-observe-<version>.yaml` file attached to the GitHub Release to pin the immutable image digest.
4. In standalone/HA mode, confirm `error_inbox.receipt_retention` (Helm `errorInbox.receiptRetention`, env `LO_ERROR_INBOX_RECEIPT_RETENTION`) matches the external VictoriaLogs retention. `retention.logs` only drives embedded storage, and receipts otherwise inherit it. The startup warning about ignored signal retention prints the effective receipt retention.

### 4.2 All-in-one upgrade

1. Stop the service cleanly with `SIGTERM`.
2. Run Option A cold backup from Section 2.1.
3. Replace the single `lazy-observe` binary (or update the Docker Compose image tag and digest). Release archives embed the web UI, checksum manifest, and four matched Victoria binaries. Never mix versions inside `storage_bin_dir`; if you use that development or offline override, replace all four binaries together.
4. If the new release changed the metadata baseline, keep the old `<data_dir>` backup and initialize a fresh database. If the baseline did not change, start the new binary directly; startup verifies and atomically refreshes `<data_dir>/bin`.
5. Run the probes and query checks in Section 3.3 before removing the old backup.

### 4.3 HA upgrade

1. Complete the Section 2.2 backup, including a PostgreSQL dump, S3 backup confirmation, and a verified `secret.key` backup.
2. Run the read-only preflight check using the new binary or image before touching live replicas:

   ```sh
   lazy-observe doctor --ha --config /etc/lazy-observe/config.yaml
   ```

   If the new release is incompatible with the existing PostgreSQL baseline, `metadata-postgres` prints `FAIL` right away without disrupting running replicas.

3. After every preflight check prints `PASS`, roll the replicas:
   - **Kubernetes and Helm.** The chart uses `RollingUpdate` and `PodDisruptionBudget`. The `pre-stop` hook sends `SIGUSR1` so `/readyz` returns `503`, waits through the endpoint-removal window, and then sends `SIGTERM` for graceful shutdown.
   - **Non-Kubernetes multi-replica hosts.** Upgrade one host at a time. Send `SIGUSR1` and wait for the front-end load balancer to drain traffic, send `SIGTERM`, replace the binary or image, start the instance, wait until `/readyz` returns `200`, and then proceed to the next host.

   > Keep `self_monitoring.enabled`, `self_monitoring.web`, and `self_monitoring.web_replay` identical across all HA replicas during rolling upgrades. Starting any replica with `self_monitoring.web: false` (or `self_monitoring.enabled: false`) revokes the shared `self-monitoring-web` token that other running replicas serve to signed-in browsers.
4. Once all replicas are ready, confirm alert rule evaluation, notification delivery, and `vmagent` leader handover in logs and `/healthz`.

### 4.4 Rollback

#### Same metadata baseline

1. Stop the new release (or roll back to the previous image digest or binary).
2. If the new release wrote no incompatible data, start the previous binary directly. If you suspect dirty writes, restore the pre-upgrade backup from Section 3 first.
3. Rerun `/readyz`, `/healthz`, and the signal query checks.

#### Across metadata baselines

- Never start an older binary against a metadata database created by a newer baseline.
- Restore the pre-upgrade database or data directory backup together with the exact pre-upgrade binary or image digest. In HA mode, run `lazy-observe doctor --ha` to confirm baseline and key fingerprint parity before restoring traffic.

## 5. Emergency procedures

### 5.1 Platform encryption key mismatch and rotation (`admin reset-key-fingerprint`)

At startup, lazy-observe computes the 128-bit fingerprint of `security.secret_key_file` (default `<data_dir>/secret.key`) and compares it with the `platform_key_fingerprints` table. On mismatch, the server refuses to start before initializing the gateway or storage, and logs the file path, the recorded fingerprint, and the loaded file fingerprint.

#### Case A: accidental Secret recreation or wrong key file mounted (lossless recovery)

**Do not run `reset-key-fingerprint`.**

1. Stop the failing replica.
2. Restore the original `secret.key` file from backup or your secret manager so its bytes match the healthy replicas, with mode `0600` (or read-only container mount `0444`).
3. Restart the replica and verify that startup logs print `platform encryption key matches the recorded fingerprint`. All existing encrypted values, including AI provider API keys, remain readable.

#### Case B: old key permanently lost or intentional key rotation

> `admin reset-key-fingerprint` does not re-encrypt historical ciphertext. Confirming rotation means accepting the loss of data sealed with the old key.

1. Stop every lazy-observe replica. Scale the Deployment to `0` in Kubernetes, or stop all systemd and Compose instances. Every replica must stop first because any running replica keeps sealing writes with the old key in memory, which produces ciphertext that rotated replicas cannot decrypt.
2. Generate or mount a new 256-bit random key file containing 32 random bytes encoded as standard base64. Never use a human passphrase:

   ```sh
   openssl rand -base64 32 > /etc/lazy-observe/secret.key
   chmod 0600 /etc/lazy-observe/secret.key
   chown lazy-observe:lazy-observe /etc/lazy-observe/secret.key
   ```

3. Run the fingerprint reset with the same server configuration and the new key file. The command only reads the key file and exits with an error if the file is missing; it never creates or rewrites a key file:

   ```sh
   lazy-observe admin reset-key-fingerprint --config /etc/lazy-observe/config.yaml
   ```

   On success, the command prints the old and new fingerprints and records a `security.key_fingerprint_reset` audit event with `actor=cli`. Running it again with the same key prints `already recorded` and changes nothing.

4. Restart all replicas. In HA mode, run `lazy-observe doctor --ha` first.
5. Complete the post-rotation cleanup:
   - Sign in to the web UI, open **Settings > AI**, and re-enter and save the AI provider API key so it seals under the new key.
   - The next startup derives new self-monitoring project tokens (`self-monitoring` and, when `self_monitoring.web` is enabled, `self-monitoring-web`) from the new key. Previous `self-monitoring-web` tokens are automatically revoked when starting with the new key or with `self_monitoring.web: false`, while the backend `self-monitoring` project token is retained and cannot be manually revoked through the management API because the built-in self-monitoring project is protected (`protected_project`).

### 5.2 Emergency password reset and lockout clearing (`admin reset-password`)

When an administrator forgets a password, gets locked out, or needs an offline password reset for any user, run the CLI directly against the metadata database without going through the HTTP API:

```sh
# 1. Reset admin on All-in-one without security.admin_password_file (generates a random password)
lazy-observe admin reset-password --config /etc/lazy-observe/config.yaml

# 2. Provide a password file (required for non-admin accounts, or when security.admin_password_file / HA is used)
install -m 0600 /dev/null /run/lazy-observe-new-password
printf '%s\n' '<at-least-8-chars-strong-password>' > /run/lazy-observe-new-password
lazy-observe admin reset-password \
  --config /etc/lazy-observe/config.yaml \
  --username admin \
  --password-file /run/lazy-observe-new-password
rm -f /run/lazy-observe-new-password
```

`cmd/lazy-observe/admin.go` implements these reset behaviors:

- **Generated password mode.** Omit `--password-file` only for `admin` when `security.admin_password_file` is unset. The CLI writes the new password to `<data_dir>/initial-admin-password` with mode `0600`, prints only the path, and forces a password change on the next sign-in. The gateway deletes that file after the password changes.
- **Password file mode.** Pass `--password-file` to supply a file. The CLI strips surrounding whitespace, requires at least 8 characters, does not force a password change on next sign-in, and never deletes or overwrites `security.admin_password_file`.
- **Session revocation and lockout clearing.** The command revokes all active sessions for the target account and records an `auth.password_reset` audit event with `actor=cli`. When Redis is enabled (HA deployments), pass the same `redis.*` configuration so the CLI revokes Redis sessions and clears the Redis failed-login lockout counter. By default, 5 consecutive failures trigger a 15-minute lockout. In a single-process deployment without Redis, the lockout counter lives in gateway memory, which the CLI cannot reach; restart the gateway if the account is locked out.

### 5.3 Graceful shutdown and signal handling

`lazy-observe server` (implemented in `cmd/lazy-observe/server.go` and `cmd/lazy-observe/server_gateway.go`) supports two-phase graceful shutdown:

| Signal | Behavior |
| --- | --- |
| `SIGUSR1` | Sets `/readyz` to `503` (`not_ready`) and closes long-lived streams such as Live Tail while keeping the listener open for in-flight requests. Use this signal during load-balancer drain windows. |
| `SIGTERM` / `SIGINT` | Starts full shutdown. If `SIGUSR1` was not received earlier, the gateway keeps listening for `250ms` so probes observe `503`. It then flushes the self-monitoring SDK, finishes in-flight requests and closes the Error Inbox within `process.drain_timeout` (default `20s`), and instructs the supervisor to stop `vmagent` first and then shut down the three embedded Victoria storage processes within `process.shutdown_timeout` (default `15s`). |

Set orchestrator stop grace periods (`stop_grace_period` in Docker Compose, `terminationGracePeriodSeconds` in Kubernetes, and `TimeoutStopSec` in systemd) to at least `45s` to cover the pre-stop drain window, `drain_timeout`, and `shutdown_timeout`.

## Related documentation

- [Deployment guide (configuration, Standalone, and Helm)](deployment.md)
- [Grafana and Perses integration guide](grafana-perses.md)
- [Docker Compose deployment](https://github.com/eimsound/lazy-observe/blob/main/deploy/compose/README.md)
- [systemd service deployment](https://github.com/eimsound/lazy-observe/blob/main/deploy/systemd/README.md)
- [Server and SDK version matrix](version-matrix.md)
