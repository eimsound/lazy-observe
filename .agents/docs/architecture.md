# Architecture

This document describes the current main branch after the M1–M5 implementation and release-hardening review. Release builds default to the verified manifest and compressed storage executables embedded in the `lazy-observe` binary; an external `storage_bin_dir` is only a development/offline override. The document distinguishes implemented behavior from compatibility work that remains outside the v1 scope.

## Accepted architecture transitions

Accepted decisions deliberately change implementation choices in the
immutable v1.0 requirements baseline:

- [ADR 0001](adr/0001-nightingale-style-alert-engine.md) replaces duplicate
  evaluation with a lazy-observe-owned, Nightingale-style single-owner
  alert engine, fenced PostgreSQL leases, durable alert state, and a
  transactional notification outbox. The production path does not require
  `vmalert` or Alertmanager.
- [ADR 0003](adr/0003-pre-release-single-database-baseline.md) supersedes the
  earlier migration lifecycle while the project is pre-release. SQLite and
  PostgreSQL each use one mutable baseline; development databases are recreated
  after schema changes. Atlas is not part of the production database path.
- [ADR 0004](adr/0004-keep-ent-behind-metadata-persistence-seam.md) defines the
  implemented metadata seam that keeps Ent, project predicates, edge loading,
  dialect details, and persistence ordering out of the Alert engine, scrape
  targets, and startup wiring. Unrelated metadata behavior is unchanged.
- [ADR 0005](adr/0005-metrics-backed-application-performance.md) keeps endpoint
  latency, throughput, and error-rate aggregates on OpenTelemetry HTTP metrics
  while using traces as drill-down evidence for specific requests.
- [ADR 0006](adr/0006-centralize-current-user-access-behavior.md) places login,
  current-user profile/password/token behavior, and session lifecycle behind an
  account seam with explicit metadata and Redis session adapters.
- [ADR 0007](adr/0007-bound-accepted-asynchronous-telemetry-work.md) is implemented
  for error events through a bounded, metadata-backed Inbox. The Gateway durably
  accepts each event before returning success; workers resume across restarts,
  derive VictoriaLogs and Issue state idempotently, and expose backlog, retry,
  rejection, and terminal-failure diagnostics.

## Accepted but not implemented

- [ADR 0009](adr/0009-run-issue-ai-as-durable-jobs.md) defines durable Issue AI
  analysis as an independent follow-up capability. It is specified and planned
  separately from Core observability pre-production quality; the current
  in-memory automatic queue and synchronous manual request behavior remain in
  effect until the [Issue AI plan](plans/issue-ai-analysis.md) lands.

The sections below are the source of truth for current runtime behavior.

## Process model

`lazy-observe server` is the public process and Fiber gateway. In all-in-one mode it starts four verified release executables, one at a time, and waits for each `/health` endpoint before continuing:

| Child | Loopback address | Role | Persistent path |
| --- | --- | --- | --- |
| `victoria-metrics-prod` | `127.0.0.1:18428` | Metrics storage and query | `data/storage/metrics` |
| `victoria-logs-prod` | `127.0.0.1:19428` | Logs and raw error events | `data/storage/logs` |
| `victoria-traces-prod` | `127.0.0.1:20428` | Trace storage and Jaeger-compatible query | `data/storage/traces` |
| `vmagent-prod` | `127.0.0.1:22428` | Prometheus exporter scraping and remote write | Queue under `data/storage/scrape` |

Release builds compress and embed the four platform-specific executables. On
first use the gateway validates their manifest and SHA-256 values, extracts them
under `data_dir/bin`, and executes only those verified files; `storage_bin_dir`
remains a development override. The supervisor owns each child lifecycle,
prefixes its output, reloads vmagent after scrape configuration changes, and
restarts an unexpected exit with equal-jitter exponential backoff capped by
`process.restart_max`. During recovery `/livez` remains healthy while
`/healthz` and `/readyz` name the restarting component. A healthy restart resets
that component's retry index. Shutdown first disables every restart loop, then
stops vmagent before storage processes so pending remote writes can drain, and
finally enforces the configured timeout.

Standalone mode never starts or controls the three external storage services.
It starts—and therefore supervises and restarts—vmagent only when
`storage.metrics.ingest_url` is non-empty. Alert evaluation always runs inside
the lazy-observe application process.

