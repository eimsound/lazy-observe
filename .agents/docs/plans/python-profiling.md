# Python SDK 持续剖析实施计划

## 目标与来源

- 源规格：`.agents/docs/research/python-315-sampling.md`（spike 结论）、
  `.agents/docs/research/python-cpu-profiling-gil.md`（S2 审查后的 GIL 与忙闲切换实测）、
  负责人 2026-11 的切片决策（S1–S4 串行，不阻塞 v0.1.0），以及 ADR 0014（2026-11 修订）。
- 最终状态：Python 应用显式设置 `profiling=True` 后，SDK 只在 Linux、CPython 3.15+
  free-threaded 构建、且运行时 `sys._is_gil_enabled()` 为 False 时，以
  `mode=cpu`、`all_threads=True`、100 Hz 采样自身进程，每 60 s 采 10 s 并聚合成 pprof，
  经 `POST /api/profiles` 上传；`type=cpu`，协议面不变。3.9–3.14、3.15 GIL 构建、
  GIL 被重新启用的 3.15t、macOS/Windows 打开开关只打一条 warning、不采样。默认关闭，
  README 标 experimental，写明支持范围，并写明不支持 greenlet（gevent、eventlet）。
- 范围外：
  - sidecar（attach 其他 PID）与 CLI collapsed 两条采样路线（决策 2）；sidecar 与墙钟剖析
    一起推迟、另立项，是覆盖 GIL 构建的唯一候选。
  - 3.14 私有模块 `_remote_debugging` 和 3.9–3.14 的任何采样实现（决策 3）。
  - GIL 构建上的任何进程内采样，包括 wall/all + 线程 CPU 时钟加权和状态位过滤
    （ADR 0014 备选方案）。
  - wall/gil/exception 采样类型与 pprof sample type 的扩展；墙钟剖析单独立项（决策 5）。
  - 协议面、`docs/version-matrix.md`、`CHANGELOG.md`、README 的改动放在 S4 或完全不改（决策 5、16）。
  - 白名单式版本判断；只做特性探测（决策 4）。

## 仓库证据

- `sdk/go/profiling.go:18-20`：Go SDK 的窗口、队列与超时常量；`:58-95` profile 投递队列
  （容量 6、`BatchSize=1`）；`:161-201` multipart 上传，字段为 `profile` 文件、`service`、
  `upload_id`、`type`、`start`、`end`，`Authorization: Bearer <token>`。
- `internal/gateway/profile_api.go:95-235`：`profileIngest` 的契约。文件字段名 `profile`、
  最大 10 MB、必须以 gzip 开头（`:123`），`:134` 用 `pprof.ParseData` 解析；
  `:138-145` 校验 `service`、32 位十六进制 `upload_id`、`type`、RFC3339 `start`/`end`；
  `:147-152` 可选 JSON `labels`；`:241-248` 的 `validProfileUploadID` 要求 32 位 hex；
  `:376-379` 的 `parseProfileType` 只接受 `cpu`/`heap`/`goroutine`；同 `upload_id`
  同内容返回 200，不同内容返回 409（`existingProfileUploadResponse`，`:222-238`）。
- `internal/gateway/profile_api.go:134-136` 用 `github.com/google/pprof/profile` 解析上传内容；
  `go.mod:18` 已有该依赖，服务端没有因此新增依赖。
- `sdk/python/lazyobserve/delivery.py:25-111`：`DeliveryQueue`，容量含在途批次、满队列丢弃、
  指数退避、`close(timeout)` 排空；`:167-212` 的重试与丢弃原因
  （`queue_full` / `retry_exhausted` / `shutdown_budget`）。S1 的 profile 投递直接复用它。
- `sdk/python/lazyobserve/error_event.py:156-209`：`ErrorTransport` 的 ingest 形态——
  `urllib.request`、`Authorization: Bearer`、独立后台队列。profile 上传沿用同一套 stdlib 与鉴权。
- `sdk/python/lazyobserve/client.py:269-365`：`build_client` 组装各信号；`:357-365` 用
  `default_warn_factory` 把投递丢弃接到 logger；`:231-248` 的 `shutdown` 统一关闭队列与 provider。
- `sdk/python/lazyobserve/config.py:37-63`：`Config` 与 `DeliveryConfig` 的默认值/归一化；
  S3 的新开关落在这里。
- `sdk/python/pyproject.toml:7-13`：版本 0.1.0，`requires-python = ">=3.9"`；S1–S4 不改版本。
- `.agents/docs/research/python-315-sampling.md:29-44`：路线 (a) 进程内自采样不需要 ptrace；
  `inspect.signature(profiling.sampling.sample.sample)` 在 3.15.2 上是
  `(pid, collector, *, duration_sec=None, all_threads=False, ..., mode=0, ...)`；
  `mode=0` 是 wall，`all_threads=False` 只采主线程，`constants.PROFILING_MODE_WALL = 0`。
  同一文件 `:83-100` 记录 PoC 的 pprof 编码器与上传脚本，仅作只读参考。
- 现有 Python 测试惯例：`sdk/python/tests/test_delivery.py:1-120` 直接断言队列行为；
  `sdk/python/tests/test_error_event.py:14-43` 用 `ThreadingHTTPServer` 捕获 HTTP；
  `sdk/python/tests/helpers.py:15-47` 组装不产生网络流量的 `Client`。
- `.agents/docs/adr/0013-signal-storage-is-victoria-only.md`：现有 ADR 的结构——背景与决定、
  `## 备选方案`、`## 后果`。

## 实施策略

- **推荐路线（先钉字节契约，再接运行时）**：S1 先把「聚合栈 → pprof 字节 → multipart 上传」
  做成可移植的私有模块，用 Go `pprof` 回读和 fake opener 在 3.9+ 上验证；S2 再实现只在
  3.15+ 可用的采样器；S3 把采样器、窗口和公开开关接进 `Client` 生命周期；S4 补 CI 矩阵和文档。
  理由：S1 的测试不依赖 3.15 解释器，3.9/3.13 阻塞 job 也能跑；未文档化 API 的风险被推迟到
  S2 且被特性探测隔离；pprof 契约由平台自己的解析器（`github.com/google/pprof/profile`）
  独立验证，而不是自己写解码器自证。
- **备选路线 A（先端到端采样，再补上传）**：S1 直接写 3.15 采样器 + 上传，用真实解释器跑通
  一个窗口。否决：red 测试需要 3.15，3.9/3.13 的阻塞 CI 无法提供反馈；一旦未文档化 API
  签名变化，整个切片一起红，无法区分是采样还是编码/上传的问题。
- **备选路线 B（引入第三方 pprof 编码库）**：用现成 protobuf/pprof 库替代自写编码器。
  否决：决策 9 要求零依赖；新增 protobuf 运行时依赖还要过许可证与 `mise run license-check`，
  而 profile.proto 需要的子集很小。
- **备选路线 C（改协议面新增 `type=python-cpu` 或字段）**：否决：决策 5 明确不改协议面，
  服务端 `parseProfileType`（`internal/gateway/profile_api.go:376`）只接受 cpu/heap/goroutine。

