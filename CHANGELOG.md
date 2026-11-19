# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). No version has been released yet; the first public release will be `v0.1.0` and versions will follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
  the `lazy-observe` binary through `go:embed`. A fixable CRITICAL/HIGH finding
  only warns while the component's pin already is the latest stable upstream
  release at least 7 days old (there is nothing to upgrade to), and blocks the
  release once the pin is behind that release or the upstream lookup fails.
  The verdict for one commit can therefore change over time: when a newer
  upstream release turns 7 days old, its findings start to block until the pin
  is upgraded. At the time of the change the remaining 8 HIGH Go standard
  library findings per architecture only warn (#69).

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
- The web build runs ESLint with the React hooks rules (`pnpm lint`, part of
  `mise run web`); mock data is no longer bundled into production builds.
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
  - An external VictoriaTraces (or a cluster's `vtselect`) in Standalone or HA
    mode is not managed by lazy-observe: pass `-search.latencyOffset=0s` to it
    yourself, otherwise the LLM statistics page lags by up to 30 seconds
    (`docs/deployment.md`).
  - Single-node VictoriaMetrics v1.154 and later answer the labels,
    label-values, and series APIs (used by query completion and AI schema
    collection) from the per-day index only. Long time ranges are slower (2–3
    times in a 30,000-series, 90-day test, still tens of milliseconds);
    upstream warns of much higher memory use at larger series counts and
    retention (#72).
- Versions pinned by hand in scripts, which Dependabot cannot see (the Victoria
  storage binaries, the e2e cluster images, the bootstrap mise release, and
  Trivy), now have a minimum release age of 7 days, kept in
  `scripts/release-age.env`: the `pinned-releases` CI job fails a pull request
  whose pin points at a release younger than that, a prerelease, or a draft
  (and fails closed when the GitHub API cannot be read), and the new weekly
  `pinned-releases` workflow keeps one tracking issue open while a pin is
  behind the latest eligible release. Dependabot's `cooldown` and the
  `minimumReleaseAge` of the `web`, `sdk/node`, and `sdk/browser` pnpm
  workspaces use the same 7 days, so pnpm also holds back a security patch
  younger than that until it is listed in `minimumReleaseAgeExclude`. `web` is
  now a single-package pnpm workspace (`pnpm-workspace.yaml`); the lockfile is
  unchanged. `sdk/python` is not covered yet (#70).
- `mise run sdk-journeys` runs the Go, Node, Python, and browser SDK journeys
  (`sdk/*/e2e.sh`) against a freshly started local platform, and changes that
  touch server, SDK, web, or storage-download paths now run it as the
  `SDK journeys` CI job, which `ci-ok` waits for. The browser
  `performance-journey` was waiting for a heading the Performance page does not
  have and now waits for its Web Vitals tab and breadcrumb; the web page is
  unchanged. `sdk/browser/e2e.sh` installs Playwright system dependencies only
  as root or with passwordless `sudo`. The browser and Python SDK READMEs,
  which since #9 called the journeys manual-only, now point to this task (#68).
- The PostgreSQL metadata tests (`mise run test-meta-pg`) cache the downloaded
  embedded-postgres archive in `LO_TEST_PG_CACHE` (default
  `$HOME/.cache/embedded-postgres`, also cached by CI), so they no longer
  download it from Maven Central on every run, and a failed download reports
  the real HTTP status of the probed URL instead of the library's
  `no version found matching 18.3.0` (#66).
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
  `Symbol.toStringTag` of `"Module"`). In the declarations of
  `@lazy-observe/bundler-plugin`, the `vite` and `webpack` exports are now typed
  as the matching `UnpluginInstance` members instead of referencing a
  `WebpackPluginInstance` type that was never imported. The web console lints
  with Oxlint instead of ESLint and runs its unit tests on Vitest instead of
  `node --test`, and `mise run sdk-browser` now also runs the bundler plugin's
  tests (#97).
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
- `@lazy-observe/node` now requires Node >= 22.15 and ships OpenTelemetry JS
  3.x (`@opentelemetry/sdk-trace` replaces `sdk-trace-node`, the OTLP proto
  exporter is 0.301.1, `api-logs` is folded into `api`), with the span and log
  processor constructors rewritten accordingly (#32).
- The Go SDK now requires OpenTelemetry Go 1.45 (logs 0.21) and emits the
  matching v1.43 semconv schema URL; with the old v1.41 URL, an application on
  OTel Go 1.45 failed to create the client on a schema-URL conflict (#11).
- The server builds with Go 1.26.11 and picks up the govulncheck-reported fixes
  in grpc 1.83.2, x/net 0.62, brotli 1.2.7, and klauspost/compress 1.18.7; the
  `sdk/go` manifest follows the same OpenTelemetry Go 1.45 upgrade (#11).
- Container images build on Debian 13 (trixie) instead of Debian 12 (bookworm)
  and run on `gcr.io/distroless/static-debian13:nonroot`, and the Dockerfile
  frontend plus every base image are pinned to a multi-platform index digest.
  The multi-replica test services drop their Alpine variants (VictoriaMetrics
  uses the `-cluster-scratch` images); only the SeaweedFS fixture stays Alpine,
  because upstream publishes it only that way (#4).
- CI runs `govulncheck` (`mise run vulncheck`, covering the server module and
  `sdk/go`) as a required gate, pins BuildKit to a multi-architecture digest,
  scans the pre-release image with Trivy before the archive and image release,
  rejects unpinned workflow references, and keeps Dependabot on every
  dependency manifest (#11).
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
  query API.
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
  latest release is old enough, and `--latest-eligible` still exits 2, so the
  storage-binary vulnerability check keeps blocking (#86).

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
