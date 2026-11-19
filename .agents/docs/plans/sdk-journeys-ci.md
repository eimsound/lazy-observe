# SDK journey 接入 mise/CI，修复 browser performance-journey 选择器 实施计划

## 目标与来源

- 源需求：切片任务「把四个 SDK 的 journey 接进 mise 和 CI，并修 browser 的 performance-journey 选择器」。
  journey 此前没有接入任何 mise 任务，CI 也不跑，只能对着手动起好的平台执行，所以 ADR 0010 之后
  读请求还带项目 Bearer、必然 403 这件事一直没被发现（PR #9 已把凭据修好）。
- 最终状态：
  1. `mise run sdk-journeys` 用临时 data_dir 和空闲端口起真实平台，依次跑 Go、Node、Python、Browser
     四个 journey，无论成败都清理平台、子进程和临时目录；任一工具链缺失直接失败，不 skip。
  2. `ci.yml` 新增 `sdk-journeys` job，由新的 `sdk_journeys` 改动面标志控制；
     `scripts/ci-changes.sh`、`scripts/ci-changes-test.sh`、`scripts/check-e2e-contract.sh` 同步。
  3. `sdk/browser/e2e/performance-journey.spec.ts` 不再等一个不存在的 Performance heading，
     改用页面上真实存在的 role 选择器；不改 web 页面。
  4. 「journey 只能手动跑、mise/CI 不调用」的说法全部改掉。
- 范围外：不改 `requirements.md`、`.agents/skills/**`；不加依赖；不改 web 页面 DOM；不跑性能门禁；
  不改 `CHANGELOG.md`（按 `do` 流程另起 `chore/changelog-<topic>` 切片）。

## 仓库证据

- 四个 journey：`sdk/go/e2e.sh`、`sdk/node/e2e.sh`、`sdk/python/e2e.sh`、`sdk/browser/e2e.sh`
  （+ `sdk/browser/e2e/*.spec.ts`）。都要求 `LO_E2E_PLATFORM_URL`、`LO_E2E_TOKEN`（`lo_`，只拼摄取 DSN）、
  `LO_E2E_ADMIN_PASSWORD`（读请求走 admin 会话 Cookie + `X-LO-Project: default`，ADR 0010）。
  示例端口：go 43320/43330，node 43420/43421/43430，python 43520，browser 43520/43530/43540/43550，
  其中 python 与 browser 都从 43520 起，所以四个 journey 只能串行。
- 没有任何 mise 任务或 CI job 调用它们：改前 `rg -n 'e2e\.sh' mise.toml .github` 只有服务端 `scripts/e2e*.sh`；
  `scripts/e2e.sh`、`scripts/check-e2e-contract.sh` 的注释写着「mise 和 CI 都不调用」「暂不进 CI」；
  `sdk/browser/README.md`、`sdk/python/README.md` 写着「neither `mise` nor CI runs it」。
- 平台启动的先例：
  - `scripts/check-release-smoke.sh`：空 cwd + `env -i` 白名单环境启动真实二进制，等 `/readyz`，从日志读
    `initial admin password written … path=` 与 `created default project … project_api_token=`；
  - `internal/gateway/e2e_test.go:109`：`server --listen <addr> --storage-bin-dir <dir> --data-dir <dir>`，
    `LO_SELF_MONITORING_ENABLED=false`；
  - `scripts/e2e-multi-replica.sh`：`LO_E2E_BINARY` 指向二进制、`LO_E2E_STORAGE_BIN_DIR` 指向 storage。
- bootstrap 凭据（`cmd/lazy-observe/server_dependencies.go:206` `bootstrapAdmin`，
  `internal/meta/bootstrap_admin.go`）：设了 `security.admin_password_file`（env
  `LO_SECURITY_ADMIN_PASSWORD_FILE`）就用该密码建 admin，不走「生成密码 + 强制首次改密」（PR #12/#18）；
  默认项目首次创建时日志打印一次 `project_api_token=lo_…`（`internal/meta/store.go:1124`）。
  → 已知密码 + 项目 token 不需要改密流程，也不需要改服务端。
  PR #56 的 `admin reset-password` 与 PR #60 的平台密钥指纹对「新 data_dir + 新库」没有影响（实测通过）。
- 端口：`--component-port-offset`（`internal/config/config.go:338`，0–30000）整体平移 18428/19428/20428/22428
  四个受管组件端口，`--listen` 指定网关，所以能避开宿主机上已在跑的平台。
