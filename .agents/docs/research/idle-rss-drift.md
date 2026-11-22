# 空闲 RSS 漂移排查与缓解调研（`197e273` → `cbba479`）

> 调查与实测日期：2026-11-22  
> 测试宿主机：`Linux 6.12.115+deb13.1-amd64`（8 vCPU、15 GiB RAM + 2.0 GiB Swap），`CGO_ENABLED=0`，Go `1.27.4`  
> 对比基线：`main` @ `cbba47981b7913463f8da3d14930afe6238dbc11`（改前） vs `slice/idle-rss-drift-1`（改后）

## 1. 问题与背景

`scripts/check-release-smoke.sh`（以及 `mise run gate-release-artifacts` / `bench/performance-gates.json`）在零配置启动、写入并回读 OTLP metrics/logs/traces 后，统计 `gateway` 与四个托管子进程（`victoria-metrics-prod`、`victoria-logs-prod`、`victoria-traces-prod`、`vmagent-prod`）的 `ps -o rss=` 之和（`aggregate_idle_rss_bytes`），要求严格小于 `524,288,000 B`（`500 MiB`）。

历史初始基线 `197e273` 约 `366–379 MiB`，近期在 `main` 节点实测已漂移至 **`435.4–447.5 MiB`**（`456.5–469.2 MB`），距 500 MiB 门限仅余 52–65 MiB，若不查清根因并治理，后续版本升级或轻微波动极易击穿发布门禁。

## 2. 改前实测取证（`cbba479`）

### 2.1 连续 3 次 `check-release-smoke.sh` 实测

构建产物：`bin/lazy-observe`（`119,275,680 B` / `113.75 MiB`）；四份内嵌 `.gz`（`internal/embed/bins/assets/linux-amd64/`）合计 `53,494,938 B`（`51.02 MiB`）；`internal/webui/dist` 合计 `2,261,052 B`（`2.16 MiB`）。

| 轮次 | `gateway` | `metrics` | `logs` | `traces` | `vmagent` | 总 RSS（Bytes / MiB） |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Run 1 | 311,635,968 (297.2 MiB) | 71,794,688 (68.5 MiB) | 27,127,808 (25.9 MiB) | 31,068,160 (29.6 MiB) | 24,199,168 (23.1 MiB) | **465,825,792 (444.2 MiB)** |
| Run 2 | 313,663,488 (299.1 MiB) | 77,365,248 (73.8 MiB) | 24,977,408 (23.8 MiB) | 31,649,792 (30.2 MiB) | 21,553,152 (20.6 MiB) | **469,209,088 (447.5 MiB)** |
| Run 3 | 309,760,000 (295.4 MiB) | 68,247,552 (65.1 MiB) | 26,648,576 (25.4 MiB) | 29,696,000 (28.3 MiB) | 22,151,168 (21.1 MiB) | **456,503,296 (435.4 MiB)** |

结论：**`gateway` 单进程独占 ~295–299 MiB（占总 RSS 的 67–68%）**，四个 Victoria 子进程合计 `131–148 MiB`。

### 2.2 ELF 节、程序头与内嵌资产体积明细

`readelf -SW` / `-lW` 与 `.cache/storage-bin/linux-amd64/` 统计：

- **可加载段**：
  - `LOAD #0` (`r-xp`, `.text` 等)：`FileSiz = MemSiz = 0x1e467d1`（`30.28 MiB`）
  - `LOAD #1` (`r--p`)：`FileSiz = MemSiz = 0x5238288`（`82.22 MiB`），其中：
    - `.rodata`：`57,305,625 B`（`54.65 MiB`），四份内嵌 `.gz` 占 **`53,494,938 B`（`51.02 MiB`，占 `.rodata` 的 93.35%）**：
      - `victoria-metrics-prod.gz`（v1.156.0）：`14,862,635 B`（原 binary `28,624,472 B`）
      - `victoria-traces-prod.gz`（v0.13.0）：`13,210,033 B`（原 binary `25,243,136 B`）
      - `victoria-logs-prod.gz`（v1.54.0）：`13,067,683 B`（原 binary `25,124,080 B`）
      - `vmagent-prod.gz`（v1.156.0）：`12,354,587 B`（原 binary `23,209,584 B`）
    - `.gopclntab`：`22,880,968 B`（`21.82 MiB`）；`.go.type`：`5,996,664 B`（`5.72 MiB`）；`.go.func`：`29,984 B`
  - `LOAD #2` (`rw-p`)：`FileSiz = 0x13fd20`（`1.25 MiB`）、`MemSiz = 0x21c40b0`（`33.77 MiB`）；其中 `.noptrbss` 为 `32.31 MiB`（主要为 `crypto/internal/fips140/drbg.memory` `33,554,432 B` 零初始化保留空间，因未按页触碰实际仅 ~`0.3 MiB` 驻留）。

