# GitHub Actions Cache 超限治理（10 GB 配额）

> 状态：一期、二期与三期治理调研及实施方案记录（Issue #300、#310、#311），2026-11-21。
> 数据来源：只读查询 `gh api repos/eimsound/lazy-observe/actions/cache/usage` 与 `actions/caches --paginate`（一期 2026-11-20 12:18 UTC，对应 `main` `714a21f`；二期 2026-11-21 17:54–19:00 UTC，对应 `main` `92610f3`；三期 2026-11-21 20:05 UTC 后对应 `f58eb5a`，以及 #310 合入 `2150509` 后 `mise-bin-*` 实测）。

## 1. 现状盘点

GitHub Actions 单仓缓存配额上限为 **10 GB**（超出后按 LRU 淘汰最久未访问条目，7 天未访问自动过期）。
观测期间仓库用量多次超出配额：此前峰值达到约 **16 GB（75 条）** 和 **12.08 GB**，曾导致 `embedded-pg`（37 MB）在 `main` 刚命中 32 分钟后就在 PR 上被 LRU 淘汰（2026-11-19 23:46 UTC）。

2026-11-20 12:18 UTC 快照：

- **总量**：`11,695,102,536 bytes`（**10.89 GiB / 11.70 GB**），共 **54 条**。
- **按 ref 分布**：此时全部 54 条均在 `refs/heads/main`（PR ref 上的旧条目已在 Go 1.27.4 升级落地触发 `main` 全量 CI 时被 LRU 挤掉；平时 Renovate 与切片 PR 改 `go.mod` / `mise.toml` / `pnpm-lock.yaml` 时会在 `refs/pull/<N>/merge` 写入大量一次性副本，推高至 16 GB）。

### 按 key 前缀汇总（2026-11-20 12:18 UTC）

| key 前缀 | 条数 | 总大小（MiB） | 字节数 | 说明 |
| --- | ---: | ---: | ---: | --- |
| `go-Linux` | 4 | 2,239.8 | 2,348,566,684 | X64 + ARM64 各 2 代（Go 1.26 与 Go 1.27）；每条含 `mod` + `go-build` |
| `go-sdk-journeys` | 2 | 1,283.2 | 1,345,542,863 | 与 `go-Linux-X64` 重复，单条已膨胀到 755.0 MB |
| `mise-sdk-node` | 5 | 1,126.5 | 1,181,268,642 | 3 个 Node 版本 × 两代；单条 216–261 MB（误装全套 Go/dev 工具） |
| `mise-sdk-browser` | 4 | 886.0 | 929,017,368 | 与 `mise-sdk-node` 完全重复 |
| `go-sdk` | 2 | 680.9 | 713,955,850 | 与 `go-Linux-X64` 高度重复（单条 379.5 MB） |
| `mise-go-cross` | 3 | 609.3 | 638,915,886 | base 工具集（X64 + ARM64） |
| `mise-sdk-go` | 2 | 425.8 | 446,510,761 | `mise-go` 的子集，单独存两代 |
| `mise-e2e` | 2 | 397.2 | 416,527,990 | 与 `mise-go-cross`（X64）完全重复 |
| `mise-sdk-python` | 2 | 397.2 | 416,525,161 | 与 `mise-go-cross`（X64）完全重复（未加 `--skip-tools`） |
| `mise-e2e-ha` | 2 | 397.2 | 416,469,842 | 与 `mise-go-cross`（X64）完全重复 |
| `go-meta-pg` | 2 | 363.0 | 380,619,622 | 与 `go-Linux-X64` 重复 |
| `mise-go` | 1 | 340.6 | 357,100,758 | `go-ci` 工具集（叠加了旧版 Go 1.26） |
| `storage-native` | 1 | 333.0 | 349,176,419 | 原生架构 Victoria 二进制归档（关键缓存） |
| `mise-sdk-journeys` | 1 | 257.5 | 270,013,789 | 与 `mise-base` 工具集几乎一致 |
| `mise-e2e-restart` | 1 | 231.2 | 242,399,488 | 与 `mise-go-cross`（X64）完全重复 |
| `mise-web` | 1 | 231.1 | 242,281,716 | 与 `mise-go-cross`（X64）完全重复 |
| `pnpm-web` | 4 | 227.6 | 238,705,953 | X64 + ARM64 各 2 代（因 key 含 `mise.toml` 哈希，无锁文件改动也换新） |
| `mise-meta-pg` | 2 | 187.4 | 196,518,874 | 单条从 61.2 MB 翻倍到 126.3 MB（叠加了 Go 1.26 + 1.27） |
| `pnpm-sdk-journeys` | 2 | 169.8 | 178,058,393 | 同上，因 key 含 `mise.toml` 哈希而换新 |
| `playwright` | 1 | 113.9 | 119,414,276 | Chromium 浏览器二进制 |
| `pnpm-sdk-browser` | 2 | 100.7 | 105,625,143 | 同上，因 key 含 `mise.toml` 哈希而换新 |
| `pnpm-sdk-node` | 2 | 65.4 | 68,532,240 | 同上，因 key 含 `mise.toml` 哈希而换新 |
| `uv-sdk-python` | 5 | 52.1 | 54,681,964 | 5 个 Python 版本的 uv 包缓存 |
| `embedded-pg` | 1 | 36.9 | 38,672,854 | Postgres 18.6.1 发行包（关键缓存，曾被淘汰） |
| **合计** | **54** | **11,153.3** | **11,695,102,536** | **10.89 GiB（超 10 GB 配额）** |