Both topologies construct the same `storage.SignalBackend`. The gateway only
depends on that interface for metrics query/write bases, logs/traces bases,
health checks, and tenant headers. Embedded mode supplies the
managed loopback addresses; standalone supplies the configured external
addresses. HTTP handlers do not keep a second URL map or a standalone-specific
code path.

The production web application is embedded in the same release binary and served by the gateway with SPA fallback. During source development Vite can instead serve it on port 5173 and proxy `/api` to the gateway.

## Data flow

### Ingest

```text
project API token -> gateway authentication -> project lookup
    |                    |                    |
    |                    |                    +-> project-scoped metadata/blob keys
    |                    +-> strip untrusted tenant/filter controls
    +-> OTLP or platform event
             | metrics       -> inject project_id -> VictoriaMetrics
             | logs          -> tenant headers   -> VictoriaLogs
             | traces        -> tenant headers   -> VictoriaTraces
             | errors        -> durable metadata Inbox
             |                    +-> worker -> VLogs + fingerprinted Issue row
             | replay/maps   -> blob + metadata index
             + profiles      -> gzip pprof blob + metadata index
```

The public signal endpoints are OTLP/HTTP `/v1/metrics`, `/v1/logs`, and `/v1/traces`. Error events, Vitals, replay segments, source maps, and profiles use platform endpoints. The gateway body limit is 52 MiB. A successful error-event response means the complete batch is durably present in the metadata Inbox; it does not mean VictoriaLogs indexing and Issue derivation have already completed. Capacity exhaustion rejects new work without evicting accepted entries.

vmagent reads generated scrape configuration, scrapes exporter endpoints, and remote-writes metrics. The in-process alert evaluator reads enabled rules from metadata, elects one candidate per `(project_id, signal, rule_id)` using rendezvous hashing, and must acquire a database lease with a monotonic fencing epoch before committing. It evaluates MetricsQL/PromQL through the project-filtered metrics boundary and LogsQL through VictoriaLogs `stats_query` with trusted tenant headers. State, user-visible alert events, and notification Outbox rows commit atomically; dispatch workers retry durable at-least-once deliveries with stable idempotency keys.

Metrics reads and writes may use different external addresses. OTLP metrics,
Prometheus remote-write, and Web Vitals use `storage.metrics.ingest_url`; queries
use `storage.metrics.query_url`. Remote-write 1.0 bodies are decoded and every
client-supplied `project_id` label is replaced before the request is re-encoded.
The gateway emits the canonical Snappy/protobuf content headers and remote-write
version `0.1.0`, so project integrity does not depend on VictoriaMetrics'
`extra_label` extension. Web Vitals use VictoriaMetrics' Prometheus text-import
API.

### Query

UI sessions use an HTTP-only session cookie. MCP clients, scripts, and CLIs use a user API token plus `X-LO-Project`. Project API tokens are ingest-only and every query route rejects them with `403 ingest_only_token` (ADR 0010); user tokens cannot ingest telemetry. Before a query reaches storage, the gateway resolves its project and applies the same isolation boundary as ingest:

- Metrics queries use VictoriaMetrics `extra_filters` in VM mode or server-side PromQL AST matcher injection in `prometheus` mode.
- Metrics autocomplete uses the same isolated label-name and label-value APIs,
  so switching projects cannot reuse another project's storage suggestions;
  PromQL prettification is an authenticated parser-only route and never reads
  series data.
- LogsQL requests receive trusted VictoriaLogs tenant headers.
- Jaeger-compatible trace requests receive trusted VictoriaTraces tenant headers.
- Issues, replay metadata, source maps, profiles, alert data, and scrape targets are filtered by project-owned metadata.

### Isolation enforcement

VictoriaMetrics single-node has no native tenant boundary. The gateway overwrites the incoming `project_id` label on writes and removes client-supplied `extra_filters`, `extra_filters[]`, and `extra_label` before adding its trusted filter. It rewrites Prometheus remote-write 1.0 protobuf labels directly. In Prometheus-compatible standalone mode it parses PromQL and adds `project_id=<project>` to every vector selector.