### 2.3 smoke 测量点 `/proc/<pid>/smaps_rollup` 与 `gateway` Top VMAs（改前）

以下数据在 `check-release-smoke.sh` 同一测量时刻采集（该轮 `ps -o rss=` 总计 `455,827,456 B / 434.71 MiB`）：

| 进程 | `ps_rss` (MiB) | `Rss` (kB) | `Pss` (kB) | `Anonymous` (kB) | `Private_Clean` (kB) | `Private_Dirty` (kB) | `RssFile` (kB) | `Shared_Clean` (kB) | Threads |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| `gateway` | 298.30 | 305,464 | 305,456 | 212,220 | 93,224 | 212,232 | 93,244 | 8 | 18 |
| `metrics` | 58.46 | 59,868 | 59,860 | 43,332 | 3,820 | 56,040 | 16,536 | 8 | 13 |
| `logs` | 25.79 | 26,408 | 26,400 | 12,764 | 3,620 | 22,780 | 13,644 | 8 | 25 |
| `traces` | 31.08 | 31,828 | 31,820 | 17,268 | 3,644 | 28,176 | 14,560 | 8 | 23 |
| `vmagent` | 21.07 | 21,576 | 21,568 | 8,168 | 3,712 | 17,856 | 13,408 | 8 | 11 |

改前 `gateway` Top VMAs（`/proc/<pid>/smaps`）：

| 虚拟映射 | 权限 | Size (kB) | Rss (kB) | Anonymous (kB) | Private_Clean (kB) | Private_Dirty (kB) | 说明 |
| --- | --- | ---: | ---: | ---: | ---: | ---: | --- |
| `[anon]`（Go Heap） | `rw-p` | 221,184 | **205,064** | 205,064 | 0 | 205,064 | Argon2id 分配的 ~192 MiB 临时块未及时归还 OS |
| `bin/lazy-observe`（`LOAD #1`） | `r--p` | 84,196 | **72,420** | 0 | 72,420 | 0 | `.rodata`（含 51.02 MiB 内嵌 `.gz`）+ `.gopclntab` |
| `bin/lazy-observe`（`LOAD #0`） | `r-xp` | 31,004 | 19,996 | 0 | 19,996 | 0 | Go 代码段 `.text` |
| `[anon]` | `rw-p` | 4,296 | 2,616 | 2,616 | 0 | 2,616 | Go 运行时/栈 |
| `[anon]`（`.bss`） | `rw-p` | 33,300 | 2,248 | 2,248 | 0 | 2,248 | `.bss`/`.noptrbss` 实际触碰页 |
| `bin/lazy-observe`（`LOAD #2`） | `rw-p` | 1,280 | 1,088 | 300 | 788 | 300 | `.data`/`.noptrdata` |

### 2.4 分阶段动态追踪与热重启剥离

在同一工作目录对 `cbba479` 进行分阶段采样（冷启 → Web 资源 → 登录 → 强制改密 → 摄入 → 静置 +10s → 同 `data_dir` 热重启）：