## 2. 根因分析

1. **PR、integration 分支与 release tag 都在写缓存**：原 workflow 全部使用复合 `actions/cache`，任何改动 `go.mod` / `mise.toml` / `pnpm-lock.yaml` 的 PR 都会在 `refs/pull/<N>/merge` 写入 2–5 GB 一次性缓存；`release.yml` 的 `docker/build-push-action` 还在 `refs/tags/*` 下写 `mode=max` 层缓存，其它 tag 与 `main` 均无法读取。
2. **同类内容按 job 拆成大量重复 key**：
   - `mise-go-cross`、`mise-e2e`、`mise-e2e-restart`、`mise-e2e-ha`、`mise-web`、`mise-sdk-python`、`mise-bench`、`mise-performance-gates` 缓存了 8 份内容完全相同的 231 MB base 工具集；`mise-sdk-node` 与 `mise-sdk-browser` 重复存了 6 份；`mise-sdk-go`、`mise-sdk-journeys`、`mise-meta-pg` 也是现有工具集的子集。
   - Go 侧 `go-Linux-X64`、`go-sdk-journeys`、`go-sdk`、`go-meta-pg`、`go-bench`、`go-release` 各自保存，单代就占去 2.5+ GB；`~/go/pkg/mod` 还随 X64 与 ARM64 重复存了两份。
3. **易变输入污染 key**：
   - 原 `go-*` 与 `pnpm-*` 的 key 全部混入了整份 `mise.toml` 的哈希，哪怕只改了 `helm`/`rg` 或任务注释，也会导致全部 Go、pnpm、mise 缓存同时换新。
   - `mise-*` 用 `hashFiles('mise.toml')` 覆盖整份 613 行文件（近 14 天改了 30 次，约八成仅改 `[tasks.*]` 或注释，未动 `[tools]`）。
4. **`restore-keys` 与缺省 `mise run` 导致单条缓存滚雪球膨胀**：
   - Go 1.26 → 1.27 升级时，`go-Linux-X64` 通过 `restore-keys: go-Linux-X64-` 把 1.26 的 `~/.cache/go-build` 恢复出来再叠写 1.27 产物，单条从 475.6 MB 涨到 662.8 MB（`go-sdk-journeys` 涨到 755.0 MB）；旧版 Go 对象文件无法被新 Go 复用，纯占空间。
   - `mise-*` 同理：升级 Go 后 `mise-meta-pg` 从 61.2 MB 翻倍到 126.3 MB，`mise-e2e` 从 166.2 MB 涨到 231.1 MB，旧工具版本永远留在 `~/.local/share/mise`。
   - `sdk-node`、`sdk-browser`、`sdk-python` 运行 `mise run` 未加 `--skip-tools`，mise 自动把 `mise.toml` 里全部非 lazy 工具（`go`、`node@26`、`air`、`golines`、`actionlint`、`rg`、`jq`）都装进了这些 job 的目录，把原本约 50 MB 的 Node 工具集撑大到 260 MB。

## 3. 实施方案与设计取舍

### 改动概览（4 个文件）

1. **只读恢复、仅 `main` 保存（`ci.yml`、`bench.yml`、`release.yml`）**：
   - 全部改用已固定同一 40 位 SHA 的 `actions/cache/restore` 与 `actions/cache/save`（`55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0`），不引入新 action 属主。
   - 所有保存步骤统一加 `if: github.ref == 'refs/heads/main' && steps.<id>.outputs.cache-hit != 'true'`，确保 PR、integration、tag 只读不写，且精确命中时不重复上传。
   - 严格单一 writer：同一 workflow 运行内每个共享 key 只有一个 job/矩阵格保存，避免并发 `actions/cache/save` 竞争——`go` 保存 `mise-go`/`gomod`/`gobuild(X64)`，`go-cross` 保存 `mise-base(X64/ARM64)` 与 `gobuild`/`pnpm-web(ARM64)`，`web` 保存 `pnpm-web(X64)`，`e2e(storage)` 保存 `storage-native`，`sdk-node` 保存 `mise-sdk-node-*` 与 `pnpm-sdk-node(Node 26)`，`sdk-browser(Node 26)` 保存 `pnpm-sdk-browser`；`e2e-restart`、`e2e-ha`、`sdk-go` 全部只读，`sdk-browser` 对 `mise-sdk-node-*` 只读，`sdk-journeys` 对 `storage-native` 只读。
   - `release.yml`：（一期做法，已于三期 #311 彻底移除 BuildKit GHA 缓存，见 §7）`container-check` 的 BuildKit `cache-to` 仅在 `refs/heads/main`（手动 snapshot 选 `main`）时写入；只在 tag push 运行的 `docker` job 移除 `cache-to`，只读 `container-amd64` / `container-arm64`。