## 已确认的测试接缝

- `lazyobserve._pprof.encode_cpu_profile(...)`：返回 gzip pprof 字节。用 Go
  `github.com/google/pprof/profile.Parse` 回读，证明平台能消费；Python 侧只做结构断言
  （sample type、string table、去重、空输入）。
- `lazyobserve._profile_upload.ProfileUploader`：对外只有 `enqueue`/`close`/`dropped`。
  测试注入 fake opener，断言请求 URL、鉴权头、multipart 字段、409/4xx 静默、5xx 重试。
- S2 的采样器接缝：`Sampler.collect(...)` 接收 leaf-first 帧列表（与 3.15 `Collector` 回调
  同形），测试用 fake `profiling.sampling` 模块注入，不需要真实 3.15 解释器。
- S3 的公开接缝：`Config.profiling` + `Client.shutdown(timeout)` 的可观察行为
  （是否发请求、是否打 warning、窗口是否在预算内上传）。

## 依赖顺序

1. S1：pprof 编码器（无依赖）→ fixture + Go 回读（依赖 1）→ profile 投递（依赖 1）。
2. S2：采样器（依赖 S1 的聚合输入形状；3.15 特性探测）。
3. S3：公开开关与生命周期（依赖 S1 投递、S2 采样器）。
4. S4：CI 矩阵与文档（依赖 S1–S3 的可运行行为）。

## ADR 0014 修订带来的切片调整（2026-11）

S2 审查实测：GIL 构建上进程内采样看不见短 CPU 段，3.15t 重新启用 GIL 后也一样；greenlet 下
Tachyon 抓栈必然失败。证据见 `.agents/docs/research/python-cpu-profiling-gil.md`。各切片的追加
要求如下，S2 的原交付不在此重写：

- **S2（本地 `d30b98c` 恢复时）**：
  - 保留 `mode=cpu`、`all_threads=True`、按 `len(timestamps_us)` 计数；S1 编码器不改。
  - 可用性判断新增三道门：`sys.platform == "linux"`；free-threaded 构建
    （`sysconfig.get_config_var("Py_GIL_DISABLED")`）；运行时 `sys._is_gil_enabled()` 为 False。
    任一不满足，就返回「不可用 + 原因」。GIL 构建上试采不报错，只会返回空样本，所以这三道门必须
    放在试采之前。
  - 暴露一个「窗口前重查」接口（只查 GIL 状态），供 S3 每个窗口开始前调用。
  - 失败分两级计数：
    - 窗口级：一个窗口 = collect（`sample()`）+ encode，任一步抛异常都算窗口失败；只有 encode
      成功才把计数清零；连续 3 个失败窗口就停用（`MAX_CONSECUTIVE_FAILURES = 3`）。#94 已经这样实现。
    - tick 级：统计窗口内 `collect_failed_sample()` 的次数，失败超过一半就停用
      （`FAILURE_RATE_THRESHOLD = 0.5`，#94 里的 0.2 要改）。窗口内尝试次数（成功加失败）
      不到 `MIN_FAILURE_RATE_ATTEMPTS = 100` 时不判断，保留 #94 的实现。
    - 失败率以 `last_failure_rate()` 暴露给 S3，S2 本身不接 `record_telemetry_drop`。
    - `recheck_gil()` 发现 GIL 已启用就永久停用；它自身不打 warning，带原因码的 warning 由 S3 打。
    - 窗口长度和间隔在 S2 里保持常量（`WINDOW_SECONDS = 10.0`、`WINDOW_INTERVAL_SECONDS = 60.0`）；
      面向用户的 `profiling_interval` 在 S3 实现。
  - 停用后在本进程内不再恢复，删掉 `backoff_seconds()` 的重试用途，退避只留给上传。
  - 失败 tick 数交给 S3，由 S3 计入 `record_telemetry_drop("profile", "sample_failed", n)`。
  - 不检测 greenlet（ADR 0014：用户自己打开、自己负责，只在文档里写明）。
  - Red：用 fake 模块分别断言：
    - GIL 构建、`_is_gil_enabled()` 为 True、非 Linux 时返回不可用原因码；
    - 连续 3 个窗口失败（collect 抛异常，或 collect 成功但 encode 抛异常）时停用；collect 成功、
      encode 失败不会把计数清零；
    - 一个窗口内失败 tick 为 51% 时停用，49% 时不停用；尝试次数不到 100 时，即使 100% 失败也不停用；
    - 停用后不再开新窗口。

    真实 3.15t 测试改为纯 Python 忙线程负载（不再依赖释放 GIL 的 hashlib）；真实 3.15 GIL 测试
    断言返回不可用。
  - `d30b98c` 的执行日志里写着「等待负责人决定默认 mode」，现已决定：保持 `mode=cpu`。
- **S3**：
  - 新增 `Config.profiling_interval: float = 60.0`（秒）：两个窗口起点之间的间隔，语义同 Go 的
    `ProfilingInterval`；窗口固定 10 s；`≤0` 时回退默认值；小于 10 s 时窗口首尾相接。
  - 每个窗口开始前调用 S2 的 GIL 重查，发现已启用就停用。
  - warning 带原因码和文档链接，每个进程每类最多一条。
  - 失败 tick 计入 `record_telemetry_drop("profile", "sample_failed", n)`，`Diagnostics` 不加字段；
    全部失败的窗口不上传。
  - fork 后子进程丢弃继承来的窗口、聚合结果和锁，重建后从新窗口开始；`upload_id` 在窗口结束、
    编码时生成。
- **S4**：
  - CI 中 3.15 GIL 构建的 job 只验证「打 warning、不采样」，真实采样只在 3.15t job 里跑。
  - 用户文档写明：支持范围；所有打 warning、不采样的情况；`profiling_interval`；不支持 greenlet
    （gevent、eventlet 会自动停用；SQLAlchemy asyncio 等库不停用，但 greenlet 里的 CPU 不可见）；
    实测开销。

## 实施步骤

### S1 编码与投递（含 ADR 0014）

- [x] 1.1 pprof 编码器
  - 交付：`sdk/python/lazyobserve/_pprof.py`，零依赖、纯标准库、兼容 3.9；输入 leaf-first
    帧列表（函数名、文件名、行号）→ 样本计数，以及采样周期/窗口起止；输出 gzip pprof。
    sample type 固定 `samples/count` + `cpu/nanoseconds`，`period_type=cpu/nanoseconds`，
    `period = 1e9/采样频率`；string table 第 0 项为空串，function/location 去重；
    gzip 头 `mtime=0` 保证字节确定。
  - 阻塞于：无。
  - 涉及位置：新文件 `sdk/python/lazyobserve/_pprof.py`。
  - Red：`sdk/python/tests/test_pprof.py` 用自写的最小 protobuf 解码器断言 sample type、
    period、样本值、function/location 去重、空输入、gzip 确定性；预期 `ModuleNotFoundError`
    / 断言失败。命令：`mise run sdk-python`。
  - Green：实现编码器，只覆盖上述行为。
  - 验证：`mise run sdk-python`。
  - 验收：测试从红转绿；同一输入两次编码字节相同。
