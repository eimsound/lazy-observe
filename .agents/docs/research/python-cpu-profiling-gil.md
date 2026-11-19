# Python CPU 剖析：GIL、忙闲切换、外部 attach 与 greenlet

> 状态：调研归档（2026-11）。结论已写进 [ADR 0014](../adr/0014-python-profiling-on-cpython-315-sampling.md)
> 和 [`plans/python-profiling.md`](../plans/python-profiling.md)。本文接着
> [`python-315-sampling.md`](python-315-sampling.md)（spike）往下写。

## 问题

S2 审查发现，GIL 版 CPython 3.15.2 上进程内调用
`profiling.sampling.sample.sample(os.getpid(), …, mode=cpu)` 时，纯 Python 忙线程拿到 0 个样本。
负责人随后提了三个问题：CPU 剖析是不是必须用外部 attach？换成别的 mode 或加权方式能不能救回来？
协程、gevent、频繁忙闲切换的负载能不能正常工作？

## 结论

1. **GIL 构建上，所有进程内方案都会系统性漏掉短 CPU 段。** 采样线程必须先拿到 GIL 才能读栈，
   而它只能在业务线程释放 GIL 时拿到：要么业务线程主动释放（sleep、IO、`epoll`），要么被强制切换
   （默认 `sys.getswitchinterval()` = 5 ms）。所以短于切换间隔的 CPU 段永远被看成「停在等待处」。
   `cpu`、`wall`、`all` 三种 mode，以及在其上做的各种加权和过滤，结果都一样。
2. **free-threaded 3.15t 上，进程内 `mode=cpu` 基本准确**，忙闲切换下误差也在 10pp 以内。但 3.15t 一旦重新启用
   GIL（`-X gil=1`），就退化成和 GIL 构建一样。所以启用条件要按运行时 `sys._is_gil_enabled()`
   判断，不能只看构建类型。
3. **「wall/all + 按线程 CPU 时钟加权」（审查报告里的方案 (e)）在忙闲切换下不成立。** 它把两次
   tick 之间的 CPU 增量记到「此刻的栈」上，而突发结束后线程已经停在等待处。这样得到的其实是墙钟分布。
   早先「(e) 与 attach 同样准」的结论，只在持续忙的负载下成立。
4. **外部 attach 在两种构建上的 cpu 和 wall 都准**，但权限和部署门槛让它不能默认开启（见第 4 节）。
5. **greenlet（gevent、eventlet 等）完全不支持。** Tachyon 在线程跑在非主 greenlet 里时抓栈失败。
   进程内和外部 attach 都失败，各种 mode 都失败，3.15 和 3.15t 都失败。

## 环境

| 项 | 值 |
| --- | --- |
| 内核 | Linux 6.12.115+deb13.1-amd64（Debian 13），8 核，Yama `ptrace_scope=0` |
| 解释器 | uv 管理的 `cpython-3.15.2-linux-x86_64-gnu` 与 `cpython-3.15.2+freethreaded-linux-x86_64-gnu`，所有脚本都用 `-I` 运行 |
| 第三方包 | gevent 26.9.0、greenlet 3.5.6（GIL 版，`uv pip install --only-binary :all:`）；3.15t 只有 greenlet 3.5.6 的 wheel，gevent 没有 3.15t wheel |
| PoC | 本地 `poc/python-315-sampling-s2-review/`，被 git 忽略，不提交。子目录：`attach/`、`wallvsall/`、`burst/`、`gevent/`、`sqla/` |

## 1. 忙闲切换下的准确性（`burst/`）

负载：一个线程或一个 asyncio 协程反复执行「在 `burn()` 里烧 N ms CPU，再等 2N ms」。真值是
`burn()` 内的线程 CPU 占比，为 94–100%。表中是各方案记到 `burn` 上的百分比，每格是两轮结果。

```
for py in 3.15 3.15t; for wl in thread asyncio; for b in 1/2 3/6 8/16 20/40:
  python -I burst.py $wl $b 3 ; python -I burst.py $wl $b 3 ext     # → burst.jsonl
python -I summarize_burst.py burst.jsonl
```

| 构建 | 负载 | 忙/闲 ms | (e) 时钟加权 | all + 状态过滤 | 外部 attach `mode=cpu` |
| --- | --- | --- | --- | --- | --- |
| GIL | 线程 | 1/2 | 0/0 | 0/0 | 99/93 |
| GIL | 线程 | 3/6 | 1/1 | 33/15 | 97/99 |
| GIL | 线程 | 8/16 | 33/30 | 87/98 | 100/100 |
| GIL | 线程 | 20/40 | 72/72 | 100/96 | 100/100 |
| GIL | asyncio | 1/2 | 0/0 | 0/0 | 96/95 |
| GIL | asyncio | 3/6 | 0/0（全记到 `EpollSelector.select`） | 0/5 | 99/100 |
| GIL | asyncio | 8/16 | 30/30 | 96/94 | 97/98 |
| 3.15t | 线程 | 1/2 | 39/36 | 91/97 | 100/99 |
| 3.15t | 线程 | 3/6 | 38/37 | 99/98 | 100/100 |
| 3.15t | asyncio | 3/6 | 37/37 | 98/99 | 100/100 |
| 3.15t | asyncio | 20/40 | 78/78 | 100/100 | 100/100 |