2. **拆分 `GOMODCACHE` 与 `GOCACHE`，合并重复 Go key（`scripts/ci-cache-keys.sh` + 三个 workflow）**：
   - `gomod-${{ steps.cache-keys.outputs.go }}`（`~/go/pkg/mod`）：纯源码，**跨架构且跨 Go 版本共享**，仅由 `go` job 在 `main` 上保存一份，其余全部 job（含 ARM64、`bench.yml`、`release.yml`）只读复用。
   - `gobuild-${{ runner.os }}-${{ runner.arch }}-${{ steps.cache-keys.outputs.go-toolchain }}-${{ steps.cache-keys.outputs.go }}`（`~/.cache/go-build`）：按架构与精确 Go 版本隔离，**`restore-keys` 仅回退到同一 `go-toolchain`**，绝不跨 Go 版本恢复旧编译产物；仅由 `go` job（X64）与 `go-cross` 的 `arm64` 格保存，彻底取消 `go-sdk-journeys`、`go-sdk`、`go-meta-pg`、`go-bench`、`go-release`。
3. **收口并瘦身 `mise-*` 缓存，清理旧版工具（`scripts/ci-cache-keys.sh` + 三个 workflow）**：
   - `ci-cache-keys.sh` 只对 `mise.toml` 的 `[tools]` 表（去注释和空行）计算 `mise` 哈希，并单独输出 `go-toolchain` 与 `pnpm-toolchain`。修改 `[tasks.*]` 或注释不再触发任何 `mise-*` 缓存换新。
   - `mise-base` 统一承接 `go-cross`、`e2e`、`e2e-restart`、`e2e-ha`、`web`、`meta-pg`、`sdk-python`、`sdk-journeys`、`bench`、`release:performance-gates`；`sdk-go` 改为只读复用 `mise-go`（安装 `MISE_CI_TOOLS: go golangci-lint`）并运行 `mise run --skip-tools sdk-go`，消除 `mise-sdk-go` 与 `mise-sdk-journeys`。
   - `sdk-node` 与 `sdk-browser` 共用 `mise-sdk-node-<node>-pnpm-<pnpm>-<os>-<arch>`，取消跨版本 `restore-keys`，并以 `mise run --skip-tools` 运行任务（`sdk-python` 同样加 `--skip-tools`），不再拉入 Go/air/golines 等无关工具；实测 `mise exec node@22.23.5` 仅加载指定 Node 与已装 pnpm，单格可从 ~260 MB 降至 ~50 MB。
   - 保存 `mise-go`、`mise-base`、`mise-release` 前先在 `main` 上执行 `mise prune --tools --yes`（实测保留 `[tools]` 中包含 `lazy = true` 在内的当前版本，清除从 `restore-keys` 带入的旧版工具），防止缓存跨升级单调膨胀。
4. **`pnpm-*` 去掉 `mise.toml` 哈希**：仅按各自 `pnpm-lock.yaml` 哈希，`release.yml` 复用 `pnpm-web`。

### 明确不做的取舍

- **不新增定时清理 cache 的 workflow**：调用缓存删除 API 需要 `actions: write`，会扩大权限面；根因是写入失控与重复 key，控制住稳态体积在 10 GB 以下即可交给 GitHub 7 天 TTL 自然回收。
- **不在 `sdk-node` / `sdk-browser` 里运行 `mise prune`**：矩阵中 `node@22.23.5` / `node@24.22.3` 不在 `mise.toml` 中，`mise prune` 会误删它们；改用精确版本 key 且不设跨版本 `restore-keys`，直接避免旧版本残留。
- **不给 `e2e` / `e2e-ha` / `go-cross` / `sdk-journeys` 加 `--skip-tools`**：`scripts/check-e2e-contract.sh` 断言它们的原命令字面量，且它们共用同一份 `mise-base`，加与不加不影响总条目数。

## 4. 预期总容量估算（按 #300 / #310 / #311 实测修正）

### 单代稳态（`main` 日常 CI）