- [x] 1.2 Go pprof 回读 fixture
  - 交付：确定性 fixture
    `internal/gateway/testdata/python_profiling/cpu.pprof.gz`，
    生成脚本 `sdk/python/tests/generate_pprof_fixture.py`，回读测试
    `internal/gateway/python_pprof_test.go`；pytest 有「当前编码器输出 == 提交的 fixture」的
    防漂移断言。
  - 阻塞于：1.1。
  - 涉及位置：`internal/gateway/profile_api.go` 的解析契约；Go 侧不新增依赖
    （`go.mod:18` 已有 `github.com/google/pprof`）。
    放在 `internal/gateway` 的理由：profile 的消费方就是该包的 `pprof.ParseData`，且
    `sdk/go/go.mod` 没有 pprof 依赖，不能为测试把它带进 SDK 模块；代价是改动 `internal/`
    会触发服务端 Go 全套 CI，可接受。
  - Red：先写 Go 测试（fixture 不存在时 `os.ReadFile` 失败）与 pytest 防漂移断言
    （fixture 不存在时失败），命令：`go test ./internal/gateway -run PythonPprof`、
    `mise run sdk-python`。
  - Green：生成并提交 fixture；测试断言 sample type、样本数/值、函数名、行号、period。
  - 验证：`go test ./internal/gateway -run PythonPprof`、`mise run sdk-python`。
  - 验收：Go 解析器能读出 100 个样本总量、4 个函数、4 个 location，且 pytest 防漂移为绿。
- [x] 1.3 profile 上传与投递
  - 交付：`sdk/python/lazyobserve/_profile_upload.py`：`CollectedProfile` 数据类、
    `new_upload_id()`（每个窗口 `secrets.token_hex(16)`，32 位 hex，与 Go SDK 的
    `newEventID` 一致）、`ProfileUploader`
    （multipart POST `endpoint + /api/profiles`，字段与 Go SDK 一致；复用 `DeliveryQueue`，
    容量 6、batch 1、发送超时 15 s；409/4xx 静默丢弃、计入 `dropped()`（reason=`rejected`）
    且不重试；5xx/网络错误抛给队列重试）。
  - 阻塞于：1.1。
  - 涉及位置：新文件 `sdk/python/lazyobserve/_profile_upload.py`；复用
    `sdk/python/lazyobserve/delivery.py:25`。
  - Red：`sdk/python/tests/test_profile_upload.py` 断言 multipart 字段/文件名/鉴权、
    `upload_id` 唯一且 32 位 hex、404/409 只尝试一次、不抛错且计入 `rejected` 丢弃、
    503 重试后成功；预期导入失败。命令：`mise run sdk-python`。
  - Green：实现上传器，只覆盖上述行为。
  - 验证：`mise run sdk-python`。
  - 验收：测试从红转绿，无新增第三方依赖。
- [x] 1.4 ADR 0014
  - 交付：`.agents/docs/adr/0014-python-profiling-on-cpython-315-sampling.md`，
    记录依赖未文档化接口、仅 ≥3.15、特性探测、默认关闭、只采 cpu，引用调研文档。
  - 阻塞于：无（与 1.1–1.3 同 commit）。
  - Red/Green：文档切片，无测试；用 `rg` 确认引用的路径存在。
  - 验收：ADR 编号顺延、格式与既有 ADR 一致。

### S2 采样器

- [x] 2. 3.15 进程内 CPU 采样器
  - 交付：`lazyobserve/_sampler.py`：启动时用 `inspect.signature` 探测
    `profiling.sampling.sample.sample` 的 `pid`/`collector`/`all_threads`/`mode` 参数；
    签名不符或试采失败则不启用。采样线程对本进程 PID 调用
    `sample(os.getpid(), collector, all_threads=True, mode=cpu)`；自定义 `Collector`
    聚合 leaf-first 帧（函数名、文件名、行号）→ 计数；函数名用 `co_qualname`（3.11+），
    避免同一文件里不同类的同名方法在 Function 去重时被合并；排除 SDK 自己的采样线程；
    连续 N 次采集失败自动停用并 `slog` 等价 warning；退避后重试窗口。
    编码器对非法帧会抛 `TypeError`/`ValueError`，采样线程必须捕获并计入采集失败，
    不能让异常冒到业务线程。
  - 阻塞于：S1（1.1 的聚合输入形状、1.3 的投递）。
  - 涉及位置：新文件 `sdk/python/lazyobserve/_sampler.py`；只读调研文档
    `.agents/docs/research/python-315-sampling.md:35-49`。
  - Red：`sdk/python/tests/test_sampler.py` 用 fake `profiling.sampling` 模块断言
    `mode`/`all_threads`/排除采样线程/连续失败自动停用；`test_sampler_unavailable.py`
    在 3.9–3.14 上断言特性探测不导入私有模块、不采集。命令：`mise run sdk-python` 与
    3.15 job 的同一 pytest 命令。
  - Green：实现最小采样器与特性探测。
  - 验证：`mise run sdk-python`；3.15/3.15t job 里跑 `pytest tests/test_sampler.py`。
  - 验收：3.15 上能产出非空聚合栈；3.14 及更早只返回不可用原因；采样线程不出现在样本里。
  - 落地说明（实测与实现细节；决策本身见上面的「ADR 0014 修订带来的切片调整」，这里不重复）：
    - 采样间隔不在 `sample()` 参数里，`SampleProfiler` 从 `collector.sample_interval_usec`
      读取，所以 100 Hz 由自定义 collector 上的 `sample_interval_usec = 10_000` 表达。
    - 帧元组的 `funcname` 实测就是 qualname（`Outer.method.<locals>.inner`），不需要额外取
      `co_qualname`。
    - `ThreadInfo.thread_id` 是 OS 线程 id（`threading.get_native_id()`），不是
      `threading.get_ident()`，排除采样线程按 native id 做。
    - `SampleProfiler.sample` 会往 stdout 打统计行，采样器把 `profiling.sampling.sample` 模块级
      `print` 换成 no-op，避免污染应用 stdout。
    - GIL 构建上 `mode=cpu` 只能看到「此刻不在 Python 字节码里（已释放 GIL）且仍在 CPU 上」的线程；
      free-threaded 3.15t 无此限制。已决定保持 `mode=cpu`，GIL 构建不采样。
    - 三道门的原因码是 `not-linux`、`gil-build`、`gil-enabled`（`<3.15` 仍先判断，原因码
      `python<3.15`）；`_is_gil_enabled` 属性缺失或调用抛错都按「GIL 已启用」处理。三道门在私有模块
      导入之前，测试断言导入和试采都没有发生。S3 把这些原因码映射成 ADR 里的 warning 原因码。
    - tick 的口径：成功尝试按每次 `collect()` 的 `len(timestamps_us)` 计，空栈和采样线程自身的栈
      也算成功尝试，失败尝试按 `collect_failed_sample()` 计；失败率 = 失败 ÷（成功 + 失败）。
      `MIN_FAILURE_RATE_ATTEMPTS = 100` 是实现里的内部保护（避免被 shutdown 截断的短窗口一次失败
      就停用），不是用户配置。
    - `collect_window` 返回 `SamplingWindow`（含 `failed_samples`、`attempts`、`failure_rate`）；
      超过阈值的窗口、全部 tick 都失败的窗口都返回 `None`（不上传）。丢弃的窗口的失败 tick 数仍可
      从 `Sampler.last_failed_ticks()` 读到（每次 `collect_window` 开头清零），S3 据此计
      `sample_failed`；`Sampler` 自身不接 `record_telemetry_drop`。
    - 停用状态（`reason()`）：`too-many-failures`、`failure-rate-exceeded`、`gil-enabled`，都不恢复；
      `backoff_seconds()` 与 `RETRY_BACKOFF_*` 已删除。
    - `Sampler` 只供窗口循环单线程驱动，`recheck_gil`/`collect_window`/`encode_window` 不加锁；
      `collect_window` 本身不重查 GIL，S3 必须在每个窗口前调用 `recheck_gil()`。
    - 采样器是阻塞式窗口接口，不自己 sleep，也没有窗口循环（S3 做）。