- 工具链：`mise run build` = `web-assets`（pnpm install/build + 嵌入）+ `build-binary`；
  `mise run storage-bins` 下载并校验 Victoria 系二进制到 `.cache/storage-bin`；
  `mise run sdk-python` 建 `sdk/python/.venv` 并跑 pytest。Browser journey 自己 `pnpm install`、
  构建 SDK、`playwright install --with-deps --only-shell chromium`（要 root 或免密 sudo）。
- Performance 页（`web/src/routes/performance-page.tsx`）没有 heading：`PageActions` 注释写明
  「page title and description are intentionally absent」，标题只在左侧导航和顶栏面包屑
  （`web/src/components/layout/breadcrumbs.tsx`，`<nav aria-label="Breadcrumb">`）里。
  真实可访问性树（Playwright `error-context.md`，改前失败时抓取）：
  `navigation "Breadcrumb": lazy-observe Performance`、`tablist` 下 `tab "Web Vitals" [selected]` /
  `tab "Backend endpoints"`、`tabpanel "Web Vitals"`。`Web Vitals` 在 en/zh 两个 locale 里文案相同
  （`web/src/lib/i18n/locales/{en,zh}.ts` 的 `performance.tabVitals`）。
  所以 `getByRole("heading", { name: /Performance|性能/ })` 永远匹配不到。
- CI 现状：`changes` job 输出布尔标志；业务 job `needs: changes` 且 `if: needs.changes.outputs.<flag> == 'true'`；
  `ci-ok` 汇总；`scripts/check-github-workflows.sh` 强制这些，并要求所有 action 按 40 位 SHA 固定、
  `changes` 的每个标志都在 `ci.yml` 里声明为 output。工具由 `scripts/ci-setup-mise.sh` 的 profile 安装
  （`MISE_CI_PROFILE`）。

## 备选路线

### 路线 A（采用）：新增 `sdk-journeys` mise 任务 + 独立 CI job

- 形态：`scripts/sdk-journeys.sh` 负责编排（启动平台、取凭据、串行跑四个 journey、trap 清理），
  `mise.toml` 的 `sdk-journeys` 任务 `depends = ["build", "storage-bins", "sdk-python"]` 后调用它；
  `ci.yml` 新增 `sdk-journeys` job（`sdk_journeys` 标志）和 `sdk-journeys` mise profile。
- 权衡：多一个 job、PR 机时上升；换来 journey 与单测失败彼此独立、可单独设超时与缓存。
- 失败模式：端口被占（脚本随机挑空闲端口并重试 50 次）、平台起不来（等 `/readyz` 超时并转储脱敏日志）、
  某个 journey 挂了（继续跑完其余三个，汇总后整体失败）、中途被信号打断（trap 转发给 journey 脚本再停平台）。

### 路线 B（不采用）：挂进现有 job

- 形态：在 `e2e` 矩阵某个分片末尾起平台跑 journey，或在 `sdk-go/node/browser/python` 各自 job 里跑自己那条。
- 排除它的具体约束：
  - `e2e` 的分片并集必须等于网关测试全集（`scripts/check-e2e-contract.sh`），塞进 journey 会稀释这条契约，
    也拖长已经 90 分钟超时的 job；
  - 四个 SDK job 各自跑一条，每个 job 都要自己重复构建带前端的二进制、下载 storage、起平台，
    总机时更高，且 python 与 browser 的端口约定本来就假设串行；
  - SDK 单测改动会连带起真实平台，把「单测失败」和「journey 失败」混在一起。

## 改动面与 CI 成本

新增标志 `sdk_journeys`：

| 改动路径 | 触发 |
| --- | --- |
| `sdk/go/**`、`sdk/node/**`、`sdk/browser/**`、`sdk/python/**`、其它 `sdk/**`（`*.md` 先被文档规则挡掉） | 是 |
| `cmd/**`、`internal/**`、`go.mod`、`go.sum`、ent 生成链路 | 是（journey 验的是真实网关） |
| `web/**`、`scripts/prepare-web-assets.sh` | 是（browser journey 登录真实 UI） |
| `scripts/download-storage-bins.sh`、`scripts/sdk-journeys.sh` | 是 |
| `.github/**`、`mise.toml`、`scripts/ci-*`、`Dockerfile*`、未分类路径 | 是（原本就全量） |
| 文档、`deploy/**`、`bench/**`、`config.example.yaml`、HA/单副本 e2e 脚本 | 否 |