「all + 状态过滤」的规则是：状态带 `HAS_GIL|ON_CPU|GIL_REQUESTED` 时计 1 个样本。

3.15t 上直接用进程内 `mode=cpu`（`burst_cpu.py`，每格 1 次）：线程负载忙 1/3/8 ms 时为
88/92/94%（真值 98–99%）；asyncio 负载忙 1/3/8 ms 时为 84/100/100%（真值 94–98%）。

3.15t 重新启用 GIL 后（`python -I -X gil=1 burst_cpu.py thread …`）：忙 3 ms 时 100% 记到 `idle`，
忙 20 ms 时 0 样本。同一负载在 `-X gil=0` 下为 99%。注意：`-I` 隐含 `-E`，所以环境变量
`PYTHON_GIL=1` 在 `-I` 下不生效，`sys._is_gil_enabled()` 仍是 False。

## 2. 墙钟模式在忙闲切换下的表现（`burst/wall_burst.py`）

负载同上。墙钟真值是 `burn` 约占 33%。每格 1 次。

| 构建 | 负载 | 忙 ms：1 / 3 / 8 / 20 | 进程内 wall | 外部 attach wall |
| --- | --- | --- | --- | --- |
| GIL | 线程 | 真值 33% | 0 / 0 / 16 / 24 | 35 / 33 / 34 / 33 |
| GIL | asyncio | 真值 32–33% | 0 / 0 / 16 / 24 | 33 / 32 / 33 / 34 |
| 3.15t | 线程 | 真值 33% | 31 / 34 / 33 / 33 | 31 / 32 / 33 / 34 |
| 3.15t | asyncio | 真值 32–33% | 32 / 33 / 34 / 33 | 33 / 33 / 33 / 33 |

GIL 构建上，进程内 wall 会把 CPU 段算到等待栈上，等待因此被夸大。所以 wall 救不了 GIL 构建；
在 3.15t 上，墙钟剖析可以以后作为独立类型立项。

## 3. 开销（`wallvsall/`）

```
for r in 1..3; for sc in "1 0" "1 15" "1 63" "4 0"; for v in none cpu wall all:
  /tmp/s2-py315t/bin/python -I bench.py $v $sc 30 4 >> bench_cpu.jsonl
```

参数：3.15t，每次 4 s，栈深 30，100 Hz，每组 3 轮，load 1.3–4.5。`sampler %core` =
采样线程 CPU 时间 ÷ 4 s。

| 忙+空闲线程 | `mode=cpu` | `wall` | `all` | RSS 增量 |
| --- | --- | --- | --- | --- |
| 1+0 | 1.75% | 1.45% | 2.15% | 19.3 MiB |
| 1+15 | 3.60% | 2.27% | 4.38% | 17.2 MiB |
| 1+63 | 9.03% | 8.20% | 15.28% | 15.2 MiB |
| 4+0 | 2.77% | 2.02% | 2.77% | 17.2 MiB |

- 这里的 `wall`/`all` 用的是 Python 层逐线程加权的 collector。`mode=cpu` 在 C 层就把空闲线程过滤掉，
  但仍要逐线程读一次 `/proc/<pid>/task/<tid>/stat`，所以开销随线程数增长。
- 忙线程吞吐与 `none` 基线相比都在 ±6% 以内，与噪声无法区分，不下百分比结论。
- 另一轮（`bench.jsonl`，GIL 与 3.15t，每组 5 轮，load 1.2–6）在 GIL 构建上看到 `wall` 吞吐下降
  2–9%，`all` 下降 3–11%；单个忙线程每 4 s 的自愿上下文切换从 4–5 次增加到 566–664 次（让出 GIL 给采样线程）。
  这一节只作参考，因为 GIL 构建不在启用范围内。

## 4. 外部 attach（sidecar）的权限与部署（`attach/`）

外部采样器不持有目标进程的 GIL。此刻持有 GIL 的业务线程状态是 `R`，所以 `mode=cpu` 在 GIL
构建上也准确。门槛在权限与部署：