### S3 公开 API 与生命周期

- [x] 3. 开关、窗口与 shutdown
  - 交付：`Config.profiling: bool = False` 与 `normalize_config` 默认值；
    `build_client` 在开启时探测可用性，不可用（<3.15、GIL 构建、GIL 已启用、非 Linux）
    只打一条带原因的 warning；
    `Config.profiling_interval: float = 60.0`，`≤0` 时回退默认值；
    窗口循环与 Go SDK 一致：每 `profiling_interval` 秒采 10 s（100 Hz、`mode=cpu`、
    `all_threads=True`），间隔小于 10 s 时窗口首尾相接；每个窗口开始前重查 GIL 状态，发现已启用
    就停用并 warning；停用后不恢复；每个窗口在结束、编码时才调用 `new_upload_id()`，然后投递；
    失败 tick 计入 `record_telemetry_drop("profile", "sample_failed", n)`，全部失败的窗口不上传；
    `os.register_at_fork` 在子进程里丢弃继承来的窗口、聚合结果和锁，重建后重启采样线程；
    `upload_id` 每窗口取 128 bit 随机数（`os.urandom` fork 安全），无需重新播种；
    `shutdown(timeout)` 立即停采，当前窗口已满 5 秒就在预算内上传，否则丢弃。
    窗口编码遇到非法帧时捕获编码器的 `TypeError`/`ValueError`，不能让异常冒到
    业务线程或 shutdown 路径。
  - 阻塞于：S1、S2。
  - 开工前核对：Go SDK 的 shutdown 处理是否与决策 14 一致（`sdk/go` 的
    `profilingLoop` 在 `ctx.Done()` 后直接返回，S3 需要对齐或记录差异）。
    S2 的 `collect_window` 是阻塞的一次 `sample()`，S2 不提供中途停止；「立即停采」可用 3.15.2
    `SampleProfiler.sample` 每轮检查的 `collector.running`（`profiling/sampling/sample.py:157`，
    为 False 即 break，随后照常 flush 已聚合的栈），由 S3 在 `_AggregatingCollector` 上加
    `running` 并从 shutdown 线程置 False。探测时的 0.1 s 试采也应放在后台线程，不阻塞 `build_client`。
  - 涉及位置：`sdk/python/lazyobserve/config.py:37`、`client.py:231`、`client.py:269`。
  - Red：`test_config.py` 断言 `profiling` 默认 False、`profiling_interval` 默认 60 且 `≤0` 回退
    默认值；`test_profiling_lifecycle.py` 用 fake 采样器与 fake 上传器断言：
    - 不可用时只 warning 一次，且带原因码；
    - 默认每 60 s 一个 10 s 窗口，`profiling_interval=5` 时窗口首尾相接；
    - 窗口前 GIL 重查为启用时停用并 warning，之后不再开窗口；
    - 失败 tick 计入 `sample_failed`，全部失败的窗口不上传；
    - 满 5 s 的窗口在 `shutdown` 内上传，不足 5 s 丢弃；
    - fork 后子进程重新注册；在窗口中间 fork 时，父子进程的 `upload_id` 不同、子进程不上传
      fork 前的样本、子进程不死锁。
    命令：`mise run sdk-python`。
  - Green：接上开关与生命周期，只覆盖上述断言。
  - 验证：`mise run sdk-python`；3.15/3.15t job。
  - 验收：默认不采样；显式开启且满足启用条件（Linux + 3.15t + 无 GIL）时产生窗口；
    `shutdown` 不泄漏线程。
  - 落地说明（实现细节与核对结论；决策本身见上面的「ADR 0014 修订带来的切片调整」，这里不重复）：
    - 新模块 `lazyobserve/_profiling.py`，核心是 `ProfilingController`：一个 daemon 线程
      `lazyobserve-profiling`，探测（含 0.1 s 试采）、窗口循环都在这个线程里，`build_client` 只负责
      `start()` 就返回。`Client.profiling` 持有它，默认 `None`；启动失败只打 `internal_error` warning，
      不让 client 变成 degraded。所有依赖（采样器工厂、上传器工厂、时钟、sleep、`record_drop`）都可注入，
      Red 用 fake 采样器 + fake 时钟 + fake 上传器，不采真实解释器、不等真实 10 s。
    - 窗口循环：`recheck_gil()` → `collect_window(10 s)` → `last_failed_ticks()` 计入
      `record_telemetry_drop("profile", "sample_failed", n)`（被丢弃的窗口也计）→ `encode_window` →
      编码成功后才 `new_upload_id()` → 入队；延迟 = `profiling_interval` − 本轮已用时间，下限 0
      （< 10 s 时首尾相接）。窗口、编码失败的前两次是 `debug` 日志，不打 warning。
    - `Config.profiling_interval` 的归一化：不是「正且有限」（`≤0`、NaN、±inf）都回退 60；`Event.wait(inf)`
      会 `OverflowError`，所以 inf 也要回退。`init()` 现在认识这两个键（此前 `profiling` 会被当作未知键
      丢弃，`test_config.py` 里相应注释已删）。
    - 停采：`_AggregatingCollector.running`（默认 True，3.15.2 的 `SampleProfiler.sample` 每轮用
      `hasattr(collector, "running") and not collector.running` 判断并 break，`finally` 里照常 flush 已聚合
      的栈）；新增 `Sampler.stop()`，从 shutdown 线程置 False，并拒绝之后的窗口。`collect_window` 先发布
      collector、再复查 stop 标志，`stop()` 先置标志、再读 collector，两步都在同一把 `_stop_lock` 里，
      两边谁后执行谁看得到对方，stop 不会丢。
      stop 是请求不是失败：不计失败、不停用。
    - warning：不接采样器的自由文本回调（那条只写 `debug`）。原因码映射 `_profiling.warning_code`：
      `python<3.15`→`python_too_old`，`not-linux`→`unsupported_platform`，`gil-build`→`gil_build`，
      `gil-enabled`→`gil_enabled`，`import-failed`/`signature-mismatch`/`mode-constant-missing`/
      `trial-sample-failed`→`probe_failed`，`too-many-failures`→`too_many_failures`（ADR 示例码之外新增的一个，
      对应连续 3 个窗口失败），`failure-rate-exceeded`→`sample_failure_rate`，其余→`internal_error`。
      每个进程每个码最多一条（模块级集合 + 锁，fork 子进程里重置）。格式
      `lazyobserve continuous profiling is off: reason=<码> (<detail>); see <DOC_URL>`。
      `DOC_URL` 指向 `docs/sdk/python.md#continuous-profiling`：**S4 必须在 `docs/sdk/python.md` 建这一节**，
      并逐码说明；本切片不改用户文档。
    - 上传器：`build_uploader` 把 `DeliveryQueue` 的丢弃同时计入 `record_telemetry_drop("profile", reason, n)`
      并 `logger.warning`，与 Go 的 `newProfileDelivery.Warn` 对齐；队列容量用 S1 默认 6（Python 的
      `DeliveryConfig` 没有 profile 容量字段，不新增）。
    - fork：`os.register_at_fork(after_in_child=...)` 在第一个 controller `start()` 时注册一次（不可注销，
      controller 用弱引用跟踪）。子进程里对「已启动、未关闭、未停用」的 controller 换掉锁、stop event、
      采样器（含窗口与失败计数）、上传器（队列、锁、已不存在的 worker 线程和父进程排队的窗口），再起新线程，
      子进程重新探测并从新窗口开始。已 `shutdown` 或已因环境原因停用的 controller 在子进程里保持关闭，
      不重复 warning。fork 钩子不依赖任何继承来的锁（跟踪列表是只追加的 list，没有锁）。
    - `shutdown(timeout)`：设 `closed`、置 stop、`Sampler.stop()`、在剩余预算内 join 采样线程（线程里
      照常编码、入队 ≥5 s 的窗口，编码器异常由 `Sampler.encode_window` 和线程顶层 `except Exception` 接住，
      不会出现在 shutdown 路径），再用剩余预算 `uploader.close()`。幂等；`Client.shutdown` 的顺序是
      error transport → profiling → OTel provider，整个 profiling 共用 `Client.shutdown` 的同一个 deadline。
      join 超时后采样线程若晚一步入队，`DeliveryQueue` 记 `shutdown_budget` 丢弃。
    - **与 Go SDK shutdown 的对照（开工前核对的结论）**：顺序一致（error → profiling 停采 → profile 队列
      `close(ctx)` → provider），共用一个 ctx/deadline，幂等（`sync.Once`/`closed` 标志）。差异只有一处：
      Go 的 `profilingLoop` 在 `ctx.Done()` 后 `StopCPUProfile`，**不论窗口已跑多久**都把部分窗口入队，
      与决策 14（满 5 s 才上传、否则丢弃）不一致；Python 按决策 14 实现，Go 未改（范围外，需要的话单独立项）。
      另：Python 的 `error_transport.close(timeout)` 仍拿完整 timeout（既有行为，S3 未动），所以最坏情况下
      总耗时可超过 `timeout`；profiling 与 provider 部分没有这个问题。
    - greenlet：不检测（ADR 0014），代码里没有任何 greenlet 相关逻辑。
    - 协议面（端点、字段、DSN、鉴权）没有改动，`docs/version-matrix.md` 未改。
    - 本切片没处理的既有问题（只记录）：`ErrorTransport`/`DeliveryQueue` 同样没有 fork 后重建（子进程里
      error 队列的 worker 线程不存在）；仅开 `traces` 时 `Client.shutdown` 会返回
      `RuntimeError("metrics shutdown: 'NoOpMeterProvider' object has no attribute 'shutdown'")`
      （`meter_provider` 是 NoOp 对象而不是 `None`），冒烟里碰到，与 profiling 无关。

