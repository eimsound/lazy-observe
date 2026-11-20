# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). No version has been released yet; versions will follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

> **Public preview.** v0.x releases are previews. Upgrading may require
> recreating the database, and protocols and SDKs may change incompatibly
> between versions. The database baseline stays mutable until the first
> supported release, which is not v0.1.0 and has no date yet.

### Added

- Published the project under the MIT license (copyright ClanEver) with GitHub
  (`eimsound/lazy-observe`) as the public primary repository; release archives
  and container images ship the bundled VictoriaMetrics components' license and
  notice files alongside third-party dependency notices (ADR 0012).
- Merged the Go, Node, browser, and Python client SDKs into this repository under
  `sdk/`. The Go SDK is the nested module
  `github.com/eimsound/lazy-observe/sdk/go`, consumed by the gateway through an
  in-repository `replace`, so a fresh clone builds without a sibling checkout or
  deploy key. SDKs release with prefixed tags (`sdk/go/vX.Y.Z`,
  `sdk-node-vX.Y.Z`, `sdk-browser-vX.Y.Z`, `sdk-python-vX.Y.Z`).
- Added the Python SDK (`lazyobserve`) with Django and Django REST Framework
  integrations, logging, database, and LLM instrumentation.

- Added narrowly scoped CORS preflight support for browser SDK error, Web
  Vitals, and Replay ingestion without exposing management or query APIs
  cross-origin.
- Moved the BYO LLM integration under administrator control in the web UI
  (Settings > AI): admins can enable/disable AI and manage provider, base URL,
  model, max tokens, and the API key from the browser; saved changes hot-swap
  the provider client without a restart. The API key is sealed with a
  platform encryption key file (first startup generates
  `<data_dir>/secret.key`, overridable via `security.secret_key_file`; HA
  replicas must share one file) and is never returned to the browser. The
  `ai:` section was removed from the server configuration (config files that
  still carry it fail loudly); the old `LO_AI_*` environment variables and
  `--ai-pricing-file` flag are gone.
- Added DeepSeek as a first-class AI provider (OpenAI-compatible protocol,
  default base URL `https://api.deepseek.com`, e.g. `deepseek-chat` /
  `deepseek-reasoner`).
- Added a VictoriaLogs-style LogsQL explorer with field and stream metadata,
  structured filters, grouped statistics, live tailing, configurable table and
  group views, chart controls, and URL-shareable query state.
- Added a project-scoped Prometheus-style MetricsQL/PromQL expression browser:
  vmui-style MetricsQL metric, label, label-value and function completion;
  metric/label exploration; parser-backed formatting; query history and tree;
  Table, Graph and Explain tabs; scalar/vector/matrix/string results, notices
  and timing; range navigation, resolution, stacked graphs and Y-axis controls;
  URL-shareable multi-query cards; and local-time/UTC dual-calendar selection.
- Added a fenced native MetricsQL/PromQL and LogsQL evaluator with rendezvous
  single-query ownership, durable pending/firing/recovered state, graceful
  membership release, and atomic state/event/Outbox commits with group-wait,
  repeat, resolved, silence, and inhibition handling.
- Made native evaluation the only active engine in the consolidated database
  baseline; new rules default to native on both database dialects.
- Added durable platform-event notification workers with multi-replica database
  claims, expired-worker recovery, channel rate-limit deferral, bounded
  exponential retries and dead letters, plus stable HTTP `Idempotency-Key`
  delivery; scheduled metric/log rules use the same durable path.
- Added a durable notification Outbox with stable idempotency keys, resolved
  priority, PostgreSQL `SKIP LOCKED` multi-worker claims, SQLite single-process
  claims, expired-claim recovery, non-consuming deferral, bounded retries, and
  dead-letter state.
- Added durable alert-engine membership, atomic per-rule leases with monotonic
  fencing epochs, and fenced pending/firing/recovered rule state to the paired
  SQLite/PostgreSQL baseline with database-time expiry checks.
- Added a pure alert-rule ownership kernel with canonical
  `(project_id, signal, rule_id)` keys, stable engine member IDs, deterministic
  rendezvous hashing, fixed golden vectors, and minimal membership-movement
  tests.
- Documented the accepted Nightingale-style single-owner alert-engine
  transition and the pre-release single-database-baseline policy, including
  PostgreSQL initialization locking, durable alert state, notification outbox,
  shadow evaluation, and cohort cutover.
