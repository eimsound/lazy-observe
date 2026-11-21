# Server and client SDK version compatibility matrix

This document is the source of truth for version compatibility between the lazy-observe server and the four client SDKs (Go, Node, Browser, and Python). Server releases, SDK releases, and protocol changes follow this file and update it on every release.

## 1. Why a declarative matrix instead of runtime handshakes

During the pre-release phase, traffic is small and changes are frequent. Adding a runtime protocol handshake (sending a protocol version header, reporting SDK versions, and branching server behavior) costs more than it pays back. Both sides would need simultaneous negotiation logic, version mapping tables, and fallback paths that would be rewritten or deleted on almost every pre-release change.

For now, lazy-observe combines a declarative matrix with CI enforcement:

- **Single protocol version number.** Section 2 defines the protocol baseline in this document.
- **Declarative matrix document.** This file anchors compatibility to the server protocol version, and every SDK README under `sdk/` links to it.
- **Monorepo enforcement.** The server and all four SDKs live in one repository (`sdk/{go,node,browser,python}`; see [ADR 0012](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0012-monorepo-sdks-and-github-hosting.md)). Protocol changes update this matrix in the same PR and run one CI suite across both sides (Section 4).
- **SDK degradation rules.** When a private ingest endpoint returns 404 or another 4xx status, SDKs degrade silently, record diagnostics, and never throw into application code.

At general availability (GA), the project will move to runtime enforcement with an `X-LO-Protocol-Version` header, SDK version reporting in diagnostics, and automated CI checks for matrix consistency. Section 6 outlines that migration path.

## 2. Protocol anchor

The current pre-release server protocol baseline is **v0** (no handshake and no version negotiation field). The server pairs this protocol version with its compile-time `main.Version` (`dev` in pre-release, or a semantic version injected through `-ldflags` during releases). Only this document records the server protocol version; no request field carries it.

### 2.1 Supported protocol endpoints

| Category | Endpoint | Details |
| --- | --- | --- |
| OTLP metrics (HTTP) | `POST /v1/metrics` | OpenTelemetry protocol with standard OTLP/HTTP payload |
| OTLP logs (HTTP) | `POST /v1/logs` | Forwarded by the gateway to VictoriaLogs `insert/opentelemetry/v1/logs` |
| OTLP traces (HTTP) | `POST /v1/traces` | Forwarded by the gateway to VictoriaTraces `insert/opentelemetry/v1/traces` |
| OTLP traces (gRPC) | `opentelemetry.proto.collector.trace.v1.TraceService/Export` | Native OTLP/gRPC ingest (default `:4317`, enabled via `otlp_grpc.enabled`, supports gzip and native TLS), forwarded to VictoriaTraces `insert/opentelemetry/v1/traces` |
| OTLP metrics (gRPC) | `opentelemetry.proto.collector.metrics.v1.MetricsService/Export` | Native OTLP/gRPC ingest (default `:4317`), forwarded to VictoriaMetrics `opentelemetry/v1/metrics` with trusted `extra_label=project_id=<slug>` injected |
| OTLP logs (gRPC) | `opentelemetry.proto.collector.logs.v1.LogsService/Export` | Native OTLP/gRPC ingest (default `:4317`), forwarded to VictoriaLogs `insert/opentelemetry/v1/logs` |
| Private ingest | `POST /api/events/errors` | Error event ingestion |
| Private ingest | `POST /api/events/vitals` | Web Vitals ingestion; unknown metric names are skipped and counted (Section 5), response includes `rejected`; supports optional low-cardinality device and geography fields (details below) |
| Private query | `GET /api/v1/performance/vitals` | Project-isolated Web Vitals aggregation and breakdowns (`by_page`, `by_device`, `by_browser`, `by_os`, `by_country`), with filtering by `page`, `device_type`, `browser_family`, `os_family`, and `country` |
| Private ingest | `POST /api/events/diagnostics` | SDK diagnostics ingestion; unknown `signal` or `reason` values are skipped and counted (Section 5), response includes `rejected` |
| Private ingest | `POST /api/replay/segments` | Session replay segment ingestion |
| Private ingest | `POST /api/profiles` | Continuous profiling (pprof) ingestion |
| Private upload | `POST /api/releases/:release/sourcemaps` | Source Map artifact upload; accepts only Source Map upload tokens (`los_...`, Section 2.2); project API tokens and user API tokens return `403` `source_map_upload_token_required` |
| Self-monitoring metrics | `GET /metrics` | Prometheus text format 0.0.4 (`text/plain; version=0.0.4; charset=utf-8`); enabled explicitly via `metrics_endpoint.enabled` (off by default); requires admin auth by default, or an optional static scrape bearer token, or explicit `allow_unauthenticated` (Section 2.2) |