### S4 CI 与文档

- [x] 4. CI 矩阵、README 与 CHANGELOG
  - 交付：`.github/workflows/ci.yml` 的 Python SDK job 矩阵：
    3.9、3.13、3.15、3.15t 阻塞，3.16 预发布非阻塞；3.9/3.13/3.15（GIL）只验证
    「开关打开打 warning、不采集」，真实采样只在 3.15t 上跑。`sdk/python/README.md` 与
    `docs/sdk/python.md` 标注 experimental 与启用方式，并写明：
    - 支持范围：Linux + CPython 3.15t，运行时无 GIL；
    - 打 warning、不采样的情况：3.9–3.14、3.15 GIL 构建、GIL 被 `PYTHON_GIL=1`/`-X gil=1` 或
      不兼容的 C 扩展重新启用、macOS/Windows；
    - 不支持 greenlet（gevent、eventlet 会因失败率超过 50% 而自动停用；SQLAlchemy asyncio 等局部
      使用 greenlet 的库不会停用，但 greenlet 里的 CPU 在火焰图上不可见）；
    - 采样节奏（每 `profiling_interval` 秒采 10 s，默认 60）与实测开销；
    - 火焰图是整个服务所有进程、实例的汇总，不能按进程拆开看。
    - `docs/sdk/python.md` 里建 `## Continuous profiling` 一节，锚点必须是 `#continuous-profiling`
      （S3 的 warning 文本 `_profiling.DOC_URL` 已指向它），逐条说明 warning 原因码：`python_too_old`、
      `unsupported_platform`、`gil_build`、`gil_enabled`、`probe_failed`、`too_many_failures`、
      `sample_failure_rate`、`internal_error`。

    `CHANGELOG.md` 记一条；
    版本号保持 0.1.0；不改 `docs/version-matrix.md`（协议面不变）。
  - 阻塞于：S1–S3。
  - 涉及位置：`.github/workflows/ci.yml`、`sdk/python/README.md`、`docs/sdk/python.md`、
    `CHANGELOG.md`；`mise.toml` 的 `sdk-python` 任务若需按版本跑同一命令再同步。
  - Red：`mise run workflow-check` 在矩阵改动前对预期断言失败（如果新增 job 需要它认识）。
  - Green：更新矩阵与文档。
  - 验证：`mise run workflow-check`、`mise run ci-changes-test`、`mise run sdk-python`。
  - 验收：Python SDK job 在四个阻塞版本上绿；3.16 预发布红不挡合并；README 明确
    experimental、默认关闭、支持范围与不支持 greenlet。
  - 落地说明（实现细节与核对结论；决策本身见上面的「ADR 0014 修订带来的切片调整」，这里不重复）：
    - **`CHANGELOG.md` 不在本 commit**：按 `do` 的「收尾」约定走独立 `chore/changelog-*` 分支，由主线程另派；上面「交付」里
      的 `CHANGELOG.md` 一条因此留给那条分支。版本号仍是 0.1.0。
    - **`mise run sdk-python` 改用 uv**：`tools.uv = "0.13.7"`（任务级，沿用原先 `tools.python` 的写法，不进 `[tools]`），
      `LO_SDK_PYTHON_VERSION`（默认 3.13）→ `uv python install --managed-python` → `uv venv --managed-python --python <版本>
      --clear .venv` → `uv pip install --python .venv/bin/python -e ".[test]"` → `.venv/bin/python -VV` → pytest。
      任务里没有 pip、没有 `python -m venv`、没有系统 Python（`--managed-python` 让 CI runner 自带的 3.12/hostedtoolcache
      不参与选择）。`scripts/ci-setup-mise.sh` 的 `sdk-python`、`sdk-journeys` 两个 profile 从 `python@3.13.5` 换成
      `uv@0.13.7`。`sdk-journeys` 依赖 `sdk-python`（默认 3.13）建 `sdk/python/.venv`，`sdk/python/e2e.sh` 与
      `scripts/sdk-journeys.sh` 不用改。
    - **uv 版本与发布年龄**：选 0.13.7（2026-11-03 发布，距今 16 天）。0.13.8 是 2026-11-12 17:03Z 发布，开工时（11-19 14:00Z）
      还差约 3 小时满 7 天，0.13.9 是 11-17，所以都没选。`scripts/check-pinned-releases.sh` 不覆盖 mise.toml 工具版本（脚本末尾
      「暂不检查」清单里写着），年龄是用 `gh api repos/astral-sh/uv/releases/tags/<版本>` 手工核对的，没有新增自动门禁。
      0.13.7 的 python 清单含 3.15.2、3.15.2 free-threaded 与 3.16.0a1。
    - **CI 矩阵**：`sdk-python` 作业 `strategy.matrix.include` 五格（3.9、3.13、3.15、3.15t 的 `experimental: false`，3.16 的
      `experimental: true`），`fail-fast: false`，`continue-on-error: ${{ matrix.experimental }}`，`LO_SDK_PYTHON_VERSION:
      ${{ matrix.python }}` 传给 `mise run sdk-python`；解释器由 uv 取，没有引入 `actions/setup-python`。作业名从
      `Python SDK (install, pytest)` 变成 `Python SDK (<版本>)`；`ci-ok` 是唯一候选 required check，它的 `needs` 名没变。
    - **`ci-ok` 与 `continue-on-error`**：`ci-ok` 只看 `needs.*.result`；GitHub 文档里作业级 `continue-on-error: true`
      的失败格不会让 workflow 失败、`needs` 结果算 success，所以 3.16 红了 `ci-ok` 不变红。**这一点只依据文档，本切片没推送，
      没有在真实 run 上验证**，PR 的 CI 里 3.16 若恰好红，可以顺便确认。`ci-ok` 的汇总逻辑未改。
    - **`scripts/check-github-workflows.sh` 新增矩阵断言**（原来只有 release workflow 对 `continue-on-error` 有断言）：`ci.yml` 里
      `continue-on-error:` 全文只许出现 1 次、必须是 `${{ matrix.experimental }}` 且位于 `sdk-python`；矩阵单元必须恰好是
      `3.9=false 3.13=false 3.15=false 3.15t=false 3.16=true`；`fail-fast: false`；传入 `LO_SDK_PYTHON_VERSION`。变异验证：
      3.15 改 `experimental: true`、再加一处 `continue-on-error`、删 `fail-fast`、把版本写死，各自让 `workflow-check` 变红。
      `scripts/ci-changes.sh` 的分组与 `ci-changes-test.sh` 不需要改（`.github/**`、`mise.toml`、`scripts/ci-*` 本来就是全量）。
    - **各格测试（本地 `LO_SDK_PYTHON_VERSION=<版本> mise run sdk-python` 实测）**：3.9.25、3.13.18、3.15.2（GIL）、3.16.0a1 都是
      `193 passed, 6 skipped`；3.15.2t 是 `196 passed, 3 skipped`。3.15t 上有 4 个真实采样测试从 skip 变成运行（`test_sampler.py` 的 3 个 + S3 的
      `test_real_315t_profiles_a_busy_thread…`），同时 3.15 GIL 构建的真实「返回不可用」测试在 3.15t 上跳过，所以净多 3 个通过；3.15t 跳过的 3 个是只在其它解释器上有意义的断言（pre-3.15、3.15 GIL 构建、
      `PYTHON_GIL=1`）。3.15 GIL 构建跳过的是 `needs Linux + free-threaded CPython 3.15+ without GIL` 的真实采样测试，同时真实
      3.15 GIL 测试断言返回不可用，符合「只验证打 warning、不采集」。
    - **文档**：`sdk/python/README.md` 与 `docs/sdk/python.md` 去掉「not implemented」的旧说法；README 加 `## Continuous profiling
      (experimental)`，`docs/sdk/python.md` 加 `## Continuous profiling`（锚点 `#continuous-profiling`，与 `_profiling.DOC_URL` 一致），
      含启用方式、支持范围、采样节奏、greenlet、实测开销、火焰图汇总，以及 `### Warning codes` 里 8 个原因码的逐条表。
      配置表补 `profiling`、`profiling_interval` 两行，parity 表的 pprof 一行同步。数字全部来自
      `research/python-cpu-profiling-gil.md`（开销、SQLAlchemy asyncio）、ADR 0014 与 S3 冒烟（`init` 额外 ≈ 9 ms），没有新测。
      用户文档里没有链接 `.agents/` 路径。开发说明（README 的 Development、`CONTRIBUTING.md` 的 SDK 任务行）改为 uv 与
      `LO_SDK_PYTHON_VERSION`。`docs/sdk/python.md` 的 Install 一节里面向使用者的 `pip install <路径>` 保留（那是使用者的安装方式，
      不是仓库开发环境）。
    - 协议面（端点、字段、DSN、鉴权）没有改动，`docs/version-matrix.md` 未改；SDK 运行时代码未改（B1、B2 同时在改）。
    - 没处理：3.16 预发布格的 `continue-on-error` 在真实 CI 上的行为未验证；uv 管理的解释器与 pytest 依赖（PyPI）在 CI 里每次
      重新下载，没有加缓存；Python 测试依赖本身没有最小发布年龄约束（`pip` 时代就没有，这次不改）。