| 类别 | key 模式 | 条数 | 实测/估算单条 | 估算小计 |
| --- | --- | ---: | ---: | ---: |
| Go 模块（X64/ARM64 共用） | `gomod-<go>` | 1 | ~412 MiB | ~412 MiB |
| Go 编译缓存（X64） | `gobuild-Linux-X64-<go-ver>-<go>` | 1 | ~292 MiB | ~292 MiB |
| Go 编译缓存（ARM64） | `gobuild-Linux-ARM64-<go-ver>-<go>` | 1 | ~176 MiB | ~176 MiB |
| mise（go-ci，已 prune） | `mise-go-Linux-X64-<tools>` | 1 | ~206 MiB | ~206 MiB |
| mise（base X64 + ARM64，已 prune） | `mise-base-Linux-{X64,ARM64}-<tools>` | 2 | ~170 / ~162 MiB | ~332 MiB |
| mise 二进制本体（#310 新增，X64 + ARM64） | `mise-bin-Linux-{X64,ARM64}-<mise-release.env>` | 2 | 55.60 / 55.88 MiB | ~111.5 MiB |
| mise（sdk-node 22/24/26，`--skip-tools`，#311 键升级） | `mise-sdk-node-v2-<ver>-pnpm-<ver>-Linux-X64` | 3 | ~45–60 MiB | ~150 MiB |
| storage 原生归档（#300 清理历史归档） | `storage-native-Linux-X64-<storage>` | 1 | ~180 MiB | ~180 MiB |
| pnpm（web×2、node、browser；已移除 `pnpm-sdk-journeys`） | `pnpm-*-Linux-*` | 4 | 45–60 MiB | ~214 MiB |
| Playwright 浏览器（按 `playwright-core` 版本） | `playwright-sdk-journeys-Linux-X64-<playwright-core>` | 1 | ~113.4 MiB | ~113.4 MiB |
| uv（5 个 Python 版本） | `uv-sdk-python-Linux-X64-<py>-<toml>` | 5 | ~13.4 MiB | ~66.8 MiB |
| embedded-pg | `embedded-pg-Linux-X64-18.6.1-<mod>` | 1 | 36.9 MiB | 36.9 MiB |
| uv（docs 文档站） | `uv-docs-Linux-X64-<toml>` | 1 | 16.4 MiB | 16.4 MiB |
| **日常 CI 单代合计** | | **24** | | **~2.25 GiB（~2.42 GB）** |

### 7 天窗口峰值估算（含 Renovate 密集升级 + 手动发版演练）

- **高频升级周**（7 天内合并 3 次 `go.mod` 升级 + 2 次 `mise.toml [tools]` 升级 + 2 次 `pnpm-lock.yaml` 升级，旧代尚未过 7 天 TTL）：
  - Go 缓存（3 代 × ~880 MiB）：~2.64 GiB（~2.83 GB）
  - `mise-go` + `mise-base`（2 代 × ~538 MiB）+ `mise-bin`（1 代 ~111.5 MiB）+ `mise-sdk-node-v2`（1 代 ~150 MiB）：~1.34 GiB（~1.44 GB）
  - `pnpm-*`（2 代 × ~214 MiB）：~428 MiB（~0.45 GB）
  - 长期稳定资产（`storage-native` + `playwright` + `uv` + `embedded-pg` + `uv-docs`）：~414 MiB（~0.43 GB）
  - **CI 峰值合计**：**~5.15 GB**（稳定低于 10 GB 配额）。
- **叠加手动在 `main` 上跑 `bench.yml` / `release.yml` snapshot**：
  - `release.yml` BuildKit GHA 缓存预算：**0 GB**（#311 已移除 `cache-from`/`cache-to`，并由 `scripts/check-github-workflows.sh` 静态禁止；原估 ~1.5–2.5 GB 已被 §7 实证推翻——两次 snapshot 即达 `3.60 GiB` / 67 条并击穿配额）；
  - 另加 `storage-all`（当前仍为 1 条旧键 `523.77 MiB`，待 #309 修复 `perf-ha` 后下次 `main` 成功运行刷新保存，预期收敛至 ~360 MiB）+ `mise-release`（~220 MiB）；
  - **极端峰值**：**~5.7–6.0 GB**（留有 >4.0 GB 余量），关键缓存（`embedded-pg`、`storage-native`、`mise-*`、`gomod`）不再被 LRU 挤出。

> 过渡期说明：本改动合入 `main` 后，旧 key（约 11.7 GB）将在 7 天内随无人访问自然过期，或在短时间内被新 key（~2.2 GB）挤出旧代，一周后回落至稳态区间。

## 5. 待办：合入一周后复查

- [x] 合入 `main` 后已通过二期排查（2026-11-21）确认真实瓶颈并落地修复（见 §6）：
  - `refs/pull/*` 与 `refs/tags/*` 已归零（全部命中条目均位于 `refs/heads/main`）；
  - 已在 #300 中清理 10 处 `mise-go-cross-*` 过渡 `restore-keys`；
  - `gomod-*` 冷启动基线修正为 ~412 MiB（非 ~200 MB），`gobuild-*`、`playwright-sdk-journeys-*`、`pnpm-sdk-journeys-*`、`storage-native-*` 膨胀原因与治理方案见 §6；
  - `mise-sdk-node-*` 裸 `mise exec --` 膨胀（6 条 / 1,111.19 MiB）另单治理（#299）。

## 6. 二期治理（2026-11-21 / Issue #300）

### 6.1 实时水位与前缀盘点（2026-11-21）

