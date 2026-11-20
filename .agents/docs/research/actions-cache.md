# GitHub Actions Cache 超限治理（10 GB 配额）

> 状态：调研与实施方案记录（切片 CQ），2026-11-20。
> 数据来源：只读查询 `gh api repos/eimsound/lazy-observe/actions/cache/usage` 与 `actions/caches --paginate`（2026-11-20 12:18 UTC，对应 `main` `714a21f`）。

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
   - `release.yml`：`container-check` 的 BuildKit `cache-to` 仅在 `refs/heads/main`（手动 snapshot 选 `main`）时写入；只在 tag push 运行的 `docker` job 移除 `cache-to`，只读 `container-amd64` / `container-arm64`。
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

## 4. 预期总容量估算

### 单代稳态（`main` 日常 CI）

| 类别 | key 模式 | 条数 | 估算单条 | 估算小计 |
| --- | --- | ---: | ---: | ---: |
| Go 模块（X64/ARM64 共用） | `gomod-<go>` | 1 | ~200 MB | ~200 MB |
| Go 编译缓存（X64） | `gobuild-Linux-X64-<go-ver>-<go>` | 1 | ~250 MB | ~250 MB |
| Go 编译缓存（ARM64） | `gobuild-Linux-ARM64-<go-ver>-<go>` | 1 | ~180 MB | ~180 MB |
| mise（go-ci，已 prune） | `mise-go-Linux-X64-<tools>` | 1 | ~240 MB | ~240 MB |
| mise（base X64 + ARM64，已 prune） | `mise-base-Linux-{X64,ARM64}-<tools>` | 2 | ~165 / 155 MB | ~320 MB |
| mise（sdk-node 22/24/26，`--skip-tools`） | `mise-sdk-node-<ver>-pnpm-<ver>-Linux-X64` | 3 | ~50 MB | ~150 MB |
| storage 原生归档 | `storage-native-Linux-X64-<storage>` | 1 | 333 MB | 333 MB |
| pnpm（web×2、node、browser、journeys） | `pnpm-*-Linux-*` | 5 | 33–85 MB | ~281 MB |
| Playwright 浏览器 | `playwright-sdk-journeys-Linux-X64-<lock>` | 1 | 114 MB | 114 MB |
| uv（5 个 Python 版本） | `uv-sdk-python-Linux-X64-<py>-<toml>` | 5 | ~10.5 MB | ~52 MB |
| embedded-pg | `embedded-pg-Linux-X64-18.6.1-<mod>` | 1 | 37 MB | 37 MB |
| **日常 CI 单代合计** | | **22** | | **~2.11 GiB（~2.26 GB）** |

### 7 天窗口峰值估算（含 Renovate 密集升级 + 手动发版演练）

- **高频升级周**（7 天内合并 3 次 `go.mod` 升级 + 2 次 `mise.toml [tools]` 升级 + 2 次 `pnpm-lock.yaml` 升级，旧代尚未过 7 天 TTL）：
  - Go 缓存（3 代 × ~630 MB）：~1.89 GB
  - `mise-go` + `mise-base`（2 代 × ~560 MB）+ `mise-sdk-node`（1 代 ~150 MB）：~1.27 GB
  - `pnpm-*`（2 代 × ~281 MB）：~0.56 GB
  - 长期稳定资产（`storage-native` + `playwright` + `uv` + `embedded-pg`）：~0.54 GB
  - **CI 峰值合计**：**~4.26 GB**（远低于 10 GB 配额）。
- **叠加手动在 `main` 上跑 `bench.yml` / `release.yml` snapshot**：
  - 另加 `storage-all`（~660 MB）+ `mise-release`（~220 MB）+ BuildKit `container-amd64`/`container-arm64` `mode=max`（~1.5–2.5 GB）
  - **极端峰值**：**~6.6–7.6 GB**，仍留有 >2.4 GB 余量，关键缓存（`embedded-pg`、`storage-native`、`mise-*`、`gomod`）不再被 LRU 挤出。

> 过渡期说明：本改动合入 `main` 后，旧 key（约 11.7 GB）将在 7 天内随无人访问自然过期，或在短时间内被新 key（~2.2 GB）挤出旧代，一周后回落至稳态区间。

## 5. 待办：合入一周后复查

- [ ] 合入 `main` 满 7 天后（约 2026-11-27 之后），执行只读核查命令确认真实稳态：
  ```bash
  gh api repos/eimsound/lazy-observe/actions/cache/usage
  gh cache list --limit 200 --json key,ref,sizeInBytes,createdAt,lastAccessedAt
  ```
- [ ] 核查要点：
  1. `active_caches_size_in_bytes` 稳定在 **5 GB 以下**（且无 `refs/pull/*` 或 `refs/tags/*` 条目）。
  2. `mise-sdk-node-*` 单条实际压缩体积符合预期（约 45–60 MB，无 Go/dev 工具混入）。
  3. `gomod-*`、`gobuild-*`、`mise-base-*`、`mise-go-*` 体积符合估算，`embedded-pg-*` 与 `storage-native-*` 未被 LRU 淘汰。
  4. 确认旧 `mise-go-cross-*` 已全部过期后，可顺手清理 `ci.yml` 中 `mise-base` 的第二行过渡 `restore-keys`。
