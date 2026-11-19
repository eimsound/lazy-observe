# mise 升级到 v2026.11.7

## 目标与范围

- 把 `scripts/mise-release.env` 固定的 mise 从 `v2026.5.3` 升到「发布满 7 天（`scripts/release-age.env`）的最新稳定版」。
- 完成条件：`bash scripts/check-pinned-releases.sh --gate` 通过，且用新版 mise 跑通 CI 门禁与 Docker 构建的 mise 安装层。
- 顺带修正：`scripts/ci-changes.sh` 把 `scripts/mise-release.env` 归入全量路径（见下文「CI 覆盖」）。
- 范围外：不动 `mise.toml` 的工具版本、不动任务语法、不换 actionlint；`CHANGELOG.md` 走独立的 changelog 分支。

## 目标版本

- `GH_TOKEN=$(gh auth token) bash scripts/check-pinned-releases.sh --latest-eligible jdx/mise` → `v2026.11.7`。
- 发布时间 2026-11-12T00:46:36Z（`gh api repos/jdx/mise/releases/tags/v2026.11.7`：`prerelease=false`、`draft=false`、`immutable=true`），
  在 2026-11-19 已满 7.3 天。上游已有 v2026.11.8（2026-11-13）、v2026.11.9、v2026.11.10，都不满 7 天，不取。

## sha256 来源（两处交叉核对一致）

| 资产 | sha256 |
| --- | --- |
| `mise-v2026.11.7-linux-x64` | `8f5426d9971eaf713b37509749174ca69cf2f08b8b25e389168dcfcab9cd1a4d` |
| `mise-v2026.11.7-linux-arm64` | `e4392a51b1b99107b3dd0c8c1b1efa737b614e15950af34921067ce829929af8` |

1. release 的 `SHASUMS256.txt`（`https://github.com/jdx/mise/releases/download/v2026.11.7/SHASUMS256.txt`）。
2. `gh api repos/jdx/mise/releases/tags/v2026.11.7` 里每个资产的 `digest` 字段。

amd64 的二进制另外下载后用 `sha256sum --check --strict` 实测通过。arm64 本机无法执行，只核对了上面两处来源。

引用该版本或 sha256 的地方用 `rg` 查全，只有 `scripts/mise-release.env` 一处需要改：`scripts/ci-setup-mise.sh`、
`Dockerfile` builder 阶段、`scripts/ci-setup-mise-test.sh` 和 `scripts/check-github-workflows.sh` 都是读取这份文件，
没有写死版本。`CHANGELOG.md` 里的 `v2026.5.3` 是历史条目，不改。

## v2026.5.3 → v2026.11.7 的 breaking 与相关变更

来源：`jdx/mise` 在 `v2026.11.7` tag 的 `CHANGELOG.md`（2026.5.3 之后全部条目，约 55 万字符）。
上游明确标 `**breaking**` 的只有 4 条，另有若干影响默认行为的条目一并列出。

| 变更 | 对我们的影响 | 结论 |
| --- | --- | --- |
| **breaking** `npm`：默认校验 npm provenance（#15341） | 只影响 `npm:` 后端。我们的 pnpm 走 aqua 二进制（安装日志是 `pnpm-linux-x64`），node/go 走 core | 不受影响 |
| **breaking** `java`：默认 Temurin（#14146） | 不用 java | 不受影响 |
| **breaking** 移除实验性 `pkgx` 后端（#13555） | 不用 | 不受影响 |
| **breaking** 任务输出样式与 verbosity 解耦（#10885）：`task.output` 只表示样式，`quiet`/`silent` 另走独立开关 | 仓库没有设 `task.output`、`MISE_TASK_OUTPUT`，也没有给 `mise run` 传 `--output`/`--quiet`（`mise.toml` 里的 `--output` 是任务内 bench 脚本参数） | 不受影响 |
| 默认 `minimum_release_age` 24h 对所有后端生效（#10279、#14701） | 上游 `settings.toml` 写明默认截止只作用于 `node@22`、`latest` 这类需要解析的版本请求，精确版本与 `mise.lock` 里的版本不过滤；`mise.toml` 全是精确版本且都早于 24h；CI 脚本 `ci-setup-mise.sh` 的工具也是精确版本（`go`/`rg`/`node`/`pnpm`/`jq` 取 `mise.toml` 的精确版本） | 不受影响，安装实测通过 |
| `lockfile_mode` 默认变为 `generate`（#14893） | 仓库没有 `mise.lock`；全部安装与任务运行后 `git status` 没有新文件 | 不受影响 |
| 锁定模式要求 checksum（#15285）、`go.mod`/`CMakeLists.txt` 不再作为版本来源（#15266） | 我们不用 `--locked`，Go 版本在 `mise.toml` 里写死 | 不受影响 |
| 安全类收紧：`path:` 工具、带内联选项的工具键、远程任务文件、config-less 任务 include 需要 trust（#14358、#13849、#14416、#10355） | 我们没有这些写法；`ci-setup-mise.sh` 与 Dockerfile 都先 `mise trust <mise.toml>` | 不受影响 |
| `settings` 写在 mise 忽略的位置会被拒绝（#14126）、`elevate_with` 仅全局（#14972） | 仓库 `mise.toml` 没有 `[settings]`，也没有 `[env]` | 不受影响 |
| `[env]` 默认开启 shell 展开（#10702） | 仓库 `mise.toml` 没有 `[env]` | 不受影响 |
| 任务：`run` 列表、`depends`、`--skip-deps`、`--skip-tools`、`MISE_TASK_RUN_AUTO_INSTALL` | 逐项实测仍有效（见下）；changelog 里任务相关条目没有标 breaking 的语法变化（读 changelog 加实测，没有逐条读完 600 多条） | 不受影响 |
| shim：新增 `shims.fallback_to_installed`（#15525，默认关）、只读文件系统下的命令包装（#15300） | 仓库不依赖 shim 行为，Dockerfile 的 `PATH` 里的 shims 目录仍然可用（`mise install` 安装层实测通过） | 不受影响 |
| `actionlint` 被标记为 deprecated，建议换 `jactionlint`（#13960） | 本仓 `mise.toml` 与 `ci-setup-mise.sh` 用 `actionlint@1.7.12`；新版 mise 现在只在安装时多打一条 `WARN`，安装与 `workflow-check` 都成功 | 不阻塞，后续见「actionlint 弃用的后续」 |