一期治理合入约 30 小时后，仓库全部缓存均位于 `refs/heads/main`，但总体积再次突破 10 GB 配额并触发 GitHub LRU 淘汰：

- **LRU 淘汰前（2026-11-21 17:54 UTC）**：`12,459,279,085 bytes`（**11.60 GiB / 12.46 GB**，118 条），其中日常 CI 缓存 **9.78 GiB（10.50 GB，75 条）** + 手动 `release.yml` snapshot 产生的 BuildKit 缓存 **1.83 GiB（1.96 GB，43 条）**。
- **LRU 淘汰后（2026-11-21 18:02 UTC；19:00 UTC 复查维持同水位）**：`10,638,926,276 bytes`（**9.91 GiB / 10.64 GB**，`actions/caches --paginate` 列出 97 条 / `usage` 接口计数 96），其中日常 CI **8.08 GiB（8.68 GB，54 条）** + BuildKit **1.83 GiB（1.96 GB，43 条）**；`mise-go-cross-*` 实测已为 **0 条**。

2026-11-21 19:00 UTC 分前缀实测（LRU 淘汰后 97 条快照）：

| key 前缀 | 条数 | 总大小（MiB） | 字节数 | 诊断结论 |
| --- | ---: | ---: | ---: | --- |
| `buildkit-blob` + `index-container` | 43 | 1,870.08 | 1,960,926,532 | 手动 `release.yml` snapshot 遗留的 BuildKit 层缓存 |
| `gobuild-Linux-X64` | 3 | 1,335.01 | 1,399,861,338 | 同 Go `1.27.4` 下 4 代单调叠加：`291.73 → 373.75 → 448.06 → 513.20 MiB`（+75.9%） |
| `mise-sdk-node` | 6 | 1,111.19 | 1,165,166,752 | 裸 `mise exec --` 拉入无关工具（另单 #299 处理） |
| `mise-go` | 4 | 823.47 | 863,472,448 | 4 个 `[tools]` 哈希代（单条 ~205.8 MiB） |
| `gomod` | 2 | 823.34 | 863,335,183 | 单代冷基线 **~412 MiB**（431,985,155 B，§4 原估 ~200 MB 的 2.06×） |
| `mise-base-Linux-X64` | 4 | 680.58 | 713,640,142 | 4 个 `[tools]` 哈希代（单条 ~170.0 MiB） |
| `mise-base-Linux-ARM64` | 4 | 647.81 | 679,274,719 | 4 个 `[tools]` 哈希代（单条 ~161.9 MiB） |
| `storage-all` | 1 | 523.77 | 549,214,645 | 双架构归档，未清历史归档时跨升级将同样叠加 |
| `gobuild-Linux-ARM64` | 2 | 497.17 | 521,325,030 | 同 Go `1.27.4` 下 3 代单调叠加：`176.05 → 242.79 → 254.39 MiB`（+44.5%） |
| `pnpm-sdk-journeys` | 5 | 430.25 | 451,149,964 | 三锁合一 key，任一工作区变更即重存 ~85–86 MiB（淘汰前 7 条 599.75 MiB） |
| `storage-native` | 1 | 345.50 | 362,278,604 | 当前 `linux-amd64` 4 归档实际仅 180.21 MiB，死归档占比 **47.8%** |
| `playwright-sdk-journeys` | 3 | 340.67 | 357,217,545 | 锁文件无关变动导致同一 Chromium 重复存 5 条 / 568.43 MiB（淘汰后余 3 条） |
| `pnpm-web`（X64 + ARM64） | 6 | 347.38 | 364,253,219 | X64 3 条 178.17 MiB + ARM64 3 条 169.21 MiB |
| `pnpm-sdk-browser` | 3 | 154.37 | 161,866,776 | 单条 ~50.4–52.0 MiB |
| `pnpm-sdk-node` | 3 | 109.98 | 115,323,589 | 单条 ~44.6 MiB（实测最新一条 `46,812,338 B`） |
| `uv-sdk-python` | 5 | 52.19 | 54,722,992 | 5 个 Python 版本，单条 ~10.3–10.5 MiB |
| `embedded-pg` | 1 | 36.88 | 38,672,854 | 单条稳态 |
| `uv-docs` | 1 | 16.43 | 17,223,944 | 文档站构建 uv 缓存 |
| **合计** | **97** | **10,146.07** | **10,638,926,276** | **9.91 GiB / 10.64 GB** |

### 6.2 二期治理改动（Issue #300）