Current enum values for `POST /api/events/diagnostics`:

- `signal`: `error`, `trace`, `vitals`, `replay`, `diagnostic`
- `reason`: `queue_full`, `retry_exhausted`, `shutdown_budget`, `server_rejected`

Each item in `POST /api/events/vitals` may include these optional low-cardinality dimension fields (omitting them stays fully backward-compatible, and the protocol version remains **v0**):

- `device_type` accepts `desktop`, `mobile`, `tablet`, or `unknown`. When the client omits it, the server falls back to low-cardinality `User-Agent` normalization. Non-allowlisted values normalize to `unknown`.
- `browser_family` accepts `chrome`, `edge`, `safari`, `firefox`, `opera`, or `other`. When the client omits it, the server falls back to `User-Agent` normalization. Non-allowlisted values normalize to `other`.
- `os_family` accepts `windows`, `macos`, `ios`, `android`, `linux`, `chromeos`, or `other`. When the client omits it, the server falls back to `User-Agent` normalization. Non-allowlisted values normalize to `other`.
- `country` accepts a two-character uppercase ASCII ISO 3166-1 alpha-2 country code (such as `US` or `CN`), excluding unknown placeholders (`XX`, `T1`, `ZZ`, `A1`, `A2`, `O1`). Invalid values normalize to `unknown`. Only when a request arrives from a proxy listed in `security.trusted_proxies` does the server extract standard edge headers first (`CF-IPCountry`, `CloudFront-Viewer-Country`, `X-Vercel-IP-Country`, `X-Country-Code`). When no trusted edge header is present, the server accepts a valid two-letter country code passed explicitly by the SDK; otherwise it records `unknown`. The server never writes real client IP addresses, raw `User-Agent` strings, or free text into metric labels.

`GET /api/v1/performance/vitals` requires a session cookie or a user API token, rejects project API tokens, and enforces project isolation. It returns `{ start, end, step_seconds, filters, summary, total_samples, by_page, by_device, by_browser, by_os, by_country, truncated }`. Historical series without dimension labels group under `unknown` or `other` during aggregation and filtering.

Enum values unknown to the server do not reject the entire batch (Sections 2.3 and 5).

### 2.2 DSN format and credential contracts

User DSN format:

```
http(s)://<host>/p/<project-slug>@<token>
```

`<token>` is a project API token with the `lo_...` prefix. The SDK parses the project slug and token, sends the credential only in the `Authorization: Bearer <token>` header, and never transmits the full DSN in plaintext. Over native OTLP/gRPC (default `:4317`), clients pass the project API token in incoming gRPC metadata as `authorization: Bearer <token>` (case-insensitive scheme). Missing, malformed, invalid, or revoked tokens return `Unauthenticated`. User API tokens (`lou_...`) and Source Map upload tokens (`los_...`) return `PermissionDenied`. Auth store or upstream storage failures return `Unavailable` with a sanitized error message.

Project API tokens are ingest-only ([ADR 0010](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0010-project-api-tokens-are-ingest-only.md)). A token embedded in an SDK DSN works only on the ingest endpoints in Section 2.1. Every read route, including `/mcp` and `/api/system/info`, returns `403` with `code: ingest_only_token` when given a project API token. Read operations accept a session cookie (newly issued session tokens use the `lss_...` prefix to stay distinct from `los_...` upload tokens, while existing sessions remain compatible by digest) or a user API token (`lou_...`) paired with the `X-LO-Project` header.