| 阶段 | 操作与触发条件 | `gateway` RSS (MiB) | `gateway` `Anon` (MiB) | `gateway` `r--p` VMA (MiB) | `gateway` `[anon]` Heap (MiB) | 5 进程总 RSS (MiB) |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| **A** | 冷启刚过 `/readyz`（解压 4×`.gz` + `Bootstrap` Argon2id ×1） | 178.72 | 90.34 | 68.85 | 84.00 | 262.96 |
| **B1** | 请求 `/` 与内嵌 Web 静态资源 | 181.64 | 92.39 | 69.72 | 86.00 | 265.89 |
| **B2** | `POST /api/auth/login`（Argon2id ×1） | 248.87 | 158.68 | 70.10 | 152.00 | 333.11 |
| **B3** | `POST /api/auth/password`（Argon2id ×2：校验旧密 + 哈希新密） | 307.30 | 217.12 | 70.10 | 210.28 | 391.55 |
| **C** | OTLP 摄入后（对应 smoke 测量点） | 296.03 | 205.84 | 70.10 | 199.00 | 384.25 |
| **D** | 静置 +10s（未发生请求） | 297.15 | 206.40 | 70.41 | 199.51 | 401.85 |
| **E** | **同 `data_dir` 热重启**（复用磁盘二进制、无 `.gz` 解压、无 Argon2id） | **50.32** | **13.62** | **17.35** | **8.44** | **144.20** |

独立验证（`go run` 最小复现脚本）：连续 4 次 `argon2.IDKey(..., argonMemory=64*1024)` 后 `VmRSS=136,988 kB`、`HeapIdleUnrel=64.3 MiB`，静置 2s 几乎不归还（`137,020 kB`），主动 `runtime.GC()` 后 `HeapIdleUnrel=128.3 MiB`，再调用 `debug.FreeOSMemory()` 后 `VmRSS` 立即回落至 **`5,640 kB`**（`HeapIdleUnrel=0.0 MiB`）。

## 3. 历史演进对照（`197e273` → `cbba479`）

在隔离临时目录用各节点对应的 Go 工具链与上游 Victoria 版本构建并运行对应 `check-release-smoke.sh`（复用当前 `internal/webui/dist` `2.16 MiB`）：

| Commit | 关键变更 | 二进制 (MiB) | 内嵌 `.gz` (MiB) | `gateway` (MiB) | 四 Victoria 合计 (MiB) | 总 RSS (MiB) | 相对前节点变化 |
| --- | --- | ---: | ---: | ---: | ---: | ---: | --- |
| `197e273` | `init`（Go 1.26.5, Fiber v2.52.14, VM 1.147 / Logs 1.52 / Traces 0.9.4） | 99.04 | 45.29 | 232.27 | 134.21 | **366.48** | 基线 |
| `61b35a3` | Go 1.26.11，**bootstrap admin 强制首次改密**（smoke 新增 `/api/auth/password`） | 99.27 | 45.29 | **282.46** | 131.05 | **413.51** | **`gateway +50.19 MiB`** |
| `fa0bcd4` | Victoria 升级（VM 1.156 / Logs 1.53 / Traces 0.13.0） | 105.61 | **50.96** | **289.83** | 132.60 | **422.43** | `.gz +5.68 MiB` → `gateway +7.37 MiB` |
| `a45baa8` | Fiber 升级至 v3.7.0 | 108.90 | 50.96 | 288.89 | 140.37 | **429.27** | 二进制 `+3.29 MiB`，子进程正常波动 |
| `714a21f` | Go 1.27.4 | 111.67 | 50.96 | **295.23** | 138.37 | **433.60** | 二进制 `+2.78 MiB`（`.gopclntab`/`.text` 等），`gateway +6.34 MiB` |
| `392453f` | VictoriaLogs 升级至 v1.54.0 | 111.72 | **51.02** | 294.75 | 131.00 | **425.75** | `.gz +0.05 MiB` |
| `cbba479` | 当前 `main`（3 次均/中位基准） | 113.75 | 51.02 | 295.4–299.1 | 139.9–148.3 | **435.4–447.5** | 相比 `197e273` 净漂移 `+69~81 MiB` |

## 4. 根因归因结论

1. **主要根因（~195 MiB 匿名堆冗余；占历史漂移 +50.2 MiB，约 65%）：Argon2id 64 MiB 临时堆滞留**
   - `internal/meta/password.go` 每次调用 `argon2.IDKey`（`argonMemory = 64*1024 KiB = 64 MiB`）都在 Go 堆上分配 64 MiB 切片。
   - `61b35a3` 之前，smoke 仅经历 `Bootstrap`（×1）与 `login`（×1）两次 Argon2id；`61b35a3` 强制首次改密后，smoke 在数秒内连续触发 **4 次** Argon2id（`Bootstrap` + `login` + `ChangePassword` 内校验旧密码与哈希新密码各 1 次），将 Go 堆推高至 205–217 MiB。
   - Go 后台 scavenger 以固定微弱速率归还空闲页，因此直到 15–30 秒后测量 RSS 时仍有 ~190+ MiB 已释放对象对应的匿名物理页滞留在 `gateway` RSS 中（而非真实活跃对象泄漏）。