## actionlint 弃用的后续

- #13960 只给 mise 注册表里的 `actionlint` 条目加了 `deprecated` 提示，后端与安装路径不变，提示是 `WARN`，不影响退出码。
  上游没有承诺何时把它变成错误；可以预见的风险是以后某个版本从注册表移除这个条目，那时
  `actionlint@1.7.12` 这个短名会解析失败，CI 的 `go` job 会在 `mise install` 处直接报错，不会静默放过。
- `jactionlint` 是 jdx 维护的 fork（PR 当时 6 star、0 fork），二进制名不同，换过去要同时改 `mise.toml`、
  `ci-setup-mise.sh` 的 `go-ci` profile 与 `check-github-workflows.sh` 的调用，还要重新评估许可证与发布节奏。
- 建议后续另开切片，二选一：把短名换成显式后端（如 `aqua:rhysd/actionlint`），继续用上游 actionlint、
  不受注册表条目移除影响；或者评估后换成 `jactionlint`。下次升级 mise 时先看注册表里 `actionlint` 条目是否还在。

## CI 覆盖

`scripts/mise-release.env` 是 3793e0e 从 `ci-setup-mise.sh` 与 `Dockerfile` 拆出来的，这两处都在
`scripts/ci-changes.sh` 的全量路径里，拆出后的新文件却落进 `scripts/*` 兜底，只跑 `go`（另叠加 `pins`）。
这样一次 mise 升级在 PR 上只验证 `go-ci` profile，`base`/`meta-pg`/`sdk-python`/`sdk-journeys` profile、Docker 构建、
`mise x`、`mise run --skip-tools` 都没人跑（`release` profile 只在 `release.yml` 用，PR CI 本来就不跑）。本切片把它归入全量路径，`ci-changes-test.sh` 与 `CONTRIBUTING.md`
同步更新。

## 验证（2026-11-19）

用新版 mise 验证，没有覆盖开发机上的全局 mise：mise 装在临时目录（`ci-setup-mise.sh` 的下载与 sha256 校验路径在临时 `HOME` 里实跑一次），
之后的门禁用 `env -i` 干净环境，`PATH` 首位是新版 mise，`MISE_DATA_DIR`/`MISE_CACHE_DIR`/`MISE_STATE_DIR` 指向临时目录，
忽略 `~/.config/mise/config.toml`，工具全部在这套隔离目录里重新安装（`MISE_CI_PROFILE` 取 `go-ci`、`base`、`release`）。

| 命令 | 结果 |
| --- | --- |
| `ci-setup-mise.sh`（临时 `HOME`、PATH 无 mise、`MISE_CI_TOOLS=jq`） | 下载 `mise-v2026.11.7-linux-x64`，sha256 `OK`，装出 `2026.11.7` |
| `bash scripts/ci-setup-mise-test.sh` | PASS |
| `mise run workflow-check` | 通过（含 `check-pinned-releases-test.sh`、`ci-changes-test.sh`、`ci-setup-mise-test.sh`） |
| `mise run ci-changes-test` | PASS |
| `mise run fmt-check` | 通过 |
| `mise run vet` | 通过 |
| `mise run test` | 通过（`go test ./...` 全部 ok） |
| `GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint` | `0 issues.` |
| `mise run release-snapshot-check` | PASS：snapshot release configuration is valid |
| `docker build --target builder`，Dockerfile 截到 `mise install go node pnpm` 那一层 | 构建成功，`/tmp/mise: OK`，`mise install` 装好 go/node/pnpm，镜像内 `mise --version` 为 `2026.11.7` |
| `mise run --skip-tools --dry-run dist`（`MISE_TASK_RUN_AUTO_INSTALL=false`）、`mise run --skip-deps --dry-run build-binary`、`mise x go -- go version`、`mise exec -- pnpm --version` | 都正常（Dockerfile 与文档里用到的写法） |
| `bash scripts/check-pinned-releases.sh --gate` | `OK: every pinned release is at least 7 days old`（mise 7.3 天） |

没有跑：完整 Docker 构建（只构建到 mise 安装层，后面的 `mise run dist` 与 mise 无关且很慢，CI 的 release 冒烟会跑）、
arm64 的实际运行（只核对 sha256）、`e2e`/`e2e-ha`（改动只碰 mise 引导，不碰运行时代码）。