- 时长（本机实测，见「执行日志」）：服务端二进制和 storage 已就绪时，四个 journey 合计约 4 分 9 秒（`real 4m8.9s`）；
  其中 Go 96 秒（含 70 秒连续 profiling 窗口）、Node 87 秒、Python 11 秒、Browser 53 秒（4 个用例）。
  CI 冷缓存额外成本：`mise run build` 的前端构建约 3–4 分钟（pnpm store 命中后更短）、storage 归档命中缓存后秒级
  （冷启动下载约 117 MB + 若干小包，本机约 30 秒）、`playwright install --with-deps` 约 1–2 分钟、
  三个 pnpm 工作区 install 与 SDK 构建约 1 分钟。**估算新增一个 job，冷缓存 12–18 分钟、缓存命中后 8–12 分钟**（runner 比本机慢，journey 本身按 5–8 分钟算）（`timeout-minutes: 45`），
  与现有 `e2e` 分片（耗时 6+ 分钟）并行，不延长 PR 总墙钟，除非它成为最长的 job。
- 触发面：SDK、服务端 Go、web 是这个仓库最常见的改动面，所以 PR 的 CI 机时会明显上升（每个这类 PR 多约 15–20 分钟 runner 时间）。
  不为了省机时去缩小触发面：这些路径任一变化都可能让 journey 变红，而 journey 恰好是唯一覆盖
  「真实平台 + 真实 SDK + 真实 UI」的门禁。若后续觉得太贵，首选收窄 `web/**`（只在 Performance/Traces/Logs/
  Profiling/Metrics 路由变化时跑）或把 `internal/**` 里与 SDK 无关的包排除，而不是退回手动。
- 缓存：mise 目录、Go（modcache + build cache）、pnpm store（web + sdk/node + sdk/browser 三个锁文件共用一个键）、
  storage 归档（与 `e2e` 共用键）、Playwright 浏览器（`~/.cache/ms-playwright`，键为 `sdk/browser/pnpm-lock.yaml`）。

## 测试接缝

- `mise run sdk-journeys`：一条命令证明四个 journey 对着真实平台通过，用户和 CI 看到的就是这个行为。
  日常迭代用 `LO_SDK_JOURNEYS_ONLY=browser bash scripts/sdk-journeys.sh` 只跑子集（先要有 `bin/lazy-observe`、
  `.cache/storage-bin`、`sdk/python/.venv`）。
- `scripts/ci-changes.sh --stdin`：改动面分组，由 `mise run ci-changes-test` 钉住。
- `mise run workflow-check`（含 `check-github-workflows.sh`、`check-pipefail-pipelines.sh`、
  `check-e2e-contract.sh`、`ci-changes-test.sh`）：新 job 的静态契约（`ci-ok.needs`、SHA 固定、output 声明、
  mise 任务与 job 的对应、「缺工具不 skip」）。
- Playwright `performance-journey`：UI 选择器在真实页面上可达。
- 不改 web DOM：`PageActions` 的注释已经说明列表页刻意不放标题，补 heading 会改变既有设计，
  而页面上已经有稳定的 `tab` 与 `navigation` role 可用，只改测试选择器。

## 依赖顺序与切片

1. 选择器修复（独立）。
2. `scripts/sdk-journeys.sh` + mise 任务（browser 那条要绿后才能整体绿）。
3. CI 改动面与 job（依赖 2：job 跑的就是 2 的任务）。
4. 文档与注释（依赖 2、3）。

本任务按要求只出一个 commit（一个切片 = 一个 commit）；下面按 Tracer Bullet 的顺序勾选并贴证据。

- [x] 1. 修复 performance-journey 选择器
  - 涉及：`sdk/browser/e2e/performance-journey.spec.ts` 的 `loginUI` 与登录后的页面断言。
  - Red：`LO_SDK_JOURNEYS_ONLY=browser bash scripts/sdk-journeys.sh` → `performance-journey` 在
    `getByRole('heading', { name: /Performance|性能/ })`（`performance-journey.spec.ts:57`，15 秒超时）失败：
    `3 passed, 1 failed`。
  - Green：就绪判定改为 `getByRole("tab", { name: "Web Vitals" })`；页面标识改为断言
    `getByRole("navigation", { name: /Breadcrumb|面包屑/ })` 含 `Performance|性能`。
  - Verify：同一命令 → `4 passed (33.0s)`，`=== PASS SDK journey: browser (50s) ===`。
- [x] 2. `scripts/sdk-journeys.sh` 与 `mise run sdk-journeys`
  - Red：任务不存在时 `mise run sdk-journeys` 报 `no task sdk-journeys found`。
  - Green：脚本 + 任务 + `sdk/browser/e2e.sh` 的 `playwright install` 只在有 root/免密 sudo 时带 `--with-deps`
    （否则 CI 之外的无 sudo 开发机根本跑不起来；CI runner 有免密 sudo，行为不变）。
  - Verify：`mise run --skip-deps sdk-journeys`，见「执行日志」。