2. **第二根因（~51–53 MiB 只读文件页常驻；占历史漂移 +5.73 MiB）：`embed.FS` 解压后 `.rodata` 页缓存常驻 RSS**
   - `internal/embed/bins` 在首次启动时通过 `embed.FS` 解压四份 gzip 资产（`51.02 MiB`）。Go 编译器将 `embed.FS` 数据段直接编译进 ELF `.rodata` 节（`54.65 MiB`）。
   - 顺序解压把整个 `.rodata` 缺页映射进 `gateway` 的 `r--p` 映射表（`Private_Clean` 从热重启的 `17.35 MiB` 升至 `68.85–72.42 MiB`）。解压完成后这些只读数据整个生命周期不再读取，但除非内核承受内存压力回收页帧，否则会一直计入进程 RSS；Victoria 升级导致的 `.gz` 体积增长（`+5.73 MiB`）会 1:1 直接推高空闲 RSS。
3. **其余不可缩减基线（占历史漂移约 +6–8 MiB 及子进程正常区间）**：
   - Go 1.27.4 + Fiber v3 + OTLP/gRPC 等引入的非 `.gz` 二进制增长约 `9.0 MiB`（`103.85 → 119.28 MiB` 减去 `.gz` `+5.73 MiB` 与 web `+0.5 MiB`），对应常驻代码/元数据增长约 `+6–8 MiB`；
   - 四个 Victoria 子进程合计 `131–148 MiB`（`metrics` `55–77 MiB`、`logs` `24–27 MiB`、`traces` `29–33 MiB`、`vmagent` `21–24 MiB`），与 `197e273`（`134.2 MiB`）处于同一基线水平，波动主要来自 OTLP 写入/查询刚完成后的缓存与合并线程，**无需收紧 Victoria 参数**（避免损害 `perf-ha` / `e2e` 吞吐与尾延迟）。

## 5. 缓解方案与取舍

1. **安全回收已解压的内嵌只读文件页（`internal/embed/bins`）**
   - 在 `ExtractOnly` 实际执行了磁盘解压（`extracted > 0`）后，在 Linux（`amd64`/`arm64`）上通过纯标准库（`debug/elf` 解析 `.rodata` 偏移与长度 + 解析 `/proc/self/maps` 严格筛选 `r--p && inode != 0` 且匹配 `/proc/self/exe` `(dev, inode)` 或 overlayfs 路径回退的 VMA + 页向内对齐）调用 `syscall.SYS_MADVISE` `MADV_DONTNEED`，通知内核释放纯只读 file-backed 页。
   - **安全保证**：不引入 `unsafe`、不违背 `CGO_ENABLED=0`；向内对齐页面边界确保绝不触及相邻 `.gopclntab` 或可写段；`MADV_DONTNEED` 对 `r--p` 文件映射仅丢弃干净页表项，若后续仍需读取（例如极罕见的运行时重提取），Linux 自动从磁盘可执行文件透明重新缺页读入，数据零损坏；仅在发生实际解压后触发一次，best-effort 失败不阻断启动。
2. **大内存 Argon2id 完成后协调回收瞬态堆（`internal/meta/password.go`）**
   - 保持 `argonMemory = 64*1024`、`argonTime = 1`、`argonThreads = 4`、`argonKeyLen = 32` 密码学强度不变；不引入固定低 `GOMEMLIMIT`（避免压测与查询高频分配时触发 GC 抖动）。
   - 为 `memory >= 8 MiB` 的 `argon2.IDKey` 调用增加 `argonMemoryReclaimer`：仅当全部在途大内存 Argon2id 均已退出并发槽位、且满足 `250ms` 冷却节流窗口时（冷却期内多次完成自动合并为单次后缘定时回调，并在 `release()` 归还并发槽位之后）调用 `debug.FreeOSMemory()`，将瞬态 64 MiB 空闲堆立即归还内核，同时防止并发登录洪峰期间连续同步 GC。