- **内核检查**：`process_vm_readv` 走 `PTRACE_MODE_ATTACH_REALCREDS`，要求同 uid、目标可 dump，
  并且受 Yama 约束。Tachyon 只在 `ENOSYS` 时才回退到 `/proc/pid/mem`。
- **Yama**：
  - `ptrace_scope=1`（上游默认、Ubuntu 默认，gVisor 语义相同）下，子进程读父进程失败。父进程要先调
    `prctl(PR_SET_PTRACER, sidecar_pid)`，但每个进程只能有一个放行关系，会和崩溃上报库互相覆盖。
  - scope=2 要求 CAP_SYS_PTRACE，PSS baseline 禁止添加；scope=3 完全不行。
  - 容器继承节点的 scope（据源码推断）。
- **dumpable**：父进程执行 `PR_SET_DUMPABLE=0`，或 root 启动后降权（模拟 gunicorn `--user`），
  sidecar 都得到 `PermissionError(13)`。加 `--cap-add SYS_PTRACE` 可以绕过。
- **seccomp**：屏蔽 `process_vm_readv` 时得到 `PermissionError(1)`，不回退。Docker、containerd 的默认
  profile 在内核 ≥ 4.8 时放行它。
- **跨构建**：GIL 与 free-threaded 之间双向拒绝（`RuntimeError`）。用 `sys.executable` 拉起 sidecar
  就天然是同一构建。
- **开销**：每个被采样进程多一个解释器，GIL 版 21 MiB，3.15t 39–41 MiB；sidecar 启动 62–66 ms。
- **生命周期**：
  - `PR_SET_PDEATHSIG` 跟随线程：拉起 sidecar 的线程退出，sidecar 就被误杀；
  - 管道被 fork 继承后，父进程死了也收不到 EOF；
  - 应用把 `SIGCHLD` 设为 `SIG_IGN` 时，`wait()` 返回 0，拿不到真实退出码；
  - 可行的父进程死亡检测是 `pidfd_open(getppid())`。
- **`PYTHON_DISABLE_REMOTE_DEBUG` / `-X disable-remote-debug` 拦不住读取**：这个开关只作用于
  `sys.remote_exec`。
- **GIL 争用下的偏斜**：两个忙线程争 GIL 时，外部 attach 的样本在线程间分配不均（81/225，
  而两线程真实 CPU 约 1:1），根因未查清。

这些门槛决定了它只能作为显式开关，不能默认开启。它与墙钟类型一起推迟、另立项。

## 5. greenlet 与 gevent（`gevent/`）

| 脚本 | mode | 构建 | 结果 |
| --- | --- | --- | --- |
| `dbg3.py`：裸 greenlet，不用 gevent | wall | 3.15.2 与 3.15.2t | 主 greenlet 里正常；进入非主 greenlet 后 `get_stack_trace()` 抛 `RuntimeError: Incomplete sample: did not reach base frame` |
| `dbg2.py`：gevent，分别测有/无 `monkey.patch_all()` | all | 3.15.2 | 只要 spawn 过 greenlet 就失败，与是否 monkey patch 无关 |
| `dbg.py` / `gevent_burst.py`：进程内，采样线程分别用 patch 后的 `threading.Thread` 和 `monkey.get_original("_thread", "start_new_thread")` 启动 | all | 3.15.2 | 100 次采样全部失败，0 个样本 |
| `gevent_burst.py ext`：外部 attach | cpu | 3.15.2 | 0 个样本 |

- gevent 的 hub 本身就是一个非主 greenlet，所以 gevent 应用几乎每次采样都失败。失败时整次样本作废，
  同一时刻其它线程的栈也一起丢失。
- monkey patch 之后，用 `threading.Thread` 启动的「采样线程」其实是 greenlet，
  `threading.get_native_id()` 与主线程相同。
- 在 cpython 和 greenlet 仓库里搜 "did not reach base frame"、"greenlet profiling.sampling"，都没有找到 issue。
  按负责人决定，不向上游提交任何东西。

### 5.1 SQLAlchemy asyncio（`sqla/`，3.15t）

环境：3.15.2t（`_is_gil_enabled()` 为 False）；SQLAlchemy 2.1.9、aiosqlite 0.22.1、greenlet 3.5.6
（都有 cp315t wheel）。负载是 asyncio 循环反复执行「AsyncSession 查询内存 sqlite」，每次查询之间
在 `burn()` 里烧 N ms CPU。对照组用 `asyncio.sleep(1 ms)` 代替查询，墙钟时间与查询相当。进程内
`mode=cpu`，100 Hz，每次 5 s。命令：`.venv/bin/python -I sqla_cpu.py {sqla,sleep} N 5`。

