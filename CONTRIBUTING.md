# Contributing to lazy-observe

Thanks for helping. The project is pre-release, and changes should preserve its single-binary direction, explicit SDK instrumentation, project isolation, and cross-platform pure-Go build.

## Before you start

Read [AGENTS.md](AGENTS.md), [requirements](.agents/docs/requirements.md), the [architecture decision records](.agents/docs/adr/), and the [glossary](.agents/docs/GLOSSARY.md). `.agents/docs/requirements.md` is the product baseline and must not be edited as part of ordinary implementation work.

Hard constraints:

- Go 1.26 with the exact module directive currently at 1.26.11.
- `CGO_ENABLED=0` for Linux on amd64/arm64.
- Node.js 26 (LTS, pinned in `mise.toml`) and pnpm 10.33.0 for CI-equivalent web/SDK work.
- No AGPL or GPL dependencies. Production Go dependencies must use Apache-2.0, MIT, or BSD-2/3-Clause licenses (`scripts/check-go-licenses.sh`); production JavaScript dependencies may additionally use ISC, 0BSD, or Unlicense, permissive licenses that ship alongside MIT (`scripts/check-js-licenses.sh`).
- PoCs are local-only scratch code under `poc/<topic>/` (git-ignored, never committed); findings worth keeping go into `.agents/docs/research/<topic>.md`. Production code belongs under `cmd/` and `internal/`; client SDKs belong under `sdk/{go,node,browser,python}`.

## Development setup

Install mise 2025.8.4 or newer, `curl`, `tar`, and either `sha256sum` or `shasum`. Then:

```sh
mise install
mise run build
mise run storage-bins
mise run test
mise run sdk-go
mise run web
```

`mise run storage-bins` downloads checksum-pinned VictoriaMetrics, VictoriaLogs, VictoriaTraces, and vmagent releases into `.cache/storage-bin`.

## mise tasks

| Target | What it runs |
| --- | --- |
| `mise run ent-generate` | Generate the ignored Ent client in `internal/db/ent` from the tracked schema |
| `mise run build` | Build the Web UI and a pure-Go development gateway; the binary step honors `GOOS`, `GOARCH`, and `BINARY` |
| `mise run dist` | Build the current-platform release binary with embedded Web and storage assets |
| `mise run dist-cross` | Build embedded release binaries for Linux on amd64/arm64 |
| `mise run fmt-check` | Verify tracked non-generated Go files use `golines -m 120 --chain-split-dots` |
| `mise run mod-tidy-check` | Verify the root and Go SDK module manifests need no `go mod tidy` changes |
| `mise run test` | Generate Ent, then run `CGO_ENABLED=0 go test ./...` |
| `mise run vet` | Generate Ent, then run `CGO_ENABLED=0 go vet ./...` |
| `mise run lint` | mise-managed golangci-lint `v2.12.2` |
| `mise run test-meta-pg` | Run metadata tests against the embedded PostgreSQL baseline |
| `mise run migration-check` | Validate the paired SQLite/PostgreSQL single baselines |
| `mise run ent-generate-check` | Regenerate Ent clients and prove automatic migration code and root-module drift stay absent |
| `mise run license-check` | Check all four production Go package graphs against the Apache-2.0/MIT/BSD allowlist |
| `mise run vulncheck` | Run pinned `govulncheck` over the server module and `sdk/go`; fails on reachable known vulnerabilities |
| `mise run storage-bins` | Download and verify the four platform storage binaries |
| `mise run e2e` | Build, download storage, run opt-in Gateway, recovery, and restart E2E |
| `mise run e2e-ha` | Run the required Docker gate for real signal clusters, S3, and multi-replica coordination |
| `mise run perf-release` | Gate all four embedded release sizes and smoke-test the native zero-config artifact |
| `mise run perf-ha` | Gate exact medium-corpus query P95 and capacity-controlled per-replica weak scaling against a validating sink, with real-storage mirrored readback |
| `mise run performance-gates` | Run the release, query, and HA scaling gates sequentially |
| `mise run web` | Frozen pnpm install, Node test runner, typecheck, and production build |
| `mise run sdk-go` / `sdk-node` / `sdk-browser` / `sdk-python` | Build and test one client SDK under `sdk/` (frozen installs for the JS packages) |
| `mise run helm-lint` / `mise run helm-template-test` | Lint and contract-test all-in-one and HA chart profiles |
| `mise run workflow-check` | Statically validate GitHub Actions workflows without dispatching or executing them |
| `mise run ci-changes-test` | Contract-test the CI change detector that decides which jobs a diff runs |

When a test command changes, update `mise.toml` in the same change; mise tasks are CI entry points.

## Test layers

### Unit and package tests

```sh
mise run fmt-check
mise run mod-tidy-check
mise run test
mise run vet
mise run lint
mise run sdk-go
mise run sdk-node
mise run sdk-browser
mise run sdk-python
mise run web
```

Go tests run without CGO. Browser and Node SDK package tests use Vitest; the Python SDK uses pytest. Web contract and route tests use Node's built-in `node --test` runner; the Web task then runs typecheck and the production build.

### Storage E2E

```sh
mise run e2e
```

This starts real pinned Victoria components. The root orchestrator supplies the explicit `LO_E2E_*` paths to each fixture; normally contributors should not set them manually.

`mise run e2e-ha` is the multi-replica HA functional gate (clustering, shared
state, S3, probes, shutdown). Pull request CI runs it as the `HA E2E
(multi-replica)` job whenever server code or the gate's inputs change, so a
local run is optional; run it before pushing when you want the result early.
It requires a working Docker daemon. Exit 77 means the prerequisite was absent
and must be reported as `SKIP`, never PASS.