## Green 后审查与重构

- [ ] S1 后：检查 `_pprof.py` 与 `_profile_upload.py` 是否只暴露必要符号；确认没有把
  `poc/` 代码或第三方依赖带进仓库；重跑 `mise run sdk-python` 与
  `go test ./internal/gateway -run PythonPprof`。
- [ ] S2 后：把采样器的可用性判断与窗口聚合拆开，重跑聚焦测试与 3.15 job。
- [x] S3 后：确认 `Client.shutdown` 的预算与 Go SDK 一致，重跑全量 Python 测试。结论见 S3 落地说明
  「与 Go SDK shutdown 的对照」：profiling 与 provider 共用 deadline，Go 无 5 s 门槛是唯一的行为差异，
  `error_transport` 仍拿完整 timeout 是既有行为。全量测试见执行日志。

## 最终验证

- [ ] `mise run sdk-python`（3.13 本地；CI 另覆盖 3.9/3.15/3.15t/3.16 预发布）。
- [ ] `mise run vet`、`mise run test`、`mise run fmt-check`、
  `GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint`（S1 改到 `internal/gateway`）。
- [x] `mise run workflow-check`、`mise run ci-changes-test`（S4 改 CI 后）。
- [ ] `git status --short` 只包含 S1–S4 计划内文件，`poc/` 不在其中。