- [x] 3. CI 改动面与 job
  - Red：`printf 'sdk/go/e2e.sh\n' | bash <(git show origin/main:scripts/ci-changes.sh) --stdin | rg -c sdk_journeys` 无输出（rc=1）；
    `ci-changes-test` 里新增的期望在旧脚本下失败。
  - Green：`ci-changes.sh` 加标志与分组、`ci-changes-test.sh` 同步、`ci.yml` 加 job 与 `ci-ok.needs`、
    `ci-setup-mise.sh` 加 `sdk-journeys` profile、`check-e2e-contract.sh` 加静态契约。
  - Verify：`mise run ci-changes-test`、`mise run workflow-check`、`mise run e2e-contract-check`。
- [x] 4. 文档与注释：`scripts/e2e.sh`、`scripts/check-e2e-contract.sh`、四个 `sdk/*/e2e.sh` 头注释、
  `sdk/{browser,python}/README.md`、`CONTRIBUTING.md`、`sdk/AGENTS.md`。
  Verify：`rg -n 'neither .mise. nor CI|mise 和 CI 都不调用|暂不进 CI' .` 无命中。

## Green 后审查

- [x] `scripts/sdk-journeys.sh` 的清理路径：平台启动失败、`/readyz` 超时、journey 中途失败、收到 TERM/INT；
  跑完后 `/tmp/lazy-observe-sdk-journeys.*` 不存在、`pgrep -f` 找不到平台或 Victoria 子进程。
- [ ] CI 真实 runner 上的时长与缓存命中情况：本地无法验证，交给审查线程推送后的 PR run 确认。

## 未决事项

- 本机约定不使用 pip，所以本地用 `mise run --skip-deps sdk-journeys`，并先用 `uv` 建出同样带 `[test]` 额外依赖的
  `sdk/python/.venv`；`build`、`storage-bins` 两个依赖分别单独跑过。完整的依赖链（含 `sdk-python`）只会在 CI 上跑。
- `sdk-python` mise 任务用 `python -m venv` + `pip` 建 venv 并跑 pytest；`sdk-journeys` 依赖它，所以
  journey job 里会多跑一遍 pytest（约 20–30 秒）。要避免的话需要把「建 venv」从 `sdk-python` 里拆出来，
  那是另一个切片。
- 本机没有免密 sudo，Browser journey 依赖宿主机已装好的 Chromium 系统库；CI runner 上 `--with-deps` 自动生效。

## 执行日志

以下均在本 worktree（`slice/sdk-journeys-ci-2`，基于 `origin/main` `2962ca1`）真实执行：

- `mise run build`、`mise run storage-bins` → 成功（`build rc=0`、`storage rc=0`）。
- `uv venv --python 3.13 sdk/python/.venv` + `uv pip install -e "sdk/python[test]"` → 成功。
  （本机约定不用 pip，所以没有跑会调用 pip 的 `mise run sdk-python`，见「未决事项」。）
- Red：`LO_SDK_JOURNEYS_ONLY=browser bash scripts/sdk-journeys.sh` → rc=1，`3 passed, 1 failed`，失败点
  `getByRole('heading', { name: /Performance|性能/ })`，`=== FAIL SDK journey: browser (exit 1, 65s) ===`。
- Green：同一命令 → rc=0，`4 passed (33.0s)`，`=== PASS SDK journey: browser (50s) ===`。
- `mise run --skip-deps sdk-journeys` → rc=0，`real 4m8.9s`：Go 11 条 PASS（96 秒）、Node 10 条 PASS（87 秒）、
  Python `e2e passed`（11 秒）、Browser 4 passed（53 秒）；`PASS: all SDK journeys (go node python browser)`。
  之后 `/tmp/lazy-observe-*` 不存在，没有残留的平台、Victoria 或示例进程。
- 失败路径：`LO_SDK_JOURNEYS_ONLY=bogus`、`PATH=/usr/bin:/bin`（缺 jq）、缺 `sdk/python/.venv`、缺二进制
  → 均 rc=1 并给出明确原因，没有 skip；`LO_SDK_JOURNEYS_ONLY=go` 跑到一半发 `SIGTERM` → rc=143，
  临时目录与进程全部清理。
- `mise run ci-changes-test`、`mise run e2e-contract-check`、`mise run workflow-check` → PASS。
- `mise run sdk-go`、`mise run sdk-node`、`mise run sdk-browser` → rc=0；Python 用 venv 里的
  `python -m pytest -q tests` → `63 passed`。
- `bash -n`（新增和改动的 bash）、`sh -n`（四个 `sdk/*/e2e.sh`）、`bash scripts/check-pipefail-pipelines.sh` → ok。
- `rg -n 'neither .mise. nor CI|mise 和 CI 都不调用|暂不进 CI|manual journey' .` → 无命中。