VictoriaLogs and VictoriaTraces use their native `AccountID`/`ProjectID` headers. Incoming tenant headers are removed or overwritten. Their loopback endpoints are implementation details and must not be exposed publicly.

## Storage boundaries

| Store | Owns | Does not own |
| --- | --- | --- |
| SQLite or PostgreSQL | Users, sessions, projects, tokens, rules, silences, channels, inhibition, alert events, Issues, Issue lifecycle facts, bounded pending error payloads and receipts, scrape targets, replay/source-map/profile indexes | Finalized raw signal streams and blob bodies |
| Local blob store or S3 | Replay segments, source maps, gzip pprof payloads | Relational indexes |
| VictoriaMetrics | Metrics | Logs, traces, platform metadata and alert state |
| VictoriaLogs | OTLP logs and raw structured error events | Issue aggregate state |
| VictoriaTraces | OTLP traces | Application metadata |

SQLite is the zero-configuration default at `data/meta/lazy-observe.db`. It uses the pure-Go `modernc.org/sqlite` driver, WAL, foreign keys, a five-second busy timeout, and one open connection. PostgreSQL uses pgx. During the pre-release phase, both dialects create the current schema from one embedded `001_baseline.sql`; the single `schema_version` row identifies that baseline and is not a supported upgrade chain.

Local blob keys are rooted under the configured data directory and partitioned by project. S3 uses the same logical keys; a custom endpoint enables path-style requests for S3-compatible services.

## Security model

### API tokens and sessions

- The first boot creates `admin` with a random password and logs it once.
- The first boot also creates the `default` project and a random `lo_...` project API token, logged once.
- Project API tokens authenticate telemetry ingest only; they are embedded in browser pages and SDK configuration and never authorize reads (ADR 0010). Admins create, list, and revoke them through `/api/projects/:id/tokens`. SDKs encode the project and token in a DSN for configuration, but send the token only through `Authorization: Bearer <token>`.
- User API tokens use the `lou_...` prefix and authenticate API calls through `Authorization: Bearer <token>` with the issuing user's current RBAC role. They can select a query project with `X-LO-Project`, but cannot ingest telemetry. Users create, list, and revoke their own tokens through `/api/auth/tokens` or Settings; the secret is returned only on creation.
- Login creates an opaque server-side session and returns its token in an HTTP-only,
  `SameSite=Lax` cookie with a seven-day expiry. Production wiring explicitly chooses
  either the metadata-backed or Redis-backed account session adapter. Session lists
  expose a backend-neutral opaque revocation reference, never the authentication token.
- Session authentication resolves the current User from metadata before applying RBAC.
  A role change or User deletion therefore takes effect on an existing Redis session.
- When CSRF protection is enabled, session-authenticated mutations require a matching
  double-submit token. The session and CSRF cookies can be marked `Secure` independently
  of TLS termination location.
- Project-token authentication is cached and invalidated with a metadata revision when tokens change.

### Login hardening

The following controls are implemented:

- Passwords are stored as salted Argon2id hashes and compared in constant time. An
  unknown username still performs equivalent Argon2id work; this complements the
  random delay rather than representing a synthetic account.
- Missing users, incorrect passwords, and malformed credential requests return the
  same `401 invalid_credentials` status and body. Credential failures wait for a
  cryptographically random 0.5–1 second before responding.
- Every login attempt consumes a fixed-window source-IP bucket. A syntactically valid
  username also consumes a second bucket keyed by the SHA-256 hash of its normalized
  value. Each bucket permits ten attempts per minute, so changing source IPs does not
  bypass the per-username bound.
- A single application replica keeps the counters in process. Deployments with
  multiple Gateway instances use the Redis limiter, whose counter increment and
  expiry are atomic and shared by all instances.
- Exhausted buckets return `429 rate_limited`. Metadata, session, or limiter failures
  return `503 service_unavailable` instead of being disguised as invalid credentials;
  rate-limit and lockout responses include `Retry-After`.
- Five consecutive credential failures temporarily lock the normalized account key for
  15 minutes. The gate is process-local in zero-configuration mode and shared through
  Redis in a multi-replica deployment.
- Login removes a newly created session if the following metadata update fails. Password
  changes return `503 service_unavailable` when the password commit succeeds but other
  sessions cannot be revoked; this cross-store outcome is observable and is not reported
  as an atomic rollback.
