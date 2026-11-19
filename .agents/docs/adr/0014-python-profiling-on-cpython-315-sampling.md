# Python SDK 只在无 GIL 的 CPython 3.15t 上做进程内 CPU 剖析

> 2026-11 修订：S2 审查实测发现，GIL 构建上的进程内采样系统性看不见短 CPU 段。启用范围因此收窄为
> 「Linux 上运行时没有 GIL 的 3.15t」，窗口对齐 Go SDK，并写明 greenlet 不支持。sidecar 与墙钟剖析
> 一起推迟、另立项。证据见 [`python-cpu-profiling-gil.md`](../research/python-cpu-profiling-gil.md)。
> 随后的 grilling 补定了失败分两级计数、`profiling_interval`、fork 后的窗口处理，并决定 SDK 不检测 greenlet。

Python SDK 一直没有剖析信号：`sdk/python/README.md` 之前直接写「CPU/pprof profiling is not
implemented in Python」。CPython 3.15 新增 `profiling.sampling`（Tachyon），调研
[`python-315-sampling.md`](../research/python-315-sampling.md) 实测确认进程内对自身 PID
采样能产出平台可接受的 pprof，且不需要 ptrace 权限，也不改协议面。

但带 GIL 的解释器上，进程内采样线程必须先拿到 GIL 才能读栈。它只有在业务线程释放 GIL
（sleep、IO、`epoll`）或被强制切换（默认 5 ms）时才拿得到，所以短于切换间隔的 CPU 段永远
被看成「停在等待处」。实测忙 1–3 ms、闲 2–6 ms 的负载里，`burn` 的 CPU 占比真值为 95–99%，
进程内的各种 mode 和加权方式给出的是 0–33%：忙 1 ms 时全部为 0，GIL 版 asyncio 忙 3 ms 时
几乎全部记到 `EpollSelector.select`。free-threaded 3.15t 上同样的负载用进程内 `mode=cpu`，可以把
84–100% 记到 `burn`（真值 94–99%）；但 3.15t 一旦重新启用 GIL（`-X gil=1`），结果又退回到 0。
我们决定：

- **只在 Linux、CPython ≥ 3.15 的 free-threaded 构建、且运行时 `sys._is_gil_enabled()` 为
  False 时采样**。下列情况打开开关只打一条 warning，不采样：3.9–3.14、3.15 GIL 构建、
  3.15t 的 GIL 被 `PYTHON_GIL=1`/`-X gil=1` 或不兼容的 C 扩展重新启用、macOS 与 Windows。
  GIL 可能在运行中被新导入的 C 扩展打开，所以每个窗口开始前重查一次，发现已启用就停用并
  warning。GIL 构建上试采不报错，只会返回空样本，特性探测拦不住，必须显式判断。
  SDK 整体兼容范围仍是 `>=3.9`，也不走 3.14 的私有模块 `_remote_debugging`。
- **只做进程内、对自身 PID 的自采样**。sidecar（attach 其他 PID）和 CLI collapsed 两条路线
  不进 SDK，理由见备选方案。
- **依赖未文档化接口**。采样线程调用
  `profiling.sampling.sample.sample(pid, collector, *, all_threads=True, mode=cpu)`，
  自定义 `profiling.sampling.Collector` 聚合栈。这两个符号不在 3.15 文档的公开 API 里，
  签名或语义可能随 CPython 小版本变化，因此启动时做特性探测（检查签名并试采），而不是维护
  版本白名单。
- **失败分两级计数，停用后不恢复**：
  - 窗口级：一个窗口包括采集（`sample()`）和编码两步，任一步抛异常，这个窗口就算失败；只有编码
    成功才把连续失败计数清零。连续 3 个窗口失败就停用。只看采集的话，编码器一直失败时计数会被
    反复清零，永远到不了 3。
  - tick 级：单次抓栈失败时，Tachyon 调用 `collect_failed_sample()`，但不抛异常。一个窗口内
    失败 tick 超过一半（50%）就停用。这个阈值只用来拦「采样基本失效」（gevent 约 100%），
    远高于 3.15t 无 greenlet 时 0.4–2.2% 的基线失败率；部分失真不归它管。窗口内的尝试次数
    （成功加失败）不到 100（100 Hz 下约 1 s）时不做判断，避免 shutdown 截断的短窗口误判。
  - 失败的 tick 计入 `record_telemetry_drop("profile", "sample_failed", n)`，`Diagnostics` 不加字段；
    全部失败的窗口不上传。
  - 停用（包括窗口前发现 GIL 已启用）后，本进程内不再恢复、不退避重试，因为原因都是环境性的。
    退避只用于上传（`DeliveryQueue`）。
  - warning 带稳定的原因码（如 `gil_build`、`gil_enabled`、`unsupported_platform`、
    `python_too_old`、`probe_failed`、`sample_failure_rate`）和用户文档链接；每个进程每类最多一条。