### Release and performance gates

```sh
mise run perf-release
mise run perf-ha
# Or run all gates sequentially:
mise run performance-gates
```

Run the relevant performance task when release packaging, SDK hot paths,
queries, or platform capacity scaling change. `mise run e2e-ha` alone does not execute
the controlled performance fixture and is not a substitute for `perf-ha` or
`performance-gates`. The HA performance gate requires Docker and uses the same
exit-77 `SKIP` contract as the HA E2E gate.

### What CI runs when

CI runs on GitHub-hosted runners. A `changes` job classifies the diff, each
business job runs only when its surface changed, and a final `ci-ok` job
aggregates the results; `ci-ok` is the only required check. Skipped jobs count
as passing, so a documentation-only change finishes without running any
business job.

| Changed paths | Jobs that run |
| --- | --- |
| `*.md`, `docs/**`, `LICENSE`, `.agents/**` | None |
| `sdk/node/**`, `sdk/browser/**`, `sdk/python/**` | The matching SDK job |
| `sdk/go/**` | Go SDK job plus the gateway Go checks, build, E2E, and HA E2E (the server links `sdk/go` through `replace`) |
| `web/**` | Web |
| `internal/db/**`, `internal/meta/**` | Adds PostgreSQL metadata tests |
| `cmd/**`, `internal/**` | Go checks, build, E2E, restart, HA E2E |
| `go.mod`, `go.sum` | Same as above plus PostgreSQL metadata tests |
| `scripts/e2e-multi-replica.sh` and its helpers, `bench/performance-gates.json`, `scripts/download-storage-bins.sh` | Adds HA E2E |
| Packaging chain | Zero-config artifact smoke plus native arm64 build and smoke |
| `.github/dependabot.yml`, `.trivyignore.yaml` | Go checks only (`workflow-check` validates them) |
| `.github/**`, `mise.toml`, `scripts/ci-*`, `Dockerfile*` | Everything |

| Trigger | Workflow | Scope |
| --- | --- | --- |
| Pull request, push to `main` or `integration/**` | `ci.yml` | Change-scoped jobs above, aggregated by `ci-ok` |
| `v*` tag or manual release dispatch | `release.yml` | amd64/arm64 container smoke, a Trivy image scan that fails on fixable CRITICAL/HIGH vulnerabilities, and the performance gates, all before archives are built |
| Manual dispatch | `bench.yml` | Performance gates on demand |

Performance gates are noisy on shared runners, so they run only on manual
dispatch and before a release; the PR job runs only the functional HA
assertions of `e2e-ha`, never `perf-ha`. Run them locally when your change is
performance-sensitive. `mise run ci-changes-test` pins the path grouping; update
it together with the detector.

### PostgreSQL dialect tests

```sh
mise run test-meta-pg
```

The test uses embedded PostgreSQL and may download/start a local test server. CI runs this dialect separately. Tests that need an already running full gateway may use the explicit `LO_E2E_*` variables documented in the relevant test or script; do not make network-dependent E2E behavior implicit in unit tests.

## Metadata baseline

The project is pre-release and does not maintain a database upgrade chain.
Schema changes update the complete baseline in both directories:

```text
internal/meta/migrations/001_baseline.sql
internal/meta/migrations/postgres/001_baseline.sql
```

Keep the complete logical schema aligned across SQLite and PostgreSQL. Baseline creation and its single marker row run in one transaction. Existing development databases are disposable and must be recreated after incompatible changes. Before the first supported release, a new ADR must freeze this baseline and define append-only upgrades. Update the tracked Ent schema in `internal/db/schema`, run `mise run ent-generate`, then run both dialect tests. The generated `internal/db/ent` tree is intentionally ignored and must not be committed.

Runtime code does not include Ent automatic migration. Ent code generation uses the isolated `entc.mod` manifest so Atlas tooling cannot modify the production `go.mod`; generated `migrate` and `enttest` packages are removed deliberately. The mise build, test, vet, lint, license, and development entry points generate the ignored client before compiling. Run `mise run ent-generate-check` after schema or generator changes to prove generation is atomic and deterministic.

Run `mise run migration-check` before committing a schema change. It verifies that each dialect contains exactly one `001_baseline.sql` and that their filenames match. Run `mise run license-check` after dependency changes; it checks Linux on amd64/arm64 and rejects production packages outside the allowed license set.

## Commit and pull request style

Commit messages use one of the repository's allowed change prefixes followed
by a concise Chinese description:

```text
fix|feat|chore: 中文简述
```

Examples are `fix: 修复回放项目隔离`、`feat: 增加集群性能门禁` and
`chore: 迁移构建任务到 mise`. Keep each commit scoped to one reviewable
feature point or milestone, and update `CHANGELOG.md` in the same commit when
the change affects delivered behavior.

Before opening a GitHub pull request:

- Run the relevant mise tasks and report the exact commands and real results.
- Add or update tests for behavior changes.
- Update documentation and `config.example.yaml` when public behavior changes.
- Check new dependency licenses.
- Preserve unrelated work in a dirty worktree.
- Run `mise run workflow-check` when workflows change, then report the
  pull request's `ci-ok` result separately from local results.

Use the pull request template; do not mark a check as complete unless you ran it.

Protocol changes between the gateway and the SDKs update
[docs/version-matrix.md](docs/version-matrix.md) in the same pull request.
