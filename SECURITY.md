# Security Policy

## Supported Versions

`lazy-observe` is currently in its **v0.x public preview** phase (no GA release has been published yet). Security fixes are delivered on the `main` branch and included in the next preview release for both the server and the in-repository client SDKs (`sdk/{go,node,browser,python}`). Earlier preview builds and commits are not backported.

| Version | Supported |
| --- | --- |
| `main` (unreleased) | Yes |
| Latest `v0.x` preview release (server and in-repo SDKs) | Yes |
| Older `v0.x` preview releases or commits | No |

Per the public preview policy in [CHANGELOG.md](CHANGELOG.md), [docs/version-matrix.md](docs/version-matrix.md), and [docs/operations.md](docs/operations.md), upgrading between `v0.x` preview builds may require recreating the metadata database when `001_baseline.sql` changes, and protocols or SDKs may change incompatibly between preview versions.

## Reporting a Vulnerability

**Do not open a public GitHub Issue, pull request, or discussion for an unpatched security vulnerability.**

Please report suspected vulnerabilities privately through GitHub Private Vulnerability Reporting:

- **Private advisory form:** <https://github.com/eimsound/lazy-observe/security/advisories/new>

### What to Include

To help reproduce and assess the issue quickly, please include:

- **Affected component and version:** `lazy-observe --version`, commit SHA, container image digest, or SDK package name and version.
- **Deployment mode and environment:** All-in-One, Docker Compose, Linux systemd, Standalone, or Kubernetes/Helm, along with OS and architecture.
- **Minimal reproduction steps:** Exact configuration snippets, HTTP/OTLP requests, or a minimal proof of concept.
- **Security impact:** What boundary is violated (for example, cross-project data access, authentication/RBAC bypass, secret exposure, SSRF, or denial of service).
- **Suggested mitigation or patch** (optional).

### Credential and Data Sanitization

Before submitting any report, logs, or configuration files, **remove all real secrets and sensitive data**, including:

- Project ingest tokens (`lo_...`), user API tokens (`lou_...`), source map upload tokens (`los_...`), and session cookies (`lss_...`).
- User passwords, `initial-admin-password`, and `security.admin_password_file` contents.
- Platform encryption keys (`<data_dir>/secret.key` or `security.secret_key_file`).
- AI provider API keys, PostgreSQL/Redis/S3 credentials, webhook URLs/secrets, and private telemetry or Session Replay payloads.

Replace sensitive values with synthetic placeholders. The 128-bit platform key fingerprint printed in startup logs is a truncated one-way HMAC-SHA256 digest and is safe to include if relevant.

### Response and Disclosure Expectations

- `lazy-observe` is maintained in public preview without a formal response SLA or bug bounty program, and no separate security email address is maintained outside GitHub Private Vulnerability Reporting.
- Maintainers review incoming private advisories on a best-effort basis and communicate status, triage questions, and coordinated disclosure timing directly in the GitHub Security Advisory thread.
- Once a fix is verified and merged (or released), maintainers will publish the corresponding advisory and credit the reporter if requested.

## Security Baseline and Scope

The repository enforces the following built-in security controls across runtime, packaging, and release pipelines:

- **Static pure-Go binaries:** Server binaries are built with `CGO_ENABLED=0` and `-trimpath` for Linux `amd64` and `arm64`, embedding the Web UI and SHA-256-verified VictoriaMetrics/VictoriaLogs/VictoriaTraces/vmagent executables ([.goreleaser.yaml](.goreleaser.yaml), [docs/deployment.md](docs/deployment.md)).
- **Container and host isolation:**
  - The official container image (`ghcr.io/eimsound/lazy-observe`) runs on digest-pinned `gcr.io/distroless/static-debian13:nonroot` as UID/GID `65532:65532` with no shell ([Dockerfile](Dockerfile)).
  - Reference deployment manifests enforce least privilege: [Docker Compose](deploy/compose/README.md) (`read_only: true`, `cap_drop: [ALL]`, `no-new-privileges:true`), [Linux systemd](deploy/systemd/README.md) (`ProtectSystem=strict`, dropped capabilities, dedicated unprivileged user), and the [Helm chart](docs/deployment.md).
  - Only the gateway port (`3100`) is intended to be exposed; embedded Victoria storage children bind strictly to loopback (`127.0.0.1`) and must never be exposed directly ([docs/deployment.md](docs/deployment.md#network-and-security-notes), [docs/grafana-perses.md](docs/grafana-perses.md)).
- **Authentication, encryption at rest, and redaction:**
  - Token capabilities are strictly separated: project API tokens (`lo_...`) are ingest-only, source map upload tokens (`los_...`) are upload-only, and user API tokens (`lou_...`) or HTTP-only `SameSite=Lax` sessions (`lss_...`) authorize reads and management under RBAC (`admin`, `member`, `viewer`) ([docs/version-matrix.md](docs/version-matrix.md), [.agents/docs/architecture.md](.agents/docs/architecture.md)).
  - Passwords are stored as salted Argon2id hashes with constant-time comparison, per-IP and per-username rate limiting, and temporary account lockout. First-boot generated admin passwords are written to `<data_dir>/initial-admin-password` (mode `0600`, never logged) and require replacement at first sign-in unless `security.admin_password_file` is provided ([docs/deployment.md](docs/deployment.md#first-admin-password)).
  - Sensitive metadata (such as AI provider API keys) is encrypted at rest with a 256-bit AES-256-GCM platform key (`security.secret_key_file` / `<data_dir>/secret.key`, verified on startup via its recorded fingerprint) and is never returned to the browser. Structured management audit logs omit passwords, tokens, session values, and request bodies ([docs/deployment.md](docs/deployment.md#platform-encryption-key), [docs/operations.md](docs/operations.md)).
  - Native TLS 1.2+ termination is supported via `security.tls_cert` and `security.tls_key`, or HTTPS may be terminated at a trusted reverse proxy with `Secure` cookies enabled.
- **Supply-chain, CI, and release verification:**
  - CI enforces `govulncheck` (`mise run vulncheck`), permissive license allowlists (`scripts/check-go-licenses.sh`, `scripts/check-js-licenses.sh`, `scripts/check-python-licenses.sh`), a 7-day minimum release age across Renovate, pnpm, and uv (`scripts/release-age.env`), and SHA-pinned GitHub Actions ([CONTRIBUTING.md](CONTRIBUTING.md), [.github/workflows/ci.yml](.github/workflows/ci.yml)).
  - The release workflow ([.github/workflows/release.yml](.github/workflows/release.yml)) builds immutable OCI container archives in a read-only job, fails publishing on fixable `CRITICAL`/`HIGH` container vulnerabilities via SHA-pinned Trivy (`scripts/check-image-vulns.sh`), scans embedded upstream Victoria binaries with Trivy (`scripts/check-storage-bin-vulns.sh`), publishes those exact verified bytes without rebuilding, signs the multi-architecture image index with keyless Cosign, and attaches SLSA build provenance and SPDX SBOM attestations via `actions/attest`.

### Trust Boundary Notes

- `lazy-observe` is designed as a single-team observability platform with project-level logical isolation rather than a hard multi-tenant control plane for mutually untrusted organizations; run separate instances for unrelated security tenants ([docs/deployment.md](docs/deployment.md#network-and-security-notes)).
- Exposures resulting from publishing internal loopback storage ports (`18428`, `19428`, `20428`, `22428`) or misconfiguring external reverse proxies/TLS boundaries are deployment misconfigurations rather than platform vulnerabilities.