- **只采 CPU**：`mode=cpu`、`all_threads=True`，排除 SDK 自己的采样线程，上传 `type=cpu`。
  pprof sample type 沿用 Go SDK 的 `samples/count` + `cpu/nanoseconds`，样本值是「计数 × 周期」。
  墙钟类型以后与 sidecar 一起单独立项。
- **默认关闭**：用户显式设置 `profiling=True` 才启用；Python SDK README 标 experimental，写明
  上面的支持范围。
- **不支持 greenlet**：线程正跑在非主 greenlet 里时，Tachyon 抓栈报
  `RuntimeError: Incomplete sample: did not reach base frame`，整次样本作废，连同一时刻其它
  线程的栈也一起丢失。进程内和外部 attach、各种 mode、3.15 和 3.15t 都是这样。
  **SDK 不检测 greenlet**：持续剖析是用户显式打开的，用户文档写明「greenlet 里的代码不可见、
  不支持」，由用户自行判断，SDK 不扩大自己的责任范围。gevent、eventlet 应用会因 tick 失败率
  超过 50% 而停用。只在局部使用 greenlet 的库（例如 SQLAlchemy asyncio）不会触发停用，但火焰图
  会失真：3.15t 实测失败率 8–29%，查询路径的 CPU 从真值的 36–44% 降到样本里的 1–5%（忙 1 ms 时）。
- **prefork 每个 worker 各自采样**：用 `os.register_at_fork` 在子进程重启采样线程。子进程回调
  里要丢弃继承来的当前窗口、聚合结果和锁，全部换成新对象，从一个新窗口开始。否则 `--preload`
  时 master 在 fork 前采到的样本会被每个 worker 再上传一遍；继承来的锁如果在 fork 那一刻被
  采样线程持有，子进程会死锁。`upload_id` 在窗口结束、编码时由上传方进程生成，取 128 bit
  随机数（`secrets.token_hex(16)`，与 Go SDK 的 `newEventID` 一致）；`os.urandom` 本身 fork
  安全，不需要重新播种。labels 不带 pid。各进程、各主机的窗口在平台查询时由 `pprof.Merge`
  汇总为同一服务的火焰图；平台多副本共享元数据库与对象存储，`(project_id, upload_id)` 唯一
  索引保证重试或并发落到不同副本时不会重复入库。
- **100 Hz，每 `profiling_interval` 秒采 10 s**：新增 `Config.profiling_interval: float = 60.0`，
  语义与 Go SDK 的 `ProfilingInterval` 相同，即两个窗口起点之间的间隔。窗口长度固定 10 s，
  与 Go 的 `profilingCPUWindow` 一致，不开放配置。`≤0` 时回退到默认值（与 `export_interval`
  相同）；小于 10 s 时窗口首尾相接，相当于连续采样。不支持环境变量配置。投递复用
  `sdk/python/lazyobserve/delivery.py` 的 `DeliveryQueue`。
- **零依赖自写 pprof 编码器**：只用标准库把聚合栈编码成 gzip pprof；用 Go 侧
  `github.com/google/pprof/profile`（`internal/gateway` 已依赖）回读测试，不引入新依赖。
- **不改协议面**：仍走 `POST /api/profiles`，字段与 Go SDK 相同，`type=cpu`；
  `docs/version-matrix.md` 不需要改。

## 备选方案

- **GIL 构建照做进程内采样，在文档里标注盲区（拒绝）**：这种做法给出的不是带噪声的数据，而是方向错误的火焰图。
  典型 Web 请求的 CPU 段只有几毫秒，很可能落进盲区，CPU 会被算到 `select`、`recv`、`sleep` 上。
- **wall/all + 按线程 CPU 时钟加权（拒绝）**：每个 tick 读取 `CLOCK_THREAD_CPUTIME`，把增量记到
  此刻的栈上。对持续忙的线程准确，但在忙闲切换的负载下，增量会落到「突发结束后停住的等待栈」
  上：3.15t 忙 3 ms 时只有 37% 记到 `burn`，GIL 版为 0–1%。实际上它退化成了墙钟分布。
- **all + 状态位过滤（拒绝）**：GIL 构建上同样看不到短 CPU 段（忙 1–3 ms 时为 0–33%）。
  3.15t 上它与 `mode=cpu` 等价，但 `mode=cpu` 在 C 层过滤，空闲线程不进 Python collector，
  所以更省：1 个忙线程加 63 个空闲线程时，采样线程 CPU 在 `all` 下为单核的 15.3%，在
  `mode=cpu` 下为 9.0%。
- **用墙钟模式顺带当 CPU（拒绝）**：wall 样本包含不在 CPU 上的等待时间，sample type 必须与
  mode 一致；而且 GIL 构建上进程内 wall 同样有盲区（忙 1–3 ms 时 `burn` 为 0%，真值 33%）。