- Added required release and performance gates for Linux amd64/arm64 embedded-binary size, native zero-configuration startup and aggregate idle RSS, paired Go Fiber SDK middleware overhead, exact 10k-Issue/100k-log/10k-trace query P95, and capacity-controlled per-replica weak scaling with a validating sink, per-request payload proof, balanced rounds, and real-storage mirrored readback.
- Added a clustered-HA Helm profile and a required multi-replica acceptance gate that exercises VictoriaMetrics replication and node loss, split VictoriaLogs/VictoriaTraces roles, cross-replica S3 Replay access, project isolation, shared PostgreSQL/Redis state, single-send notifications, and graceful draining.
- Added asynchronous automatic analysis for new Issues with bounded cross-signal evidence, release context, strict structured output, graceful shutdown, and best-effort inclusion of completed AI root-cause summaries in grouped platform notifications without blocking ingest or alert delivery.
- Added a unified signal-storage interface for real Standalone deployments, including split Prometheus-compatible query/write endpoints, native VictoriaLogs/VictoriaTraces tenancy, optional supervised vmagent, and project-safe Prometheus remote-write 1.0 ingestion.
- Added a project-scoped LLM observability page with model/service aggregates, token and cost trends, filtered calls, trace navigation, and privacy-gated GenAI span details; Go and Node SDKs now emit the current `gen_ai.provider.name` attribute while retaining legacy compatibility.
- Added Replay timeline markers for captured errors, privacy-safe HTTP request metadata, and Web Vitals across the browser SDK, gateway contract, mock data, and filterable player UI.
- Added privacy-bounded MCP tools for listing Session Replay metadata and inspecting segment/error/HTTP/Web-Vital marker summaries without exposing raw rrweb events or URL queries.
- Added allow-listed structured management audit logs for authentication, project, project-API-token, and user mutations without recording credentials, session values, or request bodies.
- Added a project-scoped backend performance endpoint derived from real server spans, including p50/p95/p99 latency, throughput, error rate, time-series buckets, and explicit truncation metadata.
- Added project-scoped LogsQL threshold rules alongside MetricsQL/PromQL rules, evaluated through the trusted project-isolated storage boundary.
- Added default Go goroutine pprof snapshots alongside continuous CPU and heap profiles, completing the three required Go profiling signal types.
- Added `security.admin_password_file` (`LO_SECURITY_ADMIN_PASSWORD_FILE`):
  while no admin account exists, the first admin password is read from that
  file (surrounding whitespace trimmed, at least 8 characters, otherwise
  startup fails). Without it a random password is written atomically to
  `<data_dir>/initial-admin-password` in mode 0600 on the replica that creates
  the admin, and the log records only that path and the hostname, never the
  password (#12).
- Added `adminPassword.{existingSecret,key,autoGenerate}` to the Helm chart, so
  the first admin password comes from a read-only Secret mount instead of the
  ephemeral data directory. `autoGenerate` renders `<fullname>-admin-password`
  (reused via `lookup` and kept across upgrades), rendering fails when
  `autoGenerate` is off while `standalone-ha` runs more than one replica or
  HPA, and the chart notes cover retrieving the password in each mode (#28).
- Added `encryptionKey.{existingSecret,key,autoGenerate}` to the Helm chart:
  the platform encryption key is mounted read-only and referenced through
  `security.secret_key_file`, so every `standalone-ha` replica and restart
  shares one key instead of each generating its own `secret.key`. The key
  Secret is `lookup`-reused with `resource-policy: keep`, `allinone` on a PVC
  keeps its data-directory key, and padded standard base64 key material (for
  example `openssl rand -base64 32` output) is accepted as-is (#3).
- The web UI now labels project API tokens as ingest-only and shows the SDK
  DSN to copy, and gives personal user API tokens copyable `mcp --dsn`
  commands plus Grafana Prometheus data-source URL and header snippets (#54).
- Added `lazy-observe admin reset-password`, which replaces an account password
  directly in the metadata database without the current one. The target
  defaults to `admin`; a generated password is offered only for `admin` while
  `security.admin_password_file` is unset, is written atomically to
  `<data_dir>/initial-admin-password` in mode 0600 with only the path printed,
  and forces a change at the next login, while `--password-file` follows the
  rules of `security.admin_password_file` and forces nothing. Every other
  account, and every deployment that sets `security.admin_password_file`
  (which includes all HA deployments), needs `--password-file`. The command
  revokes every session of the account, clears the Redis-backed login lockout
  when `redis.enabled` is set, writes an `auth.password_reset` audit event with
  actor `cli`, and exits non-zero when the account does not exist. After the
  admin replaces a bootstrap-generated password, the gateway deletes that file;
  a file named by `security.admin_password_file` is never removed (#56).
- `POST /api/events/diagnostics` now treats an unknown `signal` or `reason`
  the way Web Vitals treats an unknown metric name: the entry is skipped, the
  known measurements in the same batch are written, and the response reports
  `{accepted, rejected}` instead of `{accepted}`. The server now knows the
  `vitals` signal and the `server_rejected` reason; structural errors (bad
  measurement ID or timestamp, empty `signal` / `reason`, missing or over-long
  `service`, non-positive `dropped` or above 1e9) still answer `422` for the
  whole batch, and an empty or fully skipped batch no longer sends an empty
  write upstream (#59).
- The browser SDK reports a dropped Web Vital to `/api/events/diagnostics`
  instead of discarding it silently: a failed vitals POST increments
  `droppedTelemetry`, emits one local `console.warn`, and is reported with
  `signal: vitals` and `reason: server_rejected` for a 4xx, or
  `retry_exhausted` for a network error, a 5xx, 408, or 429 (#59, #61).
- Added `lazy-observe admin reset-key-fingerprint`, the explicit exit for a
  changed platform encryption key: it reads the existing key file (a missing
  file is an error, it never creates one), replaces the recorded fingerprint,
  prints the old and the new value, and records a
  `security.key_fingerprint_reset` audit event with actor `cli`. Running it
  with the same key prints `already recorded` and changes nothing (#60).
- Added `image.digest` to the Helm chart, so the image can be pinned by digest:
  it renders `<repository>:<tag>` without a digest (unchanged, default
  `:latest`), `<repository>@<digest>` with only a digest, and
  `<repository>:<tag>@<digest>` with both. A numeric `image.tag` such as `0.1`
  now renders as the string it looks like instead of an invalid image name. An
  empty `image.repository` or a digest that is not `sha256:` followed by 64
  lowercase hex characters fails the render instead of producing a broken
  reference or silently falling back to a mutable tag. Each release attaches
  `helm-values-lazy-observe-<version>.yaml` to its GitHub Release, a values
  snippet with the pushed image's tag and digest; the chart itself is still not
  packaged and no digest is written back to `main`. The release job that
  attaches the snippet has not run against a real `v*` release yet (#67).
- Added experimental continuous CPU profiling to the Python SDK, off by
  default: `profiling=True` (and `profiling_interval`, default 60 seconds)
  records one 10-second, 100 Hz window per interval from a background thread
  and uploads it to `POST /api/profiles` with the Go SDK's wire contract. It
  samples only on Linux with a free-threaded CPython 3.15+ (`python3.15t`)
  whose GIL is off; anywhere else, or once the GIL turns on, the SDK logs one
  warning with a reason code and does not sample. Prefork servers (gunicorn,
  uWSGI) work: each worker restarts its own sampler after `fork`.
  Greenlet-based code (gevent, eventlet) is not supported. See
  `docs/sdk/python.md#continuous-profiling` for the support range, measured
  overhead, and warning codes (#98, #100, #103).

### Security

- Project API tokens are now ingest-only. Every read route, including `/mcp`
  and `/api/system/info`, answers a project token with `403
  ingest_only_token`; queries need a session or a user API token (`lou_…`)
  plus `X-LO-Project` (ADR 0010). Browser SDK tokens embedded in public pages
  can no longer read a project's logs, traces, replays, or issues.
- Request bodies are bounded after decompression: gzip/deflate/br bodies that
  expand past 52 MiB are rejected with 413 on every route (including login and
  multipart uploads), Prometheus remote-write checks the declared snappy size
  first, and pprof uploads are decompressed with the same bound.
- Session-cookie mutations under `/api/auth/*` (logout, token create/revoke,
  password, profile, session revoke) now require the CSRF double-submit token
  when CSRF is enabled; login is unchanged.
- Project tokens, user API tokens, and session tokens are stored as SHA-256
  digests (metadata database and Redis); the self-monitoring token is derived
  from the platform key instead of being read back.
- The gateway recovers handler panics (500 instead of a process crash) and sets
  60s read / 120s idle timeouts; there is deliberately no write timeout so log
  tail streaming keeps working.
- Authentication cache and in-flight keys no longer borrow fasthttp request
  buffers: cache keys are `strings.Clone`d, Fiber runs with `Immutable: true`,
  and the other values that outlive a request (AI settings snapshots, audit and
  panic log fields, MCP handler credentials and project) are copied too. With
  borrowed keys about 1 % of ingest requests were written into another project
  and about 0.8 % of forged bearer tokens were accepted as a project that had
  just been cached (#27).
- The Go SDK Fiber middleware no longer borrows request buffers: method, path,
  host, scheme, route, and propagated request headers are cloned before they
  reach asynchronously exported spans, metric attribute keys, error tags,
  panic logs, and tracestate, so telemetry cannot pick up a later request's
  values when the application leaves `Immutable` off (#29).
- The generated bootstrap admin password is no longer written to the log; it is
  persisted in a 0600 file under the data directory or supplied by a
  deployer-owned file (#12).
- Logins that use a generated bootstrap password must change it first: until
  then the session or user API token reaches only `GET /api/auth/me`, password
  change, and logout, and every other route answers
  `403 password_change_required`. The web UI redirects every application route
  to `/change-password`, and `/api/auth/login` and `/api/auth/me` return
  `must_change_password` (#18).
- Hardened metrics project isolation: the gateway answers 500 and does not
  forward a metrics query that has no project, and the isolation filter
  rejects an empty project instead of building an empty `extra_filters` /
  `extra_label`. VictoriaMetrics v1.153 and later silently ignore an empty
  value there, so an unscoped query would otherwise run across all projects;
  project slugs are never empty today, so this is defense in depth (#72).
- Release builds now scan the embedded VictoriaMetrics, vmagent, VictoriaLogs,
  and VictoriaTraces binaries with Trivy before anything is published; the
  container image scan cannot see them, because they are gzip-compressed into
  the `lazy-observe` binary through `go:embed`. Findings only warn: each
  fixable CRITICAL/HIGH finding becomes a deduplicated workflow warning and a
  row in the step summary, and never blocks the release, because it comes
  from the upstream binaries and the project ships only the official upstream
  binaries, never ones it compiled itself (ADR 0016). On 2026-11-20 the scan
  reports 6 such findings covering 5 CVEs, all HIGH in the Go standard
  library. The `storage-bin-scan` job still fails, and the archive still waits
  for it, when
  the scan itself does not complete: Trivy errors, anything other than exactly
  four Go binaries recognized, or a report that does not match Trivy's exit
  code. Whether a pin is behind upstream is not decided by this job: before
  tagging a release, check the Renovate Dependency Dashboard (rerun it first)
  for pending upgrades of the Victoria binaries and Trivy (Renovate tracks
  these pins since #130, ADR 0015). The first version of this scan (#69)
  blocked a release when a pin was behind the latest upstream release that
  was at least 7 days old, or when that lookup failed; ADR 0016 replaced that
  policy (#69, #117). The container image scan is unchanged and still blocks.
- Source map upload now has its own credential, the project-scoped **source map
  upload token** (`los_…`; ADR 0018). `POST /api/releases/:release/sourcemaps`
  used to accept the project API token, which sits in public pages (ADR 0010)
  and could overwrite any release's source maps and keep creating releases
  until the storage was full. The endpoint now accepts only an upload token,
  which uploads source maps for its own project and nothing else; the project
  API token and the user API token (`lou_…`) get `403
  source_map_upload_token_required` there, and a `los_…` token gets `403
  upload_only_token` on every other route (ingest, query, `/mcp`, management).
  Only admins can list, create, or revoke upload tokens
  (`GET|POST /api/projects/:id/source-map-upload-tokens`, `DELETE
  …/:tokenID`, and the "Source map upload tokens" section of the project's
  token dialog); the value is shown once at creation, stored as a SHA-256
  digest, and creation and revocation are audited without the value. A revoked
  token stops working at once on the replica that handled the revocation and
  within 30 seconds on the others (the 30-second authentication cache TTL;
  `mise run e2e-ha` now checks this cross-replica bound for project API tokens
  and upload tokens, plus immediate revocation on every replica for uncached
  user API tokens; #132). There is still no per-project quota on total source
  map storage, only the existing per-release limit of 200 files and 50 MB
  (#116).

### Changed

- **Breaking:** project API tokens (`lo_…`) can no longer read anything (ADR
  0010). The `/api` session routes, including `/api/auth/me`, `/api/audit`,
  and `/api/system/error-inbox`, answer them with `403 ingest_only_token`
  instead of `401 unauthorized`, and reads need a session or a user API token
  (`lou_…`) plus `X-LO-Project` (#6).
- **Breaking:** the platform encryption key (`security.secret_key_file`) is
  recorded in the metadata database and enforced at startup: the first start
  stores 128 bits of HMAC-SHA256 over a fixed label in the single-row
  `platform_key_fingerprints` table and logs the key file path with that
  fingerprint, and every later start compares the key it loaded with it. A
  mismatch aborts the start before the gateway, storage, or bootstrap runs,
  exits non-zero, and prints the key file path, both fingerprints, the likely
  cause, and the recovery step, so a regenerated Secret or a replica holding
  its own key no longer runs with AI silently disabled and a re-derived
  self-monitoring token, and a deployment that renders a new key per rollout
  (for example `helm template` or Argo CD with
  `encryptionKey.autoGenerate=true`) fails to start instead of losing sealed
  values. Recover by restoring the original key; when it is gone, stop every
  replica and record the current one with
  `lazy-observe admin reset-key-fingerprint`, which re-seals nothing — the AI
  provider API key has to be entered again (#60).
- **Breaking:** `standalone-ha` now always requires a shared platform
  encryption key — `encryptionKey.existingSecret`, or the chart-managed Secret
  with `encryptionKey.autoGenerate=true` — and the chart refuses to render the
  profile at any replica count, including one, when neither is set. The
  default `encryptionKey.autoGenerate=true` needs no action; only a release
  that turns `autoGenerate` off and provides no `existingSecret` fails to
  render. Its `/data` is an emptyDir while the metadata database is external,
  so a key generated in the pod dies with it and the replacement pod is
  refused for the fingerprint mismatch. `allinone` with persistence keeps its
  data-directory key, and `allinone` without a PVC loses key and database
  together (#60).
- **Breaking:** `POST /api/releases/:release/sourcemaps` no longer accepts a
  project API token (`lo_…`) (ADR 0018). A build that uploads source maps
  with an `lo_…` DSN now gets `403 source_map_upload_token_required`: the Vite
  build fails and `lazy-observe-sourcemaps` exits non-zero. To migrate, an
  admin creates a source map upload token in Settings → Projects → Manage
  tokens → Source map upload tokens and copies the DSN shown once
  (`<origin>/p/<project>@los_…`); replace the CI secret that feeds the build
  (for example `LO_DSN`) with that DSN, and keep the ordinary `lo_…` DSN in
  the browser's `init({ dsn })`. The DSN shape and the endpoint are unchanged
  (#116).
- **Breaking:** `@lazy-observe/bundler-plugin` no longer builds on unplugin
  (#112). It is now a Vite plugin (the `vite` export of the package root and of
  `@lazy-observe/bundler-plugin/vite`) plus a `lazy-observe-sourcemaps upload
  --dir <directory> [--dsn <dsn>] [--release <release>] [--strip]` command for
  every other bundler, and it has no runtime dependencies. The package is ESM
  only, with no CJS build, and needs Node.js >= 22.15, where `require()` of it
  still works. The `webpack` export and the `./webpack` entry are gone:
  webpack, Rollup, esbuild, and Turbopack (Next.js 16) projects run the
  command after the build, for example `lazy-observe-sourcemaps upload --dir
  <output.path>`, and pass the same release to `init({ release })`; the
  package README has the Next.js recipe. `--dsn` defaults to `$LO_DSN` and
  `--release` to `$LO_RELEASE`, then to the 7-character Git SHA. The `dsn`
  option is now optional: with no DSN the plugin and the command warn, upload
  nothing, and succeed, so a local build needs no credentials (the old plugin
  failed the build). With a DSN set, an invalid DSN, a missing directory, or a
  failed upload fails the Vite build, or makes the command exit 1 (usage
  errors exit 2), and the error message never contains the token.
- **Breaking:** the Go SDK (`github.com/eimsound/lazy-observe/sdk/go`) supports
  Fiber v3 only (`v3.7.0`, #151). `lo.Fiber()` returns a
  `github.com/gofiber/fiber/v3` handler (`func(fiber.Ctx) error`) with no v2
  compatibility layer, so Fiber v2 apps cannot pass it to `app.Use`. The SDK
  has no tagged release yet. Migrate the app to Fiber v3 first (official
  [migration guide](https://docs.gofiber.io/whats_new#-migration-guide); full
  SDK notes in `sdk/go/README.md#fiber-v3-migration-notes`), then
  `app.Use(lo.Fiber())` works unchanged:
  - Handlers take the `fiber.Ctx` interface instead of `*fiber.Ctx`; imports
    move to `github.com/gofiber/fiber/v3`.
  - `c.UserContext()`/`c.SetUserContext()` become `c.Context()`/`c.SetContext()`
    (used by `slog.InfoContext`, `db.QueryRowContext`, etc.); v2 `c.Context()`
    (`*fasthttp.RequestCtx`) becomes `c.RequestCtx()`. `c.Context()` is safe to
    keep after the handler returns; `c` itself is pooled and is not.
  - `c.BodyParser`/`c.QueryParser` become `c.Bind().Body()`/`c.Bind().Query()`;
    typed query helpers become `fiber.Query[T](c, "key", default)`.
  - `c.Protocol()` returns the HTTP version (`HTTP/1.1`), while `c.Scheme()`
    returns `http`/`https`. `DisableStartupMessage` moves to
    `fiber.ListenConfig`, and `app.Test(req, ms)` becomes
    `app.Test(req, fiber.TestConfig{Timeout: d, FailOnTimeout: true})` (zero
    `Timeout` waits without limit, replacing `-1`).
  - Behind a reverse proxy, `server.address` and error-event URL hosts now come
    from `Host`, not `X-Forwarded-Host`: v3 reads proxy headers in
    `c.Hostname()`, `c.Scheme()`, and `c.IP()` only when
    `fiber.Config{TrustProxy: true}` and `TrustProxyConfig` trust the peer
    (`c.IP()` also needs `ProxyHeader`). Configure both to restore forwarded
    hosts; `url.scheme` still falls back to raw `X-Forwarded-Proto`, while
    `X-Forwarded-Protocol`, `X-Forwarded-Ssl`, and `X-Url-Scheme` apply only
    behind a trusted proxy.
  - Every `GET` route also answers `HEAD` through the same chain, producing
    spans and `http.server.request.duration` points under that `GET` route's
    `http.route`; set `fiber.Config{DisableHeadAutoRegister: true}` for v2
    routing.
  - `c.Body()` decodes `Content-Encoding` itself and answers 415 or 501 with
    the error text as body for unsupported codings; read `c.Request().Body()`
    for raw bytes.

  `lo.Fiber()` still copies every request value it retains, so telemetry stays
  safe with or without `Immutable`. The gateway also moves to Fiber v3.7.0 and
  fasthttp 1.78 (from v2.52.16 / 1.51), keeping `DisableHeadAutoRegister`
  (API/probe `HEAD` still returns 404) and forwarding raw request bodies
  untouched. Visible gateway changes: JSON responses now send
  `Content-Type: application/json; charset=utf-8` instead of
  `application/json`, the binary grows by ~5 MB (55.3 MB to 60.6 MB in #151's
  measurement), and RSS grows by ~2.6 MiB (default read/write buffers rise
  from 4096 to 8192 bytes, also raising max accepted header size). No endpoint,
  field, DSN, or auth change (`docs/version-matrix.md` unchanged; #151).
- The metadata baseline gained `platform_key_fingerprints`, so a metadata
  database (SQLite file or PostgreSQL database) created by an earlier commit
  refuses to start with `metadata migration 1 checksum mismatch`; recreate it
  while keeping the same key file, and the first start records the current
  fingerprint (single-baseline policy) (#60).
- A browser SDK that sends the new `vitals` signal or `server_rejected` reason
  to an older server loses the whole diagnostics batch: that server answers
  `422`, the SDK retries within the queue's attempt budget, then counts the
  whole batch in `droppedTelemetry` and warns once locally, without throwing
  into the page. On this server only the unknown entries are skipped (#61).
- Fixed metrics, logs, and traces storage to the Victoria family (ADR 0013).
  Standalone mode connects to external VictoriaMetrics, VictoriaLogs, and
  VictoriaTraces; Loki/LogQL and Tempo/TraceQL adapters are out of scope, and
  the generic Prometheus-compatible query mode (`storage.metrics.kind:
  prometheus`) is kept but no longer officially supported.
- MCP (HTTP and `lazy-observe mcp --dsn`) authenticates with a user API token;
  the DSN keeps the `/p/<slug>@<token>` shape and the slug is sent as
  `X-LO-Project`. `perf-gate scale` takes the ingest token from `LO_PERF_TOKEN`
  and a user token from `--query-token-env` (default `LO_PERF_QUERY_TOKEN`)
  plus `--project`; `perf-gate query` reads with a user token.
- Existing development databases keep plaintext tokens and sessions that no
  longer authenticate; rebuild the database or issue new tokens (pre-release
  single-baseline policy).
- The web UI keeps the selected project per browser tab, resets
  project-scoped cached results on a project switch, and returns to the login
  page when a request reports an expired session; sign-out clears every cached
  result.
- Alert rule expressions are edited with the PromQL/LogsQL code editors.
- The web build lints with Oxlint (`pnpm lint`, part of `mise run web`),
  including the React hooks rules and the type-aware
  `typescript/no-floating-promises` (#97, #111); mock data is no longer
  bundled into production builds.
- Automatic Issue AI analyses report `lazy_observe.issue_ai.automatic_outcomes`
  (completed, queue_full, rate_limited, failed) and log failure causes.

- Reworked the Logs page refresh control into a Grafana-style split button: a
  labelled Refresh action on the left and an interval dropdown on the right
  (Off, 15s, 30s, 1m, 5m, 15m, 30m), without an "Auto" entry or an "every"
  prefix.
- Removed the page-level title and description from every main content area in
  the web UI; page action buttons stay in the top-right corner.

- Moved Historical and Live Logs exploration orchestration into a headless,
  project-scoped workspace with identity-checked finite queries, bounded Live
  retries and rows, explicit stop/resume, URL normalization, Reset, and a thin
  React adapter.
- Centralized current-user login, profile, password, session, and user API-token
  behavior behind an account seam with explicit metadata/Redis session adapters,
  opaque session revocation references, current-role refresh, and observable
  cross-store failure compensation.
- Kept Ent and metadata query construction behind the metadata persistence
  seam for the Alert engine, scrape targets, and startup wiring; rule,
  silence, inhibition, and scrape-target mutations now enforce project scope.
- Development mode now disables the gateway's embedded Web UI, leaving port
  3100 API-only while Vite exclusively serves the development frontend.
- Moved CI to GitHub Actions on GitHub-hosted amd64 and native arm64 runners.
  A `changes` job selects only the jobs whose surface a diff touches and a
  `ci-ok` job aggregates them as the single required check, so
  documentation-only changes run no business jobs; arm64 build and smoke run on
  pull requests that touch the packaging chain. Container images publish to
  `ghcr.io/eimsound/lazy-observe`.
- CI and the Dockerfile builder stage now install mise from the pinned
  `v2026.5.3` GitHub release with a sha256 check instead of piping the
  unpinned `mise.run` installer, reading one `scripts/mise-release.env` for the
  version and both architecture checksums, and `mise run workflow-check` now
  rejects shell pipelines that can close early under `pipefail` (`grep -q`,
  `head`) unless the line carries `# pipefail-ok: <reason>` (#62).
- `mise run lint` and `mise run sdk-go` pass `--allow-serial-runners` to
  `golangci-lint`, so a second lint in another worktree waits for the shared
  lock instead of exiting with `parallel golangci-lint is running` (#64).
- Upgraded the embedded Victoria components to the latest stable releases that
  were at least 7 days old on 2026-11-19: VictoriaMetrics and vmagent
  v1.147.0 → v1.156.0, VictoriaLogs v1.52.0 → v1.53.0, and VictoriaTraces
  v0.9.4 → v0.13.0 (community editions only). Data directories and vmagent
  queues written by the old versions open and work with the new binaries;
  downgrading after the new versions have written data was not tested. For
  deployers:
  - From v0.12 on, VictoriaTraces' 30-second `-search.latencyOffset` default
    also applies to LogsQL queries, which the LLM statistics page uses. The
    supervised VictoriaTraces now runs with `-search.latencyOffset=0s`, so new
    spans no longer wait 30 seconds to become queryable, and a trace that is
    still being written may show up incomplete.
  - The gateway now sends `disable_latency_offset=true` with its LogsQL
    queries to VictoriaTraces, so the LLM statistics page is current on an
    external VictoriaTraces (or a cluster's `vtselect`) v0.12+ without any
    flag. The Jaeger trace views have no such per-request switch: on an
    external VictoriaTraces in Standalone or HA mode, lower
    `-search.latencyOffset` (trace list, default 30 s) and
    `-insert.indexFlushInterval` (trace detail, default 20 s, on the single
    node or `vtinsert`) yourself if you need fresh traces there
    (`docs/deployment.md`; #101).
  - Single-node VictoriaMetrics v1.154 and later answer the labels,
    label-values, and series APIs (used by query completion and AI schema
    collection) from the per-day index only. Long time ranges are slower (2–3
    times in a 30,000-series, 90-day test, still tens of milliseconds);
    upstream warns of much higher memory use at larger series counts and
    retention (#72).
- Dependency upgrades have a minimum release age of 7 days, kept in
  `scripts/release-age.env`. #70 first applied it to the versions pinned by
  hand in scripts (the Victoria storage binaries, the e2e cluster images, the
  bootstrap mise release, and Trivy) with a `pinned-releases` CI job that failed
  a pull request whose pin pointed at a release younger than that, a prerelease,
  or a draft, plus a weekly `pinned-releases` workflow with a tracking issue, and
  with Dependabot's `cooldown`. #130 (ADR 0015) removed all three: Renovate now
  opens the upgrade PRs and waits 7 days after a release (`minimumReleaseAge` in
  `.github/renovate.json`, security updates included), and
  `scripts/check-github-workflows.sh` fails when that value, or the pnpm and uv
  settings below, differ from `scripts/release-age.env`. The age gate therefore
  only covers PRs Renovate opens: a pin changed by hand gets no age check, and
  the dependencies whose registry reports no release time are exempt (the Quay
  Victoria cluster images, GCR distroless, and `govulncheck`). `seaweedfs`,
  `postgres`, and the Dockerfile's `debian` stay on that exemption list only as
  a fallback: the hosted Renovate does read Docker Hub release times for them
  and waits the 7 days, and only a lookup that returns no time lets a new
  version through at once. The `minimumReleaseAge` of
  the `web`, `sdk/node`, and `sdk/browser` pnpm workspaces uses the same 7 days,
  so pnpm also holds back a security patch younger than that until it is listed
  in `minimumReleaseAgeExclude`. `web` is now a single-package pnpm workspace
  (`pnpm-workspace.yaml`); the lockfile is unchanged. `sdk/python` follows with
  `[tool.uv] exclude-newer = "7 days"` in its `pyproject.toml` (#70, #119,
  #130, #138).
- `mise run sdk-journeys` runs the Go, Node, Python, and browser SDK journeys
  (`sdk/*/e2e.sh`) against a freshly started local platform, and changes that
  touch server, SDK, web, or storage-download paths now run it as the
  `SDK journeys` CI job, which `ci-ok` waits for. The browser
  `performance-journey` was waiting for a heading the Performance page does not
  have and now waits for its Web Vitals tab and breadcrumb; the web page is
  unchanged. `sdk/browser/e2e.sh` installs Playwright system dependencies only
  as root or with passwordless `sudo`. The browser and Python SDK READMEs,
  which since #9 called the journeys manual-only, now point to this task (#68).
- The PostgreSQL metadata tests (`mise run test-meta-pg`, test-only: not linked
  into the production binary) cache the downloaded PostgreSQL distribution in
  `LO_TEST_PG_CACHE` (default `$HOME/.cache/embedded-postgres`, also cached by
  CI) and report the real HTTP status on failure instead of
  `no version found matching 18.3.0` (#66). #133 added Maven Central retries
  and mandatory sha256 checks; embedded-postgres v2.0.1 (#140) replaced that
  prefetch and the Maven source. The tests now download PostgreSQL 18.6.1 (was
  18.3.0) from `theseus-rs/postgresql-binaries` GitHub releases, trying each
  request up to 5 times (2, 4, 8, and 16 seconds apart, honoring `Retry-After`
  up to 30 seconds) on 403, 404, 408, 429, 5xx, and network errors, verified
  against the library's built-in sha256 manifest (pinned by `go.sum`);
  persistent failures fail with the HTTP status and never skip. The unpacked
  cache is ~117 MB (~37 MB in the CI cache, up from a ~14–15 MB archive) and
  needs no network when warm. The supervisor is built with `go build` from the
  `go.sum`-pinned module source rather than downloaded (#133, #140).
- The bootstrap `mise` in `scripts/mise-release.env` (read by
  `scripts/ci-setup-mise.sh` and the Dockerfile builder stage) is upgraded from
  v2026.5.3 to v2026.11.7. A change to that file now runs every CI job, as a
  change to `mise.toml` or the Dockerfile does, instead of only the Go checks
  (#82).
- Interrupting `scripts/sdk-journeys.sh` (`TERM`, `INT`, or `HUP`) now stops
  the platform and the running journey by process group, so example servers,
  builds, and browsers no longer outlive the run, and a journey's temporary
  directory is removed even when the journey is killed; processes a finished
  journey leaves behind are stopped with a warning. The `SDK journeys` CI job
  runs only the journeys a change can affect (for example, `sdk/python/**` runs
  only the Python journey; the full list is under "SDK journey scope" in
  `CONTRIBUTING.md`) and still runs all four for server, CI, and unclassified
  paths. `LO_SDK_JOURNEYS_ONLY` also accepts a space-separated list, and the
  journey gateway port range moved from 44000–44999 to 14000–14999, below the
  Linux ephemeral port range (#83).
- The web console is built with Vite 8 (Rolldown) instead of Vite 6. The
  `uplot`, `replay-engine`, `promql`, and `vendor` chunks are kept as Rolldown
  `codeSplitting` groups, plus one group for the remaining third-party code of
  the first load. No `build.target` is set, so the default target moves from
  Vite 6's `modules` (Chrome 87, Edge 88, Firefox 78, Safari 14) to Vite 8's
  `baseline-widely-available` (Chrome and Edge 111, Firefox 114, Safari and iOS
  Safari 16.4): older browsers may no longer load the console. CSS is minified
  with lightningcss instead of esbuild (MPL-2.0, build time only, not part of
  the shipped application) (#85).
- `@lazy-observe/node` now depends on Pino ^10.4 (was ^9) for `lo.pino()`.
  Pino 10's only breaking change is dropping Node 18, which the SDK's
  `>= 22.15` requirement already excludes; the SDK's code is unchanged, and the
  options `lo.pino()` accepts and the `Logger` type it returns now come from
  Pino 10 (#89).
- The SDK packages (`@lazy-observe/node`, `@lazy-observe/browser`,
  `@lazy-observe/react`, `@lazy-observe/bundler-plugin`) are built with tsdown
  instead of tsup, and the web console and the SDKs move to TypeScript 7. The
  published entries, ESM and CJS export names, external dependencies, and
  declaration symbols are unchanged; the emitted JavaScript differs (Node
  built-ins are imported with the `node:` prefix, and CJS exports carry a
  `Symbol.toStringTag` of `"Module"`; `@lazy-observe/bundler-plugin` has
  since become ESM only, see #112). The web console lints
  with Oxlint instead of ESLint and runs its unit tests on Vitest instead of
  `node --test`, and `mise run sdk-browser` now also runs the bundler plugin's
  tests (#97).
- `mise run sdk-python` builds its virtual environment with uv (a
  uv-managed CPython, no pip) and runs the Python SDK tests on the interpreter
  named by `LO_SDK_PYTHON_VERSION` (default 3.13; for example `3.9` or
  `3.15t`); CI runs it on 3.9, 3.13, 3.15, and 3.15t (#103).
- The Python SDK adopts the repository's 7-day minimum release age:
  `sdk/python/pyproject.toml` sets `[tool.uv] exclude-newer = "7 days"`, which
  limits what `uv pip install` and `uv build` (for the build backend) resolve
  to package versions that have been on PyPI for at least 7 days, and
  `scripts/check-github-workflows.sh` fails when it differs from
  `scripts/release-age.env`. Without a lockfile the window is relative to the
  current time, so resolution moves forward as time passes. The PyPI release
  path (`sdk-release.yml`, `sdk-python-v*` tags) no longer uses `setup-python`
  or pip: it installs only uv through mise and runs `mise run
  sdk-python-release`, which builds the sdist and wheel with `uv build
  --managed-python --python 3.13`; the artifacts are the same as before. The
  upload step and its trusted-publishing permission are unchanged. The new
  release path has not run through a real `workflow_dispatch` dry run or tag
  push yet, so run a dry run before the first release (#119).
- CI runs `sdk-node` on Node 22.23.5, 24.22.3, and 26.13.1, and `sdk-browser`
  on 26.13.1 for the full suite plus a bundler-plugin load smoke test on
  22.23.5 and 24.22.3 (`mise run sdk-browser-smoke`); `LO_SDK_NODE_VERSION`
  selects the Node version for these three tasks. The checks are named `Node
  SDK (Node <version>)` and `Browser SDK (Node <version>)`; `ci-ok` still waits
  for all of them (#112).
- The Go SDK's `Shutdown` no longer uploads a CPU profile window it cut short
  unless the window had already run for 5 seconds; a shorter one is discarded,
  and the discard is not counted in `DroppedTelemetry`, the same rule as the
  Python SDK. The heap and goroutine snapshots are still collected, and a
  window that runs its full 10 seconds is not affected (#118).
- Dependency upgrades are handled by Renovate instead of Dependabot and the
  hand-written pin tracking (ADR 0015, #130). Renovate runs as the Mend
  Renovate GitHub App, which has to be installed on the repository
  (installation steps in `.agents/docs/plans/renovate.md`).
  `.github/renovate.json` covers the manifests (Go modules including
  `entc.mod`, the pnpm workspaces, the Python SDK's `pyproject.toml`, GitHub
  Actions, the Dockerfile, mise tools) and the versions pinned by hand in
  scripts (the Victoria storage binaries, Trivy, the bootstrap mise release,
  the e2e and BuildKit images). Most updates are grouped; the global schedule
  is `at any time` (changed from Mondays after 3 am Asia/Shanghai in #139).
  Where a pin carries a sha256, Renovate updates the version and digest in one
  PR using `# <release tag>` markers on `scripts/mise-release.env` and
  `scripts/download-storage-bins.sh`. Trivy is the exception (#138): hosted
  Renovate hit `lookupUpdates error` on `aquasecurity/trivy`, so
  `scripts/install-trivy.sh` sha lines carry no `# <tag>` comment, Renovate
  only updates `trivy_version`, and the two sha256 values are filled in from
  `trivy_<version>_checksums.txt`. `.github/dependabot.yml`,
  `.github/workflows/pinned-releases.yml`, the `pinned-releases` CI job, and
  `scripts/check-pinned-releases.sh` (with its test and mise task) are
  deleted. A new `trivy-pin` CI job downloads Trivy on amd64 and arm64 and
  checks its sha256 whenever `scripts/install-trivy.sh` changes (`ci-ok` waits
  for it, so a Renovate Trivy PR stays red until the digests are filled in);
  Trivy upgrades rely on the 7-day age gate and sha256 check rather than a
  manual `gh release verify`. #138 also enabled `gomodUpdateImportPaths` for Go
  major upgrades (majors needing code changes are still migrated in dedicated
  PRs, as in #140 and #151), split `go` in `mise.toml` into `renovate/mise-go`,
  and grouped `github.com/google/pprof` digest bumps with Go minor/patch
  updates. Remaining manual steps before merging Renovate PRs (commands in
  `.agents/docs/plans/renovate.md`): regenerate
  `third_party/go/THIRD_PARTY_NOTICES.txt` (`mise run third-party-notices`)
  when binary Go dependencies change; update `third_party/victoria/SOURCES.txt`
  on Victoria bumps; fill `entc.sum` on `ent` group PRs (which also include
  `golang.org/x/tools` in `entc.mod`, #152); and rebase workflow-touching
  Renovate PRs onto `main` (tick rebase/retry) when the merge token lacks
  `workflow` scope. mise tool versions are now written once in `mise.toml`
  (#134), so mise updates need no manual sync.
- `internal/db/ent`, the generated Ent client, is now tracked in git (it used
  to be ignored and generated by every mise task, #128). Renovate clones the
  repository without generated code, so `go get` for the root module could not
  resolve the Ent packages. For contributors: after changing
  `internal/db/schema`, run `mise run ent-generate` and commit the regenerated
  `internal/db/ent` together with the schema; do not edit the tree by hand.
  To run `mise run ent-generate-check` locally before committing, stage the
  tree first (`git add internal/db/ent`), since it compares against the index.
  `mise run build`, `test`, `vet`, `lint`, `license-check`, and the other
  tasks no longer regenerate it, and the Air development build no longer does
  either, so a schema change is not picked up until you regenerate. The files
  are marked `linguist-generated`, so GitHub collapses them in PR diffs, and
  `mise run ent-generate-check`, which CI runs, now fails when the tracked
  tree differs from what the schema generates (edited, missing, extra, or
  ignored files, or a schema change without regeneration) instead of
  requiring that the directory stays untracked.
- The gate scripts that embedded Python (`scripts/check-release-smoke.sh`,
  `scripts/check-embedded-artifact.sh`, and `scripts/e2e-multi-replica.sh`)
  no longer use the system `python3`. They install and use a uv-managed
  CPython (`scripts/lib/uv-python.sh`), `3.13` unless
  `LO_SCRIPTS_PYTHON_VERSION` selects another. A missing `uv` or a failed
  install fails the gate rather than skipping it, and the release smoke's
  `PYTHON` override is gone. Run them through their mise tasks
  (`dist-cross`, `e2e-ha`, `perf-ha`, `gate-release-artifacts`), which provide
  uv (#125, #127).
- The SDK journey examples listen on fixed ports between the gateway range and
  the platform components instead of in the Linux ephemeral range
  (32768–60999), where `vite preview` occasionally found its port taken: Go
  16320 and 16330, Node 16420, 16421, and 16430, Python 16620, and the browser
  examples in 16520–16550. The `LO_E2E_*_PORT` variables still override them,
  and the browser journey's `vite` and `vite preview` now run with
  `--strictPort`, so a busy port fails instead of silently moving. The
  browser journey and the `sdk-browser` and `sdk-browser-smoke` tasks install
  and build `@lazy-observe/bundler-plugin` before the full `pnpm install`,
  which removes the `Failed to create bin … lazy-observe-sourcemaps` warning
  (#124).
- Public preview wording reaches the GitHub Release page: for `v0.x` tags
  GoReleaser puts the same preview notice as the README, the deployment guide,
  and the Unreleased section at the top of the release body (`release.header`,
  not rendered from v1.0.0), because that body is generated from `git log` and
  never contains this file. The documentation now separates "released" (a tag
  with artifacts) from "supported" (an upgrade path, with the database
  baseline frozen and append-only migrations), and the first supported
  release is not v0.1.0 (#126).
- Tool versions are written once, in `mise.toml` (#134). Per-task `tools.*`
  entries are gone, and `scripts/ci-setup-mise.sh` profiles plus `ci.yml`
  `mise x` calls name tools without versions (`uv`, not `uv@0.13.7`), so
  Renovate updates every caller via `mise.toml`. `uv`, `golangci-lint`, `helm`,
  `goreleaser`, `go-licenses`, and `govulncheck` are `lazy = true` (installed
  on first task/command use, not by a bare `mise install` or `mise run`). For
  contributors, the minimum mise version is now 2026.10.13 (was 2025.8.4;
  `README.md`, `README.zh-CN.md`, and `CONTRIBUTING.md` updated); older mise
  refuses to load the config, while CI and Docker bootstrap v2026.11.7.
  `scripts/ci-setup-mise.sh` verifies pins before and after install: bare tool
  names must resolve to exact versions in this repo's `mise.toml` (rejecting
  unconfigured tools, `MISE_<TOOL>_VERSION`, `mise.local.toml`, and global
  entries), profiles reject `name@version`, and `MISE_CI_TOOLS` allows exact
  `name@version` overrides (for example the Node matrix) while rejecting
  non-exact or duplicate entries.
- The toolchain is Go 1.27.4 (was 1.26.11; `mise.toml`, #152); `go` directives
  in `go.mod` and `entc.mod` (1.26.11) and `sdk/go/go.mod` (1.26.0) are
  unchanged. Go 1.27's `gofmt` indents composite literals inside multi-line
  `return` statements differently, so `internal/ai/stream.go` was reformatted
  (3 lines, `golines -w`; Go 1.26's `gofmt` reverts that indentation). Because
  a `go:` tool in `mise.toml` is compiled by the Go active at install time and
  a Go 1.26 build fails on the Go 1.27 standard library (for example
  `govulncheck` fails with `package requires newer Go version go1.27
  (application built with go1.26)`), `scripts/ci-setup-mise.sh` runs
  `mise install --force <tool>` whenever a `go:` tool's build Go differs from
  the current Go; local copies can be rebuilt the same way. `entc.mod` pins
  `golang.org/x/tools` v0.52.0 (indirect) because Ent v0.14.6's default v0.30.0
  failed `ent-generate-check` on Go 1.27 with `package "context" without types`;
  the generated `internal/db/ent` is unchanged.
- The web, Node SDK, and browser SDK workspaces use pnpm 12.20.0 (was 10.33.0;
  `mise.toml` and each `packageManager` field, #149), and each `pnpm-lock.yaml`
  now opens with a document recording pnpm itself. Because pnpm 11+ ignores
  `onlyBuiltDependencies` and fails with `ERR_PNPM_IGNORED_BUILDS` on
  unreviewed install scripts, `sdk/browser/pnpm-workspace.yaml` switches to
  `allowBuilds: {esbuild: true}` and `sdk/node/pnpm-workspace.yaml` sets
  `allowBuilds: {esbuild: false}` (preserving the old ignored behavior, since
  esbuild uses its platform binary package). Because pnpm 12 `pnpm licenses
  list` only checks the root package at a workspace root,
  `scripts/check-js-licenses.sh` now passes `--recursive`; compared with pnpm
  10, production lists for `sdk/browser` (17) and `sdk/node` (103) are
  identical, while `web` drops 5 `devDependencies` entries (type packages and
  `typescript`).
- Other development tools moved up through Renovate (#145, #147, #148, #150):
  `helm` 4.3.2 (was 3.21.3; `mise run helm-lint`, `helm-template-test`, and CI
  Helm checks pass with the chart and `docs/deployment.md` unchanged), `rg`
  15.2.0 (was 14.1.1), `air` 1.67.4, `golines` 0.16.1, `uv` 0.13.8,
  `golangci-lint` 2.15.0, `goreleaser` 2.20.2, and `govulncheck` 1.10.0. No
  source files needed changes.
- Server third-party Go modules moved to newer minor and patch releases through
  Renovate (#144, #142): pgx 5.13.0 (was 5.10.0), AWS SDK for Go v2
  (`aws-sdk-go-v2` 1.48.0, S3 1.114.10), MCP Go SDK 1.8.0 (was 1.6.1),
  Prometheus 0.316.0 with `prometheus/common` 0.74.0, go-redis 9.23.0, grpc
  1.84.0, `golang.org/x/crypto` 0.59.0, `pflag` 1.1.0, and
  `go.opentelemetry.io/proto/otlp` 1.11.1 (was 1.11.0). Only the root module's
  `go.mod`, `go.sum`, and `third_party/go/THIRD_PARTY_NOTICES.txt` changed (not
  `sdk/go`, configuration, endpoints, or schema); regenerated notices add
  `pandatix/go-abnf` (MIT), `golang.org/x/time/rate` (BSD-3-Clause), and Go
  standard library code copied inside `smithy-go` (BSD-3-Clause).
- Made release archives consume the canonical root `LICENSE` file.
- Made PostgreSQL the correctness boundary for multi-replica alert membership,
  per-rule leases/fencing, durable state, and notification Outbox work; Redis
  now remains optional for shared sessions and rate limits.
- Reduced release payloads and the all-in-one process topology from five
  managed executables to VictoriaMetrics, VictoriaLogs, VictoriaTraces, and
  vmagent.
- Consolidated every pre-release metadata schema change into one complete
  SQLite baseline and one complete PostgreSQL baseline. The single marker row
  records its name, SHA-256 checksum, application version, and duration.
- Serialized PostgreSQL metadata startup with a database-specific advisory
  lock held on one reserved pgx session, bounded lock waiting, post-lock
  baseline-marker reads and fail-closed cleanup.
- Made reviewed SQLite/PostgreSQL baseline SQL the only runtime metadata schema path, and added reproducible Ent generation plus a production Go dependency license allowlist gate.
- Expanded static workflow validation and contributor checks to cover Go SDK, release, query, SDK-overhead, and HA capacity gates.
- Hardened Linux amd64/arm64 release preparation by pinning the generic ISA baselines, gating the exact prebuilt bytes before GoReleaser imports them, and stripping forge credentials from delegated build tools.
- Enforced paired single-baseline metadata schemas through a local mise task used by CI and the release entry point.
- Replaced Make with version-pinned mise tasks across local development, automation, Docker builds, and project documentation.
- The metadata baseline SQL changed again: `users` gained
  `must_change_password`, so pre-release development databases must be rebuilt
  (single-baseline policy) (#18).
- The metadata baseline SQL (SQLite and PostgreSQL) changed once more: it gained
  the `source_map_upload_tokens` table, added directly to
  `001_baseline.sql` with no incremental migration (ADR 0003), so
  pre-release development databases must be rebuilt (#116).
- `@lazy-observe/node` now requires Node >= 22.15 and ships OpenTelemetry JS
  3.x (`@opentelemetry/sdk-trace` replaces `sdk-trace-node`, the OTLP proto
  exporters are 0.301.2, `api-logs` is folded into `api`), with the span and
  log processor constructors rewritten accordingly (#32, #115).
- The Go SDK now requires OpenTelemetry Go 1.45 (logs 0.21) and emits the
  matching v1.43 semconv schema URL; with the old v1.41 URL, an application on
  OTel Go 1.45 failed to create the client on a schema-URL conflict (#11).
- The server built with Go 1.26.11 and picked up the govulncheck-reported fixes
  in grpc 1.83.2, x/net 0.62, brotli 1.2.7, and klauspost/compress 1.18.7; the
  `sdk/go` manifest followed the same OpenTelemetry Go 1.45 upgrade (#11). The
  toolchain is now Go 1.27.4 and the server module is on grpc 1.84.0 and
  klauspost/compress 1.20.3, and brotli is no longer a dependency (it came with
  Fiber v2; #144, #151, #152).
- Container images build on Debian 13 (trixie) instead of Debian 12 (bookworm)
  and run on `gcr.io/distroless/static-debian13:nonroot`, and the Dockerfile
  frontend plus every base image are pinned to a multi-platform index digest.
  The multi-replica test services drop their Alpine variants (VictoriaMetrics
  uses the `-cluster-scratch` images); only the SeaweedFS fixture stays Alpine,
  because upstream publishes it only that way (#4).
- CI runs `govulncheck` (`mise run vulncheck`, covering the server module and
  `sdk/go`) as a required gate, pins BuildKit to a multi-architecture digest,
  scans the pre-release image with Trivy before the archive and image release,
  rejects unpinned workflow references, and kept Dependabot on every
  dependency manifest (#11; Dependabot has since been replaced by Renovate,
  #130).
- Pull requests that touch server Go code now run the multi-replica HA
  functional gate (`mise run e2e-ha`); web, Helm, non-Go SDK, and documentation
  changes do not open it, and the performance gates stay out of PR CI (#2).
- The Go SDK nested module is verified tidy (`go mod tidy -diff`) and then
  built, vetted, linted with `golangci-lint`, and tested by `mise run sdk-go`,
  with the existing deprecated and unchecked-error uses fixed (#51, #53).
- The browser SDK now depends on `web-vitals` ^6; the reported metric set
  (LCP, CLS, INP, TTFB, FCP), the `name`/`rating` values, and the rating
  thresholds are unchanged (#44).
- Upgraded the shared toolchain and third-party dependencies: Node 26.13.1 LTS
  and `@types/node` ^26 repo-wide (#31), Vite 8.3.8 in the browser SDK examples
  (#43), `@tanstack/react-table` 9.3.1 with v8 sort parity preserved (#39),
  `@tanstack/react-router` ^1.170.46 (the JS license allowlist now also accepts
  Unlicense, needed by its SSR-only `isbot`; #40), react-i18next 18 (#47),
  i18next 27 (#25), `lucide-react` 1.72 (#45), `tailwind-merge` 3.7 (#26),
  `@vitejs/plugin-react` 5.2 (#46), and `tsup` 9 (#17), plus grouped
  minor/patch bumps for GitHub Actions, the web workspace, and the SDK Go and
  JS manifests (#13, #14, #15, #22).

### Removed

- Removed the built-in dashboards and host middleware views. Dashboards are
  delegated to external Grafana or Perses through the Prometheus-compatible
  query API (ADR 0017).
- Removed the cross-repository SDK plumbing: the sibling-directory `replace`,
  the CI deploy key and SDK checkout scripts, the release-time SDK alignment
  gate, and the SDK-side server compatibility build.
- Removed the unused `alert_rule_shadow_comparisons` table and its metadata
  API, and the unused `@tanstack/react-virtual` and
  `@radix-ui/react-radio-group` web dependencies.
- Removed Darwin release targets; production archives and embedded storage payloads target Linux amd64/arm64.
- Removed legacy short configuration environment variables, `replay.retention`, and `--replay-retention`; canonical path-shaped configuration is now the only accepted interface.
- Removed the obsolete public `/select/logsql/query` PoC alias; clients use `/api/v1/logs/query`.
- Removed the unused in-memory alert `ChannelDispatcher`; durable notification delivery remains owned by the Outbox worker.
- Removed unused in-memory and Redis alert deduplication/lease abstractions; future Issue AI leases will be introduced with task-specific renewal and takeover semantics.
- Removed the Homebrew formula, tap publication workflow, and package-manager
  distribution plan; release delivery remains archives, containers, the
  installation script, and Helm.
- Removed `vmalert` supervision and embedded payloads, generated rule files and
  reloads, private alert datasource/webhook routes, scheduled-alert Redis
  deduplication/notification leases, and the legacy `vmalert` API option.

### Fixed

- With native TLS enabled, MCP tool calls and self-monitoring export reach the
  gateway through an internal plaintext loopback listener instead of speaking
  HTTP to the TLS listener.
- Proxied gzip OTLP requests no longer reach storage with a stale
  `Content-Encoding: gzip` header over an already-decoded body.
- Managed storage components receive SIGTERM when the gateway dies, so a
  killed gateway no longer leaves orphan VictoriaMetrics processes holding
  ports (Linux).
- Source map retention now reclaims content-addressed revisions that no
  release or pending error event references; a release named `revisions` no
  longer deletes the project's entire source map store.
- Logout reports 503 and keeps the session cookie when the server-side session
  could not be deleted, instead of answering 204.
- Connected Go/Fiber panic and returned 5xx errors to the real Issue ingest
  pipeline with trace-correlated stacks, non-blocking batching, explicit
  `CaptureException`, privacy-bounded request metadata, and end-to-end coverage.
- Fixed Cobra server and doctor commands silently dropping explicit
  configuration flags after merging their flag sets.
- Normalized silence writes and evaluator comparisons to UTC, and migrated
  legacy SQLite offset timestamps, preventing active silences from being
  missed after native alert evaluation replaced the previous runtime.
- Serialized PostgreSQL first-run admin, default-project, and self-monitoring
  bootstrap with a separate database advisory lock, preventing duplicate or
  conflicting seed state when application replicas start together on an empty
  database.
- Isolated fixed HA listeners from Linux ephemeral ports and moved the ephemeral SeaweedFS fixture onto a bounded tmpfs with smaller mini volumes, preventing TIME_WAIT collisions and host-disk free-space failures.
- Made GoReleaser before hooks locate the mise executable through the configured local bin directory plus the inherited tool PATH, so snapshots also work from non-activated shells.
- Required a configured majority of capacity-scaling rounds to pass paired metric and log efficiency together while retaining an independent target-balance floor, preventing an even-round aggregate median or relaxed efficiency budget from hiding weak-scaling regressions.
- Started scaling-gate phase timing only after all request workers are ready and hardened its short unit-test windows, preventing scheduler load from producing empty, flaky failure diagnostics.
- Replaced the Overview page's recent-Issues placeholder with the real project-scoped open-Issues query, including loading, retryable error, empty, three-item, and detail-navigation states.
- Prevented project-API-token cache stampedes by coalescing same-token lookups, preserving cross-token concurrency and revision invalidation, and distinguishing invalid credentials (401) from transient metadata failures (503).
- Preserved singleton upstream response headers and enlarged the gateway's cloned HTTP idle pool, eliminating invalid proxied `Content-Type` values and connection churn under concurrent OTLP batches.
- Removed the real performance page's hard-coded mock page list by discovering Web Vitals pages through the project-isolated series API, with explicit loading, empty, error, and sparse-data handling.
- Aligned scrape-target and per-project alert-configuration APIs with the real web contract, using explicit public DTOs, string IDs, stable health timestamps, and compatible channel-ID input.
- Aligned Issues list, detail, and event responses with the real web contract, including pagination totals, epoch-millisecond timestamps, project-scoped trends, safe event contexts, and hidden metadata internals.
- Cleared the Go lint gate by handling HA doctor output errors and removing redundant interface type declarations.
- Aligned alert rule, channel, silence, and event DTOs with the real web contract, including stable string IDs, SMTP compatibility, creators, rule names, and non-null collections.
- Aligned project and project-API-token management with the real web contract, including project CRUD, named one-time token secrets, revocation state, throttled last-used tracking, and the consolidated metadata baseline.
- Aligned continuous-profile API envelopes and time/ID types with the flamegraph UI, and restored profile-to-trace navigation without exposing storage metadata.
- Aligned the real Session Replay API with the web player contract, including event-time ranges, pagination totals, segment metadata, and rrweb custom error markers.
- Ignored and filtered macOS AppleDouble `._*` sidecar files from database migrations and Vitest discovery, preventing JSON panics, invalid migration parsing, and binary test collection failures.
- `LO_SECURITY_SECRET_KEY_FILE` now sets `security.secret_key_file`; the
  example configuration documented `LO_SECRET_KEY_FILE`, which nothing
  accepted, so the platform key file could only be configured in YAML (#50).
- `lazy-observe mcp --dsn` validates the token at startup: a project token
  (`lo_…`) or a legacy token is rejected with a message pointing at the
  personal API token page instead of 403 on the first tool call, and `--help`
  states that the DSN needs a user API token and sends the slug as
  `X-LO-Project` (#7).
- `perf-gate query` defaults `--token-env` to `LO_PERF_QUERY_TOKEN`, so
  `LO_PERF_TOKEN` no longer means the ingest token in `scale` and the read
  token in `query` (#8).
- The browser and Python SDK READMEs no longer claim their journeys run under
  `mise run e2e` or CI; they document the manual run against a running
  platform, the required variables, and the session-cookie credentials reads
  need (#9).
- Workflow, container-smoke, and `mise run release` pipelines no longer report
  spurious SIGPIPE failures under `pipefail`: commands that consume a
  producer's output early now collect it first (#10).
- Multi-replica `standalone-ha` deployments no longer give every replica its
  own platform encryption key, so one replica (or a restarted pod) can decrypt
  sealed settings such as the AI provider API key that another replica wrote
  (#3).
- Web Vitals ingest no longer rejects a whole batch because one metric name is
  unknown to this server: the entry is skipped, the known metrics in the batch
  are written, the response reports `{accepted, rejected}` instead of
  `{accepted}`, and one warning per request names the skipped values
  (truncated, at most five distinct names plus an overflow count), while a
  missing `page`, an empty name, or an invalid timestamp still answers `422`
  for the whole batch. A batch with nothing left to write no longer sends an
  empty write upstream (#55).
- A failed `admin reset-password` no longer leaves a password that never took
  effect in the generated `<data_dir>/initial-admin-password`: a file that
  existed before the run is restored byte for byte in mode 0600, a file the
  run created is removed, and a failure after the new password was stored
  keeps the new file, which is the only copy that works. A pre-existing file
  above 4 KiB is refused instead of overwritten, a failed restore names the
  path and asks for a rerun, and the CLI audit event now also records the
  target user ID and role (#58).
- The browser SDK counts a 408 or 429 answer to a Web Vital as
  `retry_exhausted` rather than `server_rejected`, because the same payload
  may be accepted later; 400, 404, and 422 remain `server_rejected` (#61).
- The minimum-release-age gate for hand-pinned versions no longer fails when a
  repository answers the authenticated GitHub API with a non-rate-limit 403,
  such as an organization IP allow list rejecting `GITHUB_TOKEN`; the earlier
  fallback, an anonymous API request, shares the runner's rate-limited IP and
  often failed too. The gate now reads the release time and the pre-release
  label from the repository's `github.com` release page, falls back to the
  anonymous API only if that fails, and still exits 2 when every source fails
  or a rate limit is hit. For such a repository the weekly report marks the
  pin `unknown`, not outdated, when the release page cannot show that the
  latest release is old enough, and `--latest-eligible` still exits 2 (#86).
  The release-time storage-binary scan no longer calls `--latest-eligible`
  (ADR 0016, #117), and #130 later deleted the gate, its weekly report, and
  `--latest-eligible` altogether when Renovate took over (ADR 0015).
- `lazyobserve.Client.shutdown(timeout)` in the Python SDK no longer returns a
  spurious `... has no attribute 'shutdown'` error for a signal that is not
  enabled (or on a degraded client). Its steps (error transport, profiling,
  then the log, trace, and metric providers) now share one `timeout` deadline
  instead of each waiting on its own, so `shutdown` returns within `timeout`;
  a step still running at the deadline is left to finish in the background
  and reported as `TimeoutError("<step> shutdown: budget exhausted")`. During
  interpreter finalization (for example from `__del__`) no thread can be
  started, so the steps run inline and `timeout` does not bound them (#102).
- The Python SDK's error-event delivery recovers in a forked child process:
  when the SDK was initialized before the fork (gunicorn `--preload`, uWSGI),
  `capture_exception` in a worker used to return normally while the event was
  never sent, because the child inherited the queue without its sender
  thread, and `shutdown` then timed out. Each child now restarts its own
  sender; events the parent had queued stay with the parent and are not
  resent. The `Client` drop-counter lock is replaced in the child as well,
  so counting a profiling drop can no longer deadlock after a fork, and the
  child's `dropped_telemetry` starts from 0 (#104, #105).
- When the Python SDK's `shutdown` budget runs out (including `shutdown(0)`),
  the error events and profile uploads still queued are dropped at once with
  reason `shutdown_budget` and logged through the `lazyobserve` logger,
  instead of staying queued uncounted and possibly going out after `shutdown`
  returned; dropped profile uploads also count in
  `Diagnostics().dropped_telemetry` (#105), and since #108 the dropped error
  events do too.
- Error events the Python SDK drops from its delivery queue are now counted
  like the other SDKs count them: in `Diagnostics().dropped_telemetry` and in
  `lazy_observe_sdk_dropped_telemetry` with `signal="error"` and the reasons
  `queue_full`, `retry_exhausted`, and `shutdown_budget`, before the warning
  is logged. Until now these drops were only logged, so with metrics enabled a
  Python client shows new `signal="error"` series and a larger
  `dropped_telemetry` than before (#108).
- Once the Python SDK's `shutdown` budget has run out, the delivery queue for
  error events and profile uploads starts no new request. Before, a batch the
  sender thread already held still went out with its first attempt, and a
  retry went out when the cancel landed just as its delay ended, both after
  the budget was spent. That batch is now dropped; for error events it is
  counted like the events still queued, in the `Diagnostics().dropped_telemetry`
  total and as `reason="shutdown_budget"` of
  `lazy_observe_sdk_dropped_telemetry`. A request already
  in flight is not interrupted. Deliveries inside the budget, retries, and
  `retry_exhausted` are unchanged (#123).
- `@lazy-observe/bundler-plugin`'s Vite plugin no longer aborts a build with a
  raw `Command failed: git rev-parse --short=7 HEAD` when it cannot infer the
  release from Git, for example in a Docker build that excludes `.git`, and no
  `release` option is set. Without a DSN (or with `upload: false`, or under
  `vite dev`) the build now continues, nothing is uploaded, and
  `__LAZY_OBSERVE_RELEASE__` is defined as `undefined`, so events carry no
  release; declare it as `string | undefined`. With a DSN
  and no `release` that Git cannot supply, `vite build` fails before bundling
  with `unable to infer release from git; set release explicitly`, so pass
  `release` (for example `release: process.env.RELEASE`). The CLI already
  skipped without a DSN; with a DSN and no release it now exits 1 with the
  same message (#122).

## Pre-release history - 2026-07-16 to 2026-07-17

The private pre-release history was squashed into this repository's initial
commit; the internal milestone tags it used were never published.

### Added before the public repository

- Repository and requirements baseline; Victoria component supervision proof of concept; project-isolation verification for VictoriaMetrics, VictoriaLogs, and VictoriaTraces; Fiber gateway, SQLite/ent metadata skeleton, and the initial bilingual themed web shell.
- User sessions, admin/member/viewer RBAC, projects, project API tokens, and logs/traces query passthrough.
- Go SDK with Fiber, slog, database/sql, runtime metrics, and OTLP/HTTP export.
- Metrics, logs, and trace UI, including log-to-trace navigation and trace waterfall views.
- Metric rules, grouping, silences, and webhook/Feishu/DingTalk/WeCom/email notifications.
- Login, project/token/user management, alert management, and overview UI.
- Browser and React SDKs for error capture, Web Vitals, trace propagation, Session Replay, ErrorBoundary, and route tracking.
- Node/Express SDK with Pino/Winston logging, runtime metrics, traces, and error events.
- Error ingestion, stack fingerprinting, Issue aggregation/lifecycle, and Web Vitals ingestion.
- Source-map upload, storage, symbolication, and Vite/Webpack bundler plugin.
- Replay segment storage with local/S3 abstraction and replay UI; Issues, replay, and performance pages.
- Platform event alerts for new Issues, regressions, and error-rate spikes.
- Prometheus exporter scrape management through supervised vmagent and host views.
- Alert inhibition, storm merging, and channel rate limiting.
- Standalone external-storage mode and unified YAML/environment/flag configuration.
- Global signal retention and cleanup for replay, source maps, alert events, profiles, and closed Issues.
- Go continuous CPU/heap profiling, pprof blob storage, profile query APIs, and flame-graph UI.
- Read-only MCP HTTP server and stdio proxy.
- Optional PostgreSQL metadata backend with SQLite/PostgreSQL migration parity.
- BYO LLM access through OpenAI- and Anthropic-compatible APIs and Ollama, with configuration and connectivity checks.
- AI-assisted natural-language MetricsQL/LogsQL generation, isolated query execution, and chart explanations.
- Cross-signal Issue AI analysis with structured root-cause and remediation hints.
- OpenTelemetry GenAI tracing in the Go and Node SDKs, plus span aggregation, model pricing, and cost reporting.
- Continuous Node.js CPU and heap profiling through the V8 inspector with pprof-compatible uploads.
- Bilingual project documentation, architecture/deployment/SDK guides, contribution guidance, and issue/PR templates.
- Redis-backed sessions and rate limiting, multi-replica-safe Issue counting, and `doctor --ha` readiness checks.
- VictoriaMetrics, VictoriaLogs, and VictoriaTraces cluster integration with tenant routing.
- Helm deployment in all-in-one and standalone-HA modes, with liveness/readiness probes, graceful draining, HPA, PDB, and multi-replica E2E coverage.
- Production `go:embed` packaging for the frontend and storage executables, enabling zero-argument single-binary startup.
- GoReleaser archives, container image packaging, and an install script.
- Built-in self-monitoring and an ingest performance benchmark.