1. **清理 10 处已失效 `mise-go-cross-*` 过渡 `restore-keys`**：移除 `.github/workflows/ci.yml`（9 处及 `go-cross` 上方过期迁移注释）与 `.github/workflows/docs.yml`（1 处）中的过渡回退行，`rg -n "mise-go-cross" .github/` 归零。
2. **阻止 `gobuild-*` / `gomod-*` writer 在 `main` 上跨代叠写**：
   - `ci.yml` 中 `go`（`gomod`、`gobuild-X64`）与 `go-cross[arm64]`（`gobuild-ARM64`）的 `restore-keys` 在 `github.ref == 'refs/heads/main'` 时置为空字符串 `''`（实证 `actions/cache/restore@v6.1.0` 对空输入返回 `[]`，不回退前代缓存）；精确未命中时冷编生成干净单代基线（X64 `gobuild` ~292 MiB、ARM64 `gobuild` ~176 MiB、`gomod` ~412 MiB），PR 与只读消费者（`go-cross[amd64]`、`e2e`、`e2e-restart`、`e2e-ha`、`meta-pg`、`sdk-go`、`sdk-journeys`）仍保留 `restore-keys` 回退加速。
3. **收细 `playwright-sdk-journeys-*` 并消除 `pnpm-sdk-journeys-*` 重复保存**：
   - `scripts/ci-cache-keys.sh` 从 `sdk/browser/pnpm-lock.yaml` 的 `packages:` 区块精确提取唯一 `playwright-core` 版本号并输出 `playwright-sdk-browser`（缺文件/多版本/非法格式 fail-closed 退出非零）；`sdk-journeys` 改用 `playwright-sdk-journeys-${{ runner.os }}-${{ runner.arch }}-${{ steps.cache-keys.outputs.playwright-sdk-browser }}` 且移除 `restore-keys`。
   - `sdk-journeys` 移除 `pnpm-sdk-journeys-*` 恢复与 `Save pnpm cache`，改为依次只读恢复 `pnpm-web`、`pnpm-sdk-node`、`pnpm-sdk-browser`；实测 `pnpm 12.21.0` 使用 `v11/index.db` 单文件 SQLite 索引（在 PR #306 Run `44939165474` 中直接解压到同一 `~/.pnpm-store` 会导致后一份覆盖前一份 `index.db`，`web-assets` 重下 104/163 包、`sdk/node` 重下 129/190 包），因此每次恢复后立即迁至按工作区隔离的目录 `~/.pnpm-store-{web,sdk-node,sdk-browser}`，并通过 `LO_PNPM_STORE_{WEB,SDK_NODE,SDK_BROWSER}` 向 `mise.toml` `tasks.web-assets`、`sdk/node/e2e.sh`、`sdk/browser/e2e.sh` 透传可选 `--store-dir`，实现三工作区全量只读复用且彻底消除 ~86 MiB 三锁捆绑副本。
4. **清理 `scripts/download-storage-bins.sh` 历史归档残留**：
   - 在 4 个当前归档全部完成下载与 SHA-256 校验后，删除 `$bin_dir/.archives/$platform` 顶层不属于 `$metrics_asset` / `$vmutils_asset` / `$logs_asset` / `$traces_asset` 的历史归档文件及符号链接（不触碰其它平台目录），并在二进制已就绪的快路径上同样执行清理；校验失败则 fail-closed 退出不误删。
   - 新增离线自测 `scripts/download-storage-bins-test.sh` 并接入 `mise.toml` `workflow-check`。

### 6.3 修正后的单代稳态估算（含 #310 / #311 口径更新）

- 修正 `gomod`（~412 MiB）、`gobuild` 冷基线（X64 ~292 MiB + ARM64 ~176 MiB）、`storage-native`（~180 MiB）及移除 `pnpm-sdk-journeys-*` 后，结合三期 #310 与 #311 更新后的日常 CI 增量单代预期（详表见 §4）：
  - Go 侧：`gomod`（412 MiB）+ `gobuild-X64`（292 MiB）+ `gobuild-ARM64`（176 MiB）≈ **880 MiB**；
  - `storage-native` ≈ **180 MiB**（较 345.5 MiB 减少 ~165 MiB，降幅 ~48%）；
  - `pnpm-*` 4 组（`pnpm-web` X64/ARM64、`pnpm-sdk-node`、`pnpm-sdk-browser`）≈ **214 MiB**（消除 `pnpm-sdk-journeys` ~86 MiB × 多代）；
  - `playwright-sdk-journeys`（按 `playwright-core` 精确版本，1 代）≈ **113.4 MiB**；
  - `#310` 新增 `mise-bin-*`（X64 `55.60 MiB` + ARM64 `55.88 MiB`，仅由 `go-cross` on `main` 保存、不设 `restore-keys`）≈ **111.5 MiB**；
  - `#311` 将 `mise-sdk-node-*` 升级为 `mise-sdk-node-v2-*`（3 格单条回落至 ~45–60 MiB，合计 **~150 MiB**，脱离旧 556.08 MiB 膨胀条目）；
  - `#311` 将 `release.yml` BuildKit GHA 缓存预算更正为 **0 GB**（移除 `cache-from`/`cache-to`，修正原“含 BuildKit 缓存仍远低于 10 GB”的误判）；`storage-all-*` 当前仍为 1 条旧键 `523.77 MiB`（因 Run `44944072291` `performance-gates` 的 `perf-ha` 门禁失败跳过保存，待 #309 后下次 `main` 成功运行刷新）。
  - 修正后日常 CI 单代总计约 **~2.25 GiB（~2.42 GB，24 条）**，7 天多代滚动峰值约 **~4.9–5.2 GB**，叠加手动 `bench.yml` / `release.yml` snapshot 极端峰值约 **~5.7–6.0 GB**（远低于 10 GB 配额）。