| 配置 | tick 失败率 | `burn` 真值 → 样本 | 查询路径在样本中的占比 |
| --- | --- | --- | --- |
| sqla，N=1（3 次） | 25.0 / 25.5 / 28.9%（复核一次为 27.7%） | 56–64% → 81–83% | 1.3–5.3%（真值约 36–44%） |
| sqla，N=5（3 次） | 7.8–9.0% | 87–89% → 96–98% | 0.2–0.7%（真值约 11–13%） |
| 对照组，N=1（2 次，复核 1 次） | 1.4 / 1.8 / 2.2% | 与真值一致 | — |
| 对照组，N=5（2 次） | 0.4 / 0.4% | 与真值一致 | — |

- 失败的报错不止一种。以 sqla N=1 的一次为例：
  - `Failed to parse initial frame in chain` 59 次；
  - `Incomplete sample: did not reach base frame` 48 次；
  - `process_vm_readv failed` 31 次；
  - `Unhandled frame owner` 7 次。
- 失败几乎都发生在查询阶段（`fail_phase` 为 step 130、burn 15，跨线程读取，有竞态）。
- `probe.py`：在 `AsyncConnection.run_sync` 的回调里（处于非主 greenlet），20 次 `RemoteUnwinder`
  调用全部报 `did not reach base frame`；在 asyncio 主 greenlet 里，20 次全部成功。
- 结论：这类局部使用 greenlet 的库会让 greenlet 里的 CPU 整段消失，火焰图因此失真，而失败率只有
  8–29%，不会触发 50% 的停用阈值。SDK 不检测 greenlet，由用户按文档判断。
- 没有 greenlet 时也有 0.4–2.2% 的基线失败（报错类型相同），根因没有逐条分析。这些失败计入
  `sample_failed` 丢弃计数。

## 6. 各 mode 与 Go SDK 的对照

- `profiling.sampling.constants`：`WALL=0`、`CPU=1`、`GIL=2`、`ALL=3`、`EXCEPTION=4`。CLI 的 `--mode`
  只提供 `wall/cpu/gil/exception`。`ALL` 只在内部使用：`--format gecko` 和栈快照子命令会强制用它
  （3.15.2 `cli.py` L1181-1186、L1229），它的含义是不跳过任何线程，并计算 GIL 与 ON_CPU 状态位
  （`sample.py` L512）。
- 在 GIL 构建的进程内，`gil` 和 `exception` mode 同样被采样线程持有 GIL 这件事污染：此刻持有 GIL 的
  正是采样线程自己。
- Go SDK（`sdk/go/profiling.go`）默认开启，每 `ProfilingInterval`（默认 60 s）用
  `pprof.StartCPUProfile` 采 10 s CPU，再各抓一次 `heap` 和 `goroutine` 快照。Go 的 CPU 剖析由内核按
  线程 CPU 时间触发 SIGPROF，不存在 GIL 那类观测偏差。Go 标准库没有 wall 剖析器，最接近的是
  `goroutine` 快照；第三方 fgprof（MIT）以约 99 Hz 反复抓 goroutine 快照，叠加成 wall 剖析。
- 平台的 `profile_meta.type` 枚举只有 `cpu/heap/goroutine`（`internal/db/schema/profile_meta.go:26`），
  所以新增墙钟类型会改协议面。

## 风险与未知项

- 3.15t 上的生态覆盖有限：未声明支持 free-threading 的 C 扩展会在导入时重新启用 GIL，这时 SDK 会停用。
- 吞吐数字受宿主机负载干扰，没有给出百分比结论。
- 外部 attach 在 GIL 争用下线程间偏斜的根因没查清；Yama scope=1 下 `PR_SET_PTRACER` 能否放行，
  只有源码论证，本机改不了 scope，无法实测。
- greenlet 的失败是否属于 Tachyon 应修的 bug，没有向上游确认（按负责人决定，不报上游）。
- 3.15t 无 greenlet 时 0.4–2.2% 的基线失败，根因没查。

## 附：多进程与平台多副本（读代码，未实测）

- 每个窗口在平台上存为一行 `profile_metas`，blob 的 key 带随机 uuid。`(project_id, upload_id)`
  唯一索引负责去重：内容相同返回 200，内容不同返回 409。两个副本并发写入时，后写的那个删掉自己的
  blob 再回查（`internal/gateway/profile_api.go:198-215`）。
- 火焰图按 `service + type + 时间窗口重叠` 读出全部记录，用 `pprof.Merge` 汇总
  （`internal/meta/profiles.go:65-85`，`profile_api.go:278-311`）。所以多个 worker、多台主机的数据
  汇总成同一服务的火焰图；多副本共享元数据库和对象存储，任一副本都能查全。
- 查询不设 `Limit`，读取量随进程数和时间窗口线性增长。`scripts/e2e-multi-replica.sh` 没有覆盖
  profile。