- Successful login, invalid credentials, rate limiting, service failure, and logout
  are written to the management audit trail without recording passwords.
- The web login page maps `401`, `429`, server, and network failures to stable localized
  messages and does not display raw backend errors.

The following topics are deliberately recorded for later design and are not currently
implemented: MFA or Passkeys, progressive backoff beyond the fixed temporary lockout,
CAPTCHA, login-security metrics and alerts, sliding session renewal, and durable
cross-store compensation. No decision about these deferred controls is implied here.

### RBAC

The implemented roles are `admin`, `member`, and `viewer`. Administrative project, project-token, user, and channel changes require admin. Alert and scrape configuration changes require member or admin. User API tokens retain the issuing user's current role; project API tokens grant only their project's ingest and query boundary.

This is a single-team system with projects as data partitions, not an application-level multi-tenant control plane. Strong tenant separation between unrelated organizations should use separate instances.

### Browser privacy

Session Replay masks all input values and blocks `[data-lo-block]` subtrees by default. Cross-origin trace header injection is limited to an explicit allowlist. These defaults reduce exposure but do not replace an application-specific privacy review.

Setting both `security.tls_cert` and `security.tls_key` enables native gateway TLS with a minimum of TLS 1.2. A trusted reverse proxy may terminate TLS instead; leave both fields empty in that topology.

## Web exploration modules

The [Logs exploration workspace](adr/0008-headless-logs-exploration-workspace.md) is a headless TypeScript module behind one state-machine interface: callers can read a snapshot, subscribe, send typed events, and dispose it. It owns Project-scoped draft and executed LogsQL intent, URL normalization, Historical execution identities and independent result slots, refresh/cancel rules, and the Live connection/retry/deduplication/bounded-row lifecycle. It has no React, React Query, Router, or browser-global dependency.

The Logs React adapter executes finite query plans through React Query, connects Router location replacement and VictoriaLogs tail streams, and returns every asynchronous result with its workspace identity. `LogsPage` owns rendering, pure visual preferences, metadata autocomplete, drawers, and file saving; scrolling away from the Live viewport changes only visual following and never pauses the network stream.

## Extension points and current limits

- **Standalone:** implemented through `storage.SignalBackend` for Prometheus-compatible or VM metrics queries, optional external metrics writes, VictoriaLogs, and VictoriaTraces. Loki/LogQL and Tempo/TraceQL adapters remain separate follow-up work and are not part of the first standalone compatibility scope.
- **PostgreSQL:** implemented as an optional metadata backend and required for multi-replica metadata and alert-engine coordination. Durable error Inbox claims, rule membership, leases, fencing, state, and notification Outbox correctness live in PostgreSQL. Redis supplies shared sessions and rate limits.
- **S3:** implemented for replay, source-map, and profile blobs. Metadata remains SQLite or PostgreSQL.
- **MCP:** `/mcp` exposes project-scoped read-only observability tools over HTTP, including bounded Replay metadata; `lazy-observe mcp --dsn ...` provides a stdio proxy for MCP clients. Raw Replay events and URL queries are not exposed.
- **HA:** implemented for standalone application replicas through one shared writable PostgreSQL, Redis, S3, and external signal clusters. Each application replica hosts an alert-engine candidate, but rendezvous ownership plus a fenced database lease permits only one current owner to commit each rule. External dependencies and persistent volumes remain operator responsibilities.
- **AI:** an optional provider-neutral chat client supports OpenAI, Anthropic, and native Ollama protocols. Implemented features include connectivity checks, natural-language MetricsQL/LogsQL generation, bounded query execution and explanation, asynchronous new-Issue analysis, and project-scoped GenAI token/cost/call aggregation. AI remains disabled by default and outside mandatory ingest/query paths.
- **Release and performance gates:** release builds cover Linux on amd64/arm64 with CGO disabled. Local mise gates validate embedded payloads and zero-configuration startup, query P95, Go SDK middleware overhead, and real multi-replica scaling. GitHub Actions workflows consume the same entry points on GitHub-hosted runners; CI runs only the jobs whose surface a change touches, and CI, performance, and release runs are reported independently.

See [Deployment](../../docs/deployment.md) for concrete configuration and operational boundaries.