- 原二期复查清单已合并升级至 §7.3，统一按三期完成后的最终清单复核。

## 7. 三期治理：移除 `release.yml` BuildKit GHA 缓存与 `mise-sdk-node-v2` 键升级（2026-11-21 / Issue #311）

### 7.1 实时水位与 LRU 淘汰实证（2026-11-21 20:05 UTC 后）

二期（#300）合入后，连续两次在 `main` 上手动执行 `.github/workflows/release.yml`（`workflow_dispatch` snapshot）导致仓库缓存再次突破 10 GB 配额并触发实际 LRU 淘汰：

- **总体水位**：`gh api repos/eimsound/lazy-observe/actions/cache/usage` 返回 `active_caches_size_in_bytes` = **`12,354,548,018` B（`12.35 GB` / `11.51 GiB`）**，超出 10 GB 配额 **+2.35 GB（+23.5%）**；`active_caches_count` = `125`（`actions/caches --paginate` 列出 **126** 条，100% 位于 `refs/heads/main`，1 条计数差为 GitHub API 最终一致性）。
- **BuildKit 缓存（`buildkit-blob-*` + `index-container-*`）单次双架构演练净增 `+1.77 GiB`（`+1.90 GB`）**：
  - 基线（§6.1，19:00 UTC，对应第 1 次 snapshot Run `44931530120` @ `5705f4e`）：**43 条**（41 个 blob + 2 个 index `#1`），**`1,960,926,532` B（`1,870.09 MiB` / `1.83 GiB`）**；
  - 第 2 次 snapshot Run `44944072291`（2026-11-21 20:01:51Z，on `main` `f58eb5a`）净增：**+24 条**（+22 个 blob + 2 个 index `#2`），**`+1,899,316,480` B（`+1,811.33 MiB` / `+1.77 GiB` / `+1.90 GB`）**；
  - 两次合计：**67 条**（63 `buildkit-blob-*` `3,860,195,881` B + 4 `index-container-*` `47,131` B）= **`3,860,243,012` B（`3,681.42 MiB` / `3.60 GiB` / `3.86 GB`）**，仅两次发版演练即占总用量的 **31.2%**、占整仓 10 GB 配额的 **38.6%**。
- **与 19:00 UTC 快照精确守恒校验（实际发生 LRU 淘汰）**：
  - 基线快照：`97` 条、`10,638,926,276` B；
  - 新增（`+31` 条、`+2,277,215,788` B）：BuildKit Run 2（`+24` / `+1,899,316,480` B）、`uv-sdk-python`（`+5` / `+70,059,892` B）、`storage-native` 新干净键（`+1` / `+188,910,534` B，180.16 MiB）、`playwright-sdk-journeys` 精确版本键 `1.65.1`（`+1` / `+118,928,882` B，113.42 MiB）；
  - 已被 LRU 淘汰（`-2` 条、`-561,594,046` B / `-535.58 MiB`）：`mise-base-Linux-ARM64` `4 → 3` 条（`-169,689,203` B / `-161.83 MiB`）、`gobuild-Linux-X64` `3 → 2` 条（`-391,904,843` B / `-373.75 MiB`）；
  - 校验：`97 + 31 - 2 = 126` 条，`10,638,926,276 + 2,277,215,788 - 561,594,046 = 12,354,548,018` B。
- **其余前缀现状补充确认**：
  - `pnpm-sdk-journeys-*`：仍为 5 条 / `430.25 MiB`（`451,149,964` B），#300 合入后已停止新增，等待自然过期/淘汰；
  - `storage-all-*`：仍为 1 条旧键（`523.77 MiB` / `549,214,645` B），原因是 Run `44944072291` 中 `performance-gates`（Job `136967388205`）的 `perf-ha` 门禁在弱扩展效率上失败（`2/4` 轮通过 `0.680`），导致末尾 `Save cross-platform storage archives` 步骤被跳过，非缓存机制故障（#309 修复后待下次成功运行刷新）；
  - `mise-sdk-node-*`（共 6 条 / `1,111.19 MiB` / `1.09 GiB`）：#299 合入修复了裸 `mise exec --` 触发无关工具安装的问题，但未升级 key，导致 3 条 `pnpm-12.21.0` 膨胀旧缓存（`556.08 MiB`：199.64 / 200.82 / 155.62 MiB）在 Run `44943060889` 及后续 PR 中持续精确命中并刷新 `last_accessed_at`，无法自动淘汰（虚占 ~406 MiB）；
  - `mise-bin-*`（#310 新增，Run `2150509` 合入后实测）：`mise-bin-Linux-X64`（`58,300,925` B / `55.60 MiB`）+ `mise-bin-Linux-ARM64`（`58,593,871` B / `55.88 MiB`），合计 `116,894,796` B（**~111.5 MiB**），由 `go-cross` on `main` 唯一写入。