## 未决事项

- 已定（2026-11 grilling）：窗口级 N = 3（窗口 = collect + encode）；tick 失败率阈值 50%（窗口内
  尝试次数不到 100 时不判断）；失败 tick 复用
  `record_telemetry_drop`、`Diagnostics` 不加字段；开放 `profiling_interval`；不检测 greenlet；
  不向 CPython 上游提交 issue 或草稿。
- 后续立项（不在本计划内）：sidecar 外部 attach（覆盖 GIL 构建）与墙钟剖析（新增 profile
  类型，涉及协议面）。
- 已知平台限制（只记录，本计划不处理）：火焰图查询读取窗口内全部 blob，没有数量上限；
  HA E2E 没有覆盖 profile。

## 执行日志

- 2026-11-18 S1 开工：基线 `mise run sdk-python` → 47 passed（exit 0）。
- 2026-11-19 S1.1 red：`sdk/python/.venv/bin/python -m pytest -q tests/test_pprof.py` →
  `ModuleNotFoundError: No module named 'lazyobserve._pprof'`（exit 2）。
- 2026-11-19 S1.1 green：实现 `_pprof.py` 后同一命令 → 7 passed（exit 0）。
- 2026-11-19 S1.2 red：`go test ./internal/gateway -run TestPythonPprofFixtureParses` →
  `open fixture: ... no such file or directory`（exit 1）；同一时刻 pytest 的防漂移断言
  `test_committed_fixture_matches_encoder` 失败（1 failed, 7 passed，exit 1）。
- 2026-11-19 S1.2 准备：`mise run ent-generate` → exit 0（`internal/db/ent` 未跟踪）。
- 2026-11-19 S1.2 green：
  `sdk/python/.venv/bin/python -m tests.generate_pprof_fixture` → 写出
  `internal/gateway/testdata/python_profiling/cpu.pprof.gz`（263 B，exit 0）；
  `go test ./internal/gateway -run TestPythonPprofFixtureParses` → `ok ... 0.007s`（exit 0）；
  `pytest -q tests/test_pprof.py` → 8 passed（exit 0）。
- 2026-11-19 S1.3 red：`pytest -q tests/test_profile_upload.py` →
  `ModuleNotFoundError: No module named 'lazyobserve._profile_upload'`（exit 2）。
- 2026-11-19 S1.3 green：实现 `_profile_upload.py` 后同一命令 → 7 passed（exit 0）。
- 2026-11-19 S1 全量：`mise run sdk-python` → 62 passed（exit 0）。
- 2026-11-19 Go 门禁：`mise run vet` → exit 0；`mise run test` → exit 0；
  `mise run fmt-check` → exit 0；
  `GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint` → `0 issues.`（exit 0）。
- 2026-11-19 S1 收尾：S1 四个复选框已勾选；S2–S4 未开工。
- 2026-11-19 审查 B1：`upload_id` 改为每窗口 `secrets.token_hex(16)`（128 bit，
  与 Go SDK 的 `newEventID` 一致），删除 nonce/序号/锁，修正测试、ADR 与 S1.3/S3 描述；
  4xx 改为经 `DeliveryQueue.record_drop("rejected")` 计数；pytest 防漂移改为比较解压后字节。
  重跑 `mise run sdk-python` → 63 passed（exit 0）；
  `go test ./internal/gateway -run PythonPprof -count=1 -v` → exit 0（fixture 未改动）。
- 2026-11-19 S2 实测（3.15.2 `-I` 探针脚本）：`sample` 签名
  `(pid, collector, *, duration_sec=None, all_threads=False, realtime_stats=False, mode=0,
  async_aware=None, native=False, gc=True, opcodes=False, blocking=False)`；
  `constants.PROFILING_MODE_CPU = 1`；采样间隔取自 `collector.sample_interval_usec`
  （10_000 → 100 Hz，实测 1 s 采到 100 个样本）；帧 = `(filename, LocationInfo, funcname, opcode)`，
  `funcname` 是 qualname；`ThreadInfo.thread_id` == `threading.get_native_id()`。
  GIL 构建上 `mode=cpu` 只采到采样线程自己，3.15t 无此现象（见
  `research/python-315-sampling.md` 的 S2 补充实测）。
- 2026-11-19 S2 red：`mise run sdk-python` →
  `ImportError: cannot import name '_sampler' from 'lazyobserve'`（1 error，exit 1）。
- 2026-11-19 S2 ADR 0014 修订追加 red：`~/.cache/lo-s2/py3.13/bin/python -m pytest -q tests/test_sampler.py -x` →
  收集期 `AttributeError: module 'lazyobserve._sampler' has no attribute 'REASON_PLATFORM'`。
- 2026-11-19 S2 门禁顺序变异（三道门挪到导入之后 / 试采之后）→
  `test_probe_refuses_before_importing_or_trial_sampling` 的 5 个参数分别在 `imported == []`、
  `fake.calls == []` 处失败；还原后通过。
- 2026-11-19 S2 审查修复（#94）：清零移到 `encode_window` 成功（原实现在 `collect_window` 成功时清零，
  编码器持续失败时永远凑不满 3 次）；`MIN_FAILURE_RATE_ATTEMPTS = 100`；
  `WINDOW_SECONDS = 10.0`/`WINDOW_INTERVAL_SECONDS = 60.0`。
- 2026-11-19 S2 对齐 ADR 0014 第二轮修订：rebase 到 `ae1475c`，plan 冲突以 main 为准；
  `FAILURE_RATE_THRESHOLD` 0.2 → 0.5；删 `backoff_seconds()`/`RETRY_BACKOFF_*`；新增
  `Sampler.last_failed_ticks()`；全部 tick 失败的窗口返回 `None`。补测试：51% 停用、49% 不停用、
  尝试数不足 100 时 100% 失败也不停用、collect 成功而 encode 失败不清零、三种停用路径之后不再调用
  `sample()`。阈值变异（0.4 / 0.6）分别让 4 / 5 个测试失败，还原后全绿。
- 2026-11-19 S2 对齐后 green（uv venv：`uv venv --python <ver>` + `uv pip install -e 'sdk/python[test]'`，
  `python -B -m pytest -q tests`；没有运行 `mise run sdk-python`，它内部用 `python -m venv` + pip）：
  3.9.25 / 3.13.18 / 3.15.2（GIL 构建）各 `114 passed, 5 skipped`，3.15.2t `116 passed, 3 skipped`；
  `PYTHON_GIL=1` 的 3.15.2t `tests/test_sampler.py` `51 passed, 5 skipped`；真实 3.15t 测试连跑 5 次通过。