- **sidecar 子进程外部 attach（暂缓，与墙钟类型一起另立项）**：外部采样器不持有目标进程的 GIL，
  GIL 构建上的 cpu 和 wall 都准确，忙闲切换下也准。原先「需要 ptrace 权限、跨构建不能
  attach」的说法不准确：同 uid、Docker/containerd 默认 seccomp、内核 ≥ 4.8 时不需要
  CAP_SYS_PTRACE，用 `sys.executable` 拉起就是同一构建。真正的门槛如下：
  - Yama `ptrace_scope=1`（上游、Ubuntu、gVisor 的默认值）下，子进程读父进程要父进程先调
    `PR_SET_PTRACER`，而每个进程只能有一个这样的放行关系，会和崩溃上报库互相覆盖；
    scope=2/3 下不可行；
  - 降权 worker（gunicorn `--user`、uwsgi `--uid`，dumpable=0）与 PSS baseline 下会失败；
  - 每个 worker 多一个解释器（GIL 版 21 MiB，3.15t 39–41 MiB）；
  - 子进程生命周期复杂：`PR_SET_PDEATHSIG` 跟随线程，管道 EOF 被 fork 继承，应用会接管
    SIGCHLD，uwsgi/PyInstaller 下 `sys.executable` 不是 Python。

  所以它只能作为显式开关，不能默认开启。
- **原生扩展在不持 GIL 的原生线程里采样（拒绝）**：会破坏零依赖纯 Python SDK，需要按平台
  出 wheel，维护成本与本 ADR 的范围不匹配。
- **检测到 `greenlet` 模块就停用（拒绝）**：能确定性地拦住 SQLAlchemy asyncio 这类局部失真，
  但会误伤只导入、未使用 greenlet 的应用，也把「用户自己的部署选择」变成了 SDK 的责任。
  改为在文档里写明。
- **停用后退避重试（拒绝）**：停用原因（greenlet、C 扩展打开 GIL、CPython 接口变化）不会
  自己消失，重试只会重复打 warning，或者静默空转。
- **给 `Diagnostics` 加 profiling 专用字段（拒绝）**：`Diagnostics` 是投递损失的快照，加字段就是
  改公开 API，还会与 Go SDK 不对齐；失败 tick 走已有的 `record_telemetry_drop`，用 `reason` 区分。
- **CLI collapsed 转换（拒绝）**：丢完整文件路径，还要解析别人的文本格式，契约更脆。
- **3.14 及更早用 `_remote_debugging`（拒绝）**：私有模块、接口与 3.15 不同，而且同样受 GIL
  限制，维护两条实现的成本高于覆盖收益。
- **版本白名单（拒绝）**：未文档化接口的变更不会跟着版本号走，白名单需要持续维护；
  特性探测能直接回答「这台解释器能不能采」。
- **引入第三方 pprof 编码库（拒绝）**：为 profile.proto 的一个子集新增运行时依赖，还要过
  许可证与依赖门禁，收益不抵成本。

## 后果

- Python SDK 多一条只在 Linux 3.15t 上生效的剖析路径，绝大多数 GIL 构建用户拿不到 CPU 剖析；
  3.9–3.14 和 GIL 构建的行为不变，只是开关打开时多一条 warning。覆盖 GIL 构建要等 sidecar 立项。
- CPython 升级可能打破采样：失效时 SDK 自动停用并 warning，不抛错、不中断业务；CI 固定跑
  最新 3.15.x，另有一个非阻塞的 3.16 预发布 job 提前暴露签名变化。
- pprof 编码器成为 SDK 的内部契约，由 Go `pprof` 解析器回读测试钉住；Python 侧只做结构断言，
  避免自己解码自己。
- 采样开销落在应用进程内。3.15t 上 `mode=cpu` 实测（每次 4 s、栈深 30、每组 3 轮、机器 load
  1.3–4.5，吞吐只作参考）：采样线程约占单核 1.75%（1 个线程）、3.6%（16 个）、9.0%（64 个），
  随线程数增长；这是因为 `mode=cpu` 每个 tick 都要对每个线程读一次 `/proc/<pid>/task/<tid>/stat`。
  导入后 RSS 增加 15–19 MiB；忙线程吞吐三轮均值变化在 ±3.2% 以内、单轮约 ±5%，与 `none` 基线
  自身的轮间波动（最多 4.8%）相当，与噪声无法区分。按每 60 s 采 10 s 摊下来，平均开销约为上述
  数字的 1/6。
- 火焰图只能看整个服务的汇总，不能按进程或实例拆开看（Go SDK 也不发 labels）。以下两条是平台
  已有的特性，与 Python 无关，这次不处理：
  - 火焰图查询读取时间窗口内的全部 blob 再合并，没有数量上限（`profileQuery` 不设 `Limit`）。
    worker 数越多，单次查询读得越多：16 个 worker 看 1 小时约 960 份。
  - HA E2E（`scripts/e2e-multi-replica.sh`）没有覆盖 profile；多副本并发去重的结论来自读代码。