### 7.2 根因与治理改动（Issue #311）

1. **三层叠加根因**：
   - `Dockerfile` 将易变 `ARG VERSION / COMMIT / DATE` 声明在 `builder` 阶段顶部（`release.yml` 每次传入秒级 `DATE` 与 `COMMIT`），每次运行 100% 击穿 `apt-get`、`curl mise`、`mise install` 等前置层；
   - BuildKit `type=gha,mode=max` 按内容寻址追加写入 `buildkit-blob-*` 从不覆盖旧条目，且不缓存 `RUN --mount=type=cache`（`/root/go/pkg/mod`、`/root/.cache/go-build`）；
   - 实测耗时倒挂：开启 `mode=max` 时 `#26 exporting to GitHub Actions Cache` 耗 **78.1s（amd64）/ 93.6s（arm64）**（构建步合计 209s / 189s），而关闭 GHA 缓存的 `ci.yml` `artifact-smoke` 原生冷构仅 **90s**（Job 总耗时 1m49s / 1m47s），且 `container-check` 与约 15 分钟的 `performance-gates` 并行，不在关键路径。
2. **落地改动**：
   - `.github/workflows/release.yml`：`container-check` 移除 `cache-from` 与 `cache-to`，与 `ci.yml` `artifact-smoke` 对齐（BuildKit GHA 缓存增量归 **0 B**）；
   - `Dockerfile`：将 `ARG VERSION=dev`、`ARG COMMIT=none`、`ARG DATE=unknown` 下移至 `COPY . .` 之后、`RUN ... mise run --skip-tools dist` 之前，保留本地重复构建的前置层缓存；
   - `.github/workflows/ci.yml`：`sdk-node` 与 `sdk-browser` 同步将缓存键升级为 `mise-sdk-node-v2-${{ matrix.node }}-pnpm-${{ steps.cache-keys.outputs.pnpm-toolchain }}-${{ runner.os }}-${{ runner.arch }}`，在 `main` 触发干净冷重建（~150 MiB）并停止刷新旧 `mise-sdk-node-*` 膨胀条目；
   - `scripts/check-github-workflows.sh`：新增 `buildkit_gha_cache_to` 静态校验与自测夹具，禁止 `.github/workflows/*.yml` 中出现 BuildKit `cache-to` 指向 `type=gha`（覆盖 `cache-to:` YAML 输入与 `--cache-to` CLI 参数）。

### 7.3 更新后的 7 天复查清单

- [ ] 合入 `main` 满 7 天（并确认旧 `buildkit-blob-*`/`index-container-*`、`mise-sdk-node-*`、`gobuild-*`、`pnpm-sdk-journeys-*` 过期）后执行：
  ```bash
  gh api repos/eimsound/lazy-observe/actions/cache/usage
  gh api repos/eimsound/lazy-observe/actions/caches --paginate --jq '.actions_caches[] | "\(.ref)\t\(.size_in_bytes)\t\(.key)"' | sort
  ```
- [ ] 核查要点：
  1. `buildkit-blob-*` 与 `index-container-*` 彻底归零；合入后在 `main` 执行 `release.yml` `workflow_dispatch` snapshot 演练时，`container-check` 不新增任何 BuildKit Actions 缓存条目，单格构建耗时维持在 ~2 分钟内。
  2. `mise-sdk-node-v2-*-pnpm-*-Linux-X64` 共 3 条（Node 22/24/26），单条回落至 **~45–60 MiB**（合计 ~150 MiB），旧膨胀 `mise-sdk-node-*` 过期归零。
  3. `mise-bin-Linux-{X64,ARM64}-*` 共 2 条（合计 **~111.5 MiB**）。
  4. `gobuild-Linux-X64-*` 单条稳定在 **~290–310 MiB**（ARM64 **~176–190 MiB**），`go.mod` 升级后新条不再在旧条体积上单调叠加。
  5. `pnpm-sdk-journeys-*` 彻底归零；`playwright-sdk-journeys-Linux-X64-<ver>` 仅按 `playwright-core` 版本存在单条（~113.4 MiB）。
  6. `storage-native-Linux-X64-*` 单条回落至 **~180 MiB**；`storage-all-*` 在 `performance-gates`（`bench.yml` / `release.yml`）下次于 `main` 成功运行后刷新，同步清理历史归档。
  7. 仓库 `active_caches_size_in_bytes` 稳定低于 **10 GB**（预期日常峰值 ~5.15 GB，含发版演练 ~5.7–6.0 GB），且无意外 LRU 淘汰 `embedded-pg-*`、`storage-native-*`、`mise-base-*`、`gobuild-*`。