Source Map uploads ([ADR 0018](https://github.com/eimsound/lazy-observe/blob/main/.agents/docs/adr/0018-source-map-upload-token.md)) use a dedicated project-level secret called the Source Map upload token (`los_...`). The DSN shape stays unchanged (`http(s)://<host>/p/<project-slug>@<los_...>`), so `@lazy-observe/bundler-plugin` and the `lazy-observe-sourcemaps` CLI parse it as usual, except the token is an upload token kept in CI secrets and never shipped to browsers. Global `admin` users (matching project API token management privileges) create and revoke upload tokens in the web UI under Settings, Projects, Manage tokens, or through `GET|POST /api/projects/:id/source-map-upload-tokens` and `DELETE /api/projects/:id/source-map-upload-tokens/:tokenID` (both require `admin`). The plaintext token returns once on creation, and create and revoke actions write audit records `source_map_upload_token.create` and `source_map_upload_token.revoke`. Authorization rules:

- `POST /api/releases/:release/sourcemaps` accepts only `los_` tokens and writes only to the project that owns the token. Project API tokens (`lo_`) and user API tokens (`lou_`) return `403` with `code: source_map_upload_token_required`. Missing, unknown, or revoked tokens return `401`.
- Any `los_` token sent to any other route (ingest, query, `/mcp`, or admin API) returns `403` with `code: upload_only_token`.
- Revocation takes effect immediately on the local replica and within 30 seconds on other replicas once their auth cache expires, matching project API tokens.

MCP (`lazy-observe mcp --dsn`) uses the same DSN shape, except `<token>` must be a `lou_...` user API token created in the web UI under Settings, Personal API tokens, and `<project-slug>` travels in the `X-LO-Project` header. `lazy-observe mcp` rejects non-`lou_` tokens at startup instead of waiting for the first tool call to fail with `403`.

#### Unified ingest rate limiting and backpressure contract

The gateway can enforce per-project (`project.ID`) token-bucket rate limiting through `ingest_rate_limit` across all HTTP ingest endpoints and the native OTLP/gRPC service. It is disabled by default. When enabled, it limits traffic by `requests_per_second`, `burst`, and optional `bytes_per_second`, with bounded eviction controlled by `max_tracked_projects`:

- **HTTP ingest routes.** Requests over the limit fail before reaching upstream storage with `429 Too Many Requests`, a `Retry-After: <positive integer seconds>` header (minimum `1` second, maximum `300` seconds), and the sanitized JSON body `{"code":"ingest_rate_limited","error":"project ingest rate limit exceeded"}`.
- **OTLP/gRPC ingest service.** Shares the same rate limiter instance and returns gRPC `codes.ResourceExhausted` with `google.rpc.RetryInfo` (`retry_delay` set to a positive integer number of seconds).
- **Project isolation.** Each project has an isolated token bucket, so rate limiting one project does not affect any other project.

#### Self-monitoring `/metrics` authentication and low-cardinality contract

`GET /metrics` is gated by `metrics_endpoint.enabled` (default `false`; returns `404` when disabled and never falls through to the frontend SPA fallback). When enabled, authentication fails closed:

- **`metrics_endpoint.bearer_token` configured.** Accepts only a matching `Authorization: Bearer <token>` header using constant-time comparison. Combining it with `allow_unauthenticated: true` is forbidden. All other requests return `401`.
- **No `bearer_token` and `allow_unauthenticated: false` (default when enabled).** Requires an authenticated `admin` session or an `admin` user API token (`lou_...`). Anonymous requests return `401`, project API tokens return `403` `ingest_only_token`, upload tokens return `403` `upload_only_token`, and non-admin users return `403`.
- **No `bearer_token` and explicit `allow_unauthenticated: true`.** Allows unauthenticated scraping for internal network or Pod scraping setups.
- **Label cardinality.** Metric labels use bounded static enums only (such as `surface`, `status_class`, `protocol`, `signal`, and `outcome`). The endpoint never exposes `project_id`, `trace_id`, dynamic URLs, or credentials.

### 2.3 Protocol version change rules

- **Breaking changes** (endpoint path or semantic changes, removed or redefined request or response fields, authentication changes, or DSN format changes) bump the server protocol version (for example, from v0 to v1).
- Every protocol version bump must update this matrix (endpoint list, compatibility table, and affected SDK versions) before release.
- New endpoints and new optional response fields are backward-compatible. Document them in Section 2.1 without bumping the protocol version.
- **New request-side enum values** are backward-compatible. The server skips and counts unknown values while still writing known values in the same batch instead of rejecting the batch. Both `POST /api/events/vitals` (metric names) and `POST /api/events/diagnostics` (`signal` and `reason`) implement this and return `accepted` and `rejected` counts. Structural errors (missing fields, empty `signal` or `reason`, invalid timestamps, non-positive counts, overly long `service` values) still reject the entire batch with `422` and write nothing.

## 3. Compatibility matrix

| Server version | Go SDK | Node SDK | Browser SDK | Python SDK | Status |
| --- | --- | --- | --- | --- | --- |
| `dev` (pre-release, planned initial release `v0.1.0`) | 0.1.0 | 0.1.0 | 0.1.0 | 0.1.0 | Verified |

Notes:

- The matrix currently holds one row: the server pre-release baseline (`main.Version=dev`, planned initial release `v0.1.0`) and all four SDK packages in `sdk/{go,node,browser,python}` at version `0.1.0`.
- `Verified` means the server build, e2e suite, and each SDK's test suite passed on the same repository commit (Section 4).
- This file is authoritative and updates on every server or SDK release. Any unlisted combination, such as a newly released SDK before server re-verification, is unverified and unsupported until tested.

### 3.1 SDK runtime requirements

Minimum SDK runtimes are part of the compatibility contract. Update this section in the same PR as any SDK runtime change.

| SDK | Runtime requirement | Rationale |
| --- | --- | --- |
| Node SDK (`@lazy-observe/node`) | Node.js `>=22.15` (`engines.node` in `package.json`) | Both OpenTelemetry JS 3.x and OTLP exporter 0.301.x specify `engines.node >=22.15.0`; the repository dev and CI toolchain runs Node 26.13.1 |

## 4. Maintenance rules

### 4.1 Server releases

The server release workflow (`.github/workflows/release.yml`, triggered by `v*` tags) builds `sdk/go` from the same commit because the root `go.mod` contains `replace github.com/eimsound/lazy-observe/sdk/go => ./sdk/go`.

1. CI (including e2e tests) must pass on the release commit.
2. Once the release version is chosen, add a new row to the matrix with the matching SDK versions before publishing the release.

### 4.2 SDK releases

SDKs publish from this repository using prefixed tags: `sdk/go/vX.Y.Z` for Go (Go nested module convention), and `sdk-node-vX.Y.Z`, `sdk-browser-vX.Y.Z`, and `sdk-python-vX.Y.Z` for Node, Browser, and Python.

1. Server build, e2e tests, and the target SDK's tests must pass on the tagged commit in one CI run.
2. Update the target SDK column in Section 3.
3. Tag and publish the SDK.

### 4.3 Protocol changes

Any change to endpoints, fields, DSN format, or authentication must update this document (Section 2 and Section 3) before release. Bump the protocol version whenever a change breaks compatibility.

## 5. SDK degradation rules

These rules match the version compatibility and degradation section in `sdk/AGENTS.md`.

When an SDK sends telemetry and encounters any of these server conditions:

- A private ingest endpoint returns 404 or another 4xx status (endpoint missing, removed, or authentication rejected).
- A response contains unknown fields that the SDK does not recognize.
- A request contains fields or enum values the server does not recognize and rejects.

The SDK must follow four rules:

1. **Silent degradation.** Drop the failed payload. For retryable signals such as replay segments or profile chunks, the SDK may buffer and retry with backoff.
2. **Diagnostics reporting.** Record the degradation reason (HTTP status code, endpoint, and SDK version) in the diagnostics buffer and send it on the next diagnostics flush. If the diagnostics endpoint also fails, keep an in-process counter only and never block.
3. **Never throw.** Degradation must never throw exceptions into application code, block business threads, or alter application behavior.
4. **Forward compatibility with newer servers.** Ignore any unknown response fields and parse only recognized structures.

These rules apply only to runtime SDKs. `POST /api/releases/:release/sourcemaps` (Section 2.1) is called by build-time tools (`@lazy-observe/bundler-plugin` Vite plugin and the `lazy-observe-sourcemaps` CLI) and does not silently degrade. When a DSN is configured and upload fails (including 4xx), the Vite build fails and the CLI exits non-zero. When no DSN is configured, both warn and skip upload.

Today, `POST /api/events/vitals` (metric names) and `POST /api/events/diagnostics` (`signal` and `reason`) tolerate unknown enum values. Instead of rejecting the full batch, the server skips unknown entries, counts them in `rejected`, and writes the known entries (`accepted`). Servers with partial acceptance do not drop valid items in the same batch when a new enum value appears.

**Cost of running a newer SDK against an older server.** The browser SDK added the `vitals` signal and `server_rejected` reason. An older server without partial acceptance rejects the entire diagnostics batch with `422`, dropping known diagnostics in the same batch (such as `error` signal or `queue_full` reason) alongside the new values. On the SDK side, that batch stays in the `diagnostics` queue and retries with exponential backoff up to `maxAttempts`. Once the retry budget is exhausted, the SDK drops the batch, increments `droppedTelemetry` by the batch size, records one local drop with `signal` set to `diagnostic` and `reason` set to `retry_exhausted` (never recursively reported to the diagnostics endpoint), logs only `console.warn`, and never throws or disrupts application code. Against a newer server, unknown values are skipped and counted while known diagnostics write normally, and `rejected > 0` does not trigger degradation.

On every endpoint, structural errors (missing `page`, empty metric name, invalid timestamp, empty `signal` or `reason`, overly long `service`, or non-positive `dropped`) reject the entire batch with `422` and write nothing.

## 6. Pre-GA roadmap

Before GA, lazy-observe will upgrade the declarative matrix to runtime enforcement:

1. **Protocol version header.** Add `X-LO-Protocol-Version` on both server and SDKs. SDKs report their implemented protocol version, and the server decides how to handle requests. Unknown or incompatible versions follow the degradation rules without denying service or altering application behavior.
2. **Diagnostics version reporting.** Include runtime and SDK version metadata in diagnostics payloads so the server can track SDK version distribution and degradation rates.
3. **Automated CI matrix check.** Add a release CI script that verifies matrix SDK versions match published versions, starting with a read-only dry-run check in server CI.
4. **Matrix reassessment.** Once runtime verification runs stably, decide whether to retire the declarative matrix or keep it as a historical reference table.