- 2026-11-19 S3 开工：从 `origin/main`（`79689ca`）拉 `slice/python-profiling-3`；Go SDK 核对结论见 S3 落地说明。
  环境：`uv venv --python <ver> ~/.cache/lo-s3/py<ver>`（3.9、3.13、3.15、3.15t）+
  `VIRTUAL_ENV=… uv pip install -e 'sdk/python[test]'`，测试一律 `python -B -m pytest -q tests`；没有运行
  `mise run sdk-python`（内部用 `python -m venv` + pip）。
- 2026-11-19 S3 red（3.13.18）：`pytest -q tests/test_config.py` → 14 failed, 8 passed
  （`Config` 没有 `profiling`/`profiling_interval`）；`pytest -q tests/test_sampler.py` →
  4 failed, 51 passed, 5 skipped（`_AggregatingCollector.running`、`Sampler.stop()` 不存在）；
  `pytest -q tests/test_profiling_lifecycle.py` →
  `ImportError: cannot import name '_profiling' from 'lazyobserve'`（收集期，exit 2）。
- 2026-11-19 S3 green 后对 `_profiling.py` 做变异检查，每个变异都让对应测试变红，还原后全绿：
  子进程沿用继承来的锁 → 死锁测试 `child killed by signal 14`（SIGALRM 看门狗）；子进程不重启线程 →
  2 个 fork 测试失败；`upload_id` 提前到窗口开始时生成 → `test_upload_id_is_generated_when_the_window_is_encoded_not_before`
  失败；去掉 5 s 门槛 → 4.999 s 与 0 s 两个参数失败；去掉窗口前 `recheck_gil()` → GIL 重查测试失败。
- 2026-11-19 S3 全量 pytest（`python -B -m pytest -q tests`）：3.9.25 → `191 passed, 6 skipped`；
  3.13.18 → `191 passed, 6 skipped`；3.15.2（GIL 构建）→ `191 passed, 6 skipped`；
  3.15.2 free-threading → `194 passed, 3 skipped`（含真实 3.15t 的 `test_real_315t_profiles_a_busy_thread…`
  与 S2 的真实采样测试）；`PYTHON_GIL=1` 的 3.15.2t → `191 passed, 6 skipped`（真实采样测试按预期跳过）。
- 2026-11-19 S3 稳定性：`tests/test_profiling_lifecycle.py -k "not real_315t"` 在 3.13.18 与 3.15.2t 各连跑 25 次，
  0 次失败；真实 3.15t 测试连跑 5 次全部通过（各约 6.1 s）。
- 2026-11-19 S3 真实 3.15.2t 冒烟（`~/.cache/lo-s3/smoke.py`，不入库；本地 `ThreadingHTTPServer` 当平台，
  `lazyobserve.init(profiling=True, profiling_interval=1)`，2 个纯 Python 忙线程，运行 16 s 后 `shutdown(10)`）：
  `init` 立即返回（打开 profiling 的额外开销 ≈ 9 ms，探测在后台线程）；收到 2 次 `POST /api/profiles`：
  第 1 次是满 10 s 的窗口，第 2 次是 shutdown 时只有 5.8 s 的部分窗口（≥5 s 所以上传）；两次的
  `upload_id` 不同且为 32 位 hex，`type=cpu`、`Authorization: Bearer`、`service`、RFC3339 `start`/`end`
  齐全，解压后的 pprof 含 `burn_cpu_smoke`；`shutdown` 用时 0.010 s，之后没有残留的 `lazyobserve-profil*` 线程。
- 2026-11-19 S3 `mise run workflow-check` → exit 0（本切片未改 CI、`mise.toml`、协议面）。
- 2026-11-19 S3 审查后修整（折进同一 commit）：`Sampler.stop()` 与 `collect_window` 发布 collector 改用一把
  只护两三次属性访问的锁，不再依赖无 GIL 构建下的内存序；fork 钩子逐个 controller 兜异常，一个失败不跳过其余；
  真实 3.15t 测试的 6 s 从探测结束起算（慢 runner 上导入 `profiling.sampling` 变慢也不会让窗口不足 5 s）；
  新增 fake 采样器/上传器与真实 `Sampler`/`ProfileUploader` 的签名一致性测试（改 fake 参数名即红）；
  S4 交付补写 `#continuous-profiling` 锚点与逐码说明。全量 pytest：3.9.25 / 3.13.18 → `193 passed, 6 skipped`，
  3.15.2t → `196 passed, 3 skipped`。
- 2026-11-19 S4 开工：从 `origin/main`（`94c27fa`）拉 `slice/python-profiling-4`；`git status --short` 干净。uv 版本年龄核对：
  `gh api repos/astral-sh/uv/releases/tags/{0.13.7,0.13.8,0.13.9}` → 11-03 / 11-12 17:03Z / 11-17（开工时 14:00Z，0.13.8 还差约 3 小时），选 0.13.7。
- 2026-11-19 S4 `mise run sdk-python`（改 uv 后，任务里无 pip；`LO_SDK_PYTHON_VERSION` 逐版本）：3.9 → Python 3.9.25，`193 passed, 6 skipped`；
  3.13 → 3.13.18，`193 passed, 6 skipped`；3.15 → 3.15.2（GIL 构建），`193 passed, 6 skipped`；3.15t → 3.15.2 free-threading build，
  `196 passed, 3 skipped`；3.16 → 3.16.0a1，`193 passed, 6 skipped`（预发布格的本地预演）；均 exit 0。
- 2026-11-19 S4 `bash scripts/check-github-workflows.sh` 变异验证：3.15 改 `experimental: true`、再加一处 `continue-on-error`、删 `fail-fast`、
  版本写死 → 各自 exit 1 且信息指向对应断言；还原后 exit 0。
- 2026-11-19 S4 `mise run workflow-check` → exit 0；`mise run ci-changes-test` → exit 0。
- 2026-11-19 S4 `mise run sdk-python`（默认 3.13，重建 `.venv`）→ `193 passed, 6 skipped`；`mise run build && mise run storage-bins &&
  LO_SDK_JOURNEYS_ONLY=python mise run --skip-deps sdk-journeys` → exit 0，Python journey 全部 `ok`（metrics、metric-contract、logs、issue、
  trace、db-span、llm-span），`PASS: all SDK journeys (python)`，用的是 uv 建的 `sdk/python/.venv`。
- 2026-11-19 S4 交付审查：`sdk/python/README.md` 指向原因码说明的相对链接 `../../docs/sdk/python.md#continuous-profiling` 在 PyPI 上
  失效（README 是包的 long description），改为与 `_profiling.DOC_URL` 相同的绝对地址；`sdk/python/e2e.sh` 顶部注释仍写
  「Profiling is not implemented」，改为「journey 不打开持续剖析」。复跑 `LO_SDK_PYTHON_VERSION=3.13 mise run sdk-python` →
  `193 passed, 6 skipped`，`LO_SDK_PYTHON_VERSION=3.15t` → `196 passed, 3 skipped`；`mise run workflow-check`、`mise run ci-changes-test`、
  `bash scripts/check-e2e-contract.sh` → exit 0。