3. **增强 `scripts/check-release-smoke.sh` 可观测性**
   - 在 PASS/FAIL 摘要行直接打印 5 个进程各自 MiB 与 Bytes 明细，保持原有 JSON 输出 schema 完全兼容。

## 6. 改后实测效果（`slice/idle-rss-drift-1`）

### 6.1 连续 3 次 `check-release-smoke.sh` + `gate-release-artifacts`

| 轮次 / 产物 | `gateway` | `metrics` | `logs` | `traces` | `vmagent` | 总 RSS（Bytes / MiB） | 相比改前均值 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 改后 Run 1 (`bin/lazy-observe`) | 61,460,480 (58.6 MiB) | 61,292,544 (58.5 MiB) | 24,649,728 (23.5 MiB) | 31,395,840 (29.9 MiB) | 21,618,688 (20.6 MiB) | **200,417,280 (191.1 MiB)** | -251.8 MiB (-55.7%) |
| 改后 Run 2 (`bin/lazy-observe`) | 60,358,656 (57.6 MiB) | 73,289,728 (69.9 MiB) | 24,297,472 (23.2 MiB) | 33,402,880 (31.9 MiB) | 22,134,784 (21.1 MiB) | **213,483,520 (203.6 MiB)** | -239.3 MiB (-52.9%) |
| 改后 Run 3 (`bin/lazy-observe`) | 60,981,248 (58.2 MiB) | 62,779,392 (59.9 MiB) | 27,029,504 (25.8 MiB) | 27,312,128 (26.0 MiB) | 22,179,840 (21.2 MiB) | **200,282,112 (191.0 MiB)** | -251.9 MiB (-55.7%) |
| `gate-release-artifacts` (`bin/lazy-observe-linux-amd64`) | 61,284,352 (58.4 MiB) | 76,713,984 (73.2 MiB) | 26,423,296 (25.2 MiB) | 33,271,808 (31.7 MiB) | 24,272,896 (23.1 MiB) | **221,966,336 (211.7 MiB)** | -231.2 MiB (-51.1%) |

- 双架构产物体积（`mise run dist-cross`）：`bin/lazy-observe-linux-amd64` 为 `119,447,712 B`，`bin/lazy-observe-linux-arm64` 为 `108,855,456 B`（均 `< 150,000,000 B` 门限）。

### 6.2 改后 `gateway` `smaps_rollup` 与 Top VMAs 对比

在 smoke 测量点采样（该轮总 `ps_rss = 201,502,720 B / 192.2 MiB`）：

| 指标 (`gateway`) | 改前 (`cbba479`) | 改后 (`slice/idle-rss-drift-1`) | 变化 |
| --- | ---: | ---: | ---: |
| `ps_rss` | 312,795,136 B (298.30 MiB) | **60,346,368 B (57.55 MiB)** | **-240.75 MiB (-80.7%)** |
| `RssAnon` / `Anonymous` | 212,220 kB (207.25 MiB) | **17,880 kB (17.46 MiB)** | **-189.79 MiB (-91.6%)** |
| Go Heap VMA (`rw-p [anon]`) | 205,064 kB (200.26 MiB) | **10,980 kB (10.72 MiB)** | **-189.54 MiB (-94.6%)** |
| `RssFile` | 93,244 kB (91.06 MiB) | **41,052 kB (40.09 MiB)** | **-50.97 MiB (-56.0%)** |
| `LOAD #1` VMA (`r--p`) | 72,420 kB (70.72 MiB) | **20,160 kB (19.69 MiB)** | **-51.04 MiB (-72.2%)** |
| `Private_Clean` | 93,224 kB | **41,032 kB** | -52,192 kB |
| `Private_Dirty` | 212,232 kB | **17,892 kB** | -194,340 kB |

四子进程改后 `smaps_rollup`：`metrics` `57,876 kB`、`logs` `26,068 kB`、`traces` `32,368 kB`、`vmagent` `21,536 kB`（共 `134.6 MiB`，与改前持平）；5 进程总空闲 RSS 稳定收敛至 **`191.0–211.7 MiB`**，距离 `500 MiB`（`524,288,000 B`）门限保留了 **~288–309 MiB** 的充足安全余量。
