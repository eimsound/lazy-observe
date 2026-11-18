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
  - 失败停用规则：在连续 3 次失败之外，增加「窗口内失败率超过阈值」；失败率由采样器统计并暴露。
  - Red：用 fake 模块分别断言 GIL 构建、`_is_gil_enabled()` 为 True、非 Linux 时返回不可用原因；
    断言「间歇失败、凑不满连续 3 次」的序列会触发失败率停用。真实 3.15t 测试改成纯 Python 忙线程
    负载（不再依赖释放 GIL 的 hashlib）；真实 3.15 GIL 测试断言返回不可用。
  - `d30b98c` 的执行日志里写着「等待负责人决定默认 mode」，现已决定：保持 `mode=cpu`。
- **S3**：窗口改为每 60 s 采 10 s（见下文 S3 交付）；每个窗口开始前调用 S2 的 GIL 重查，
  发现 GIL 已启用就停用，只打一条 warning；失败率阈值在开工前定下（建议 20%）。
- **S4**：CI 中 3.15 GIL 构建 job 只验证「打 warning、不采样」，真实采样只在 3.15t job 里跑；
  用户文档写明支持范围、所有打 warning 不采样的情况、不支持 greenlet，以及实测开销。

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
    `mode`/`all_threads`/排除采样线程/连续失败自动停用；同一文件里「3.9–3.14 上特性探测
    不导入私有模块、不采集」的断言按 `sys.version_info` skipif 分派（落地时合并成一个文件，
    不再单独建 `test_sampler_unavailable.py`）。命令：`mise run sdk-python` 与 3.15 job 的
    同一 pytest 命令。
  - Green：实现最小采样器与特性探测。
  - 验证：`mise run sdk-python`；3.15/3.15t job 里跑 `pytest tests/test_sampler.py`。
  - 验收：3.15 上能产出非空聚合栈；3.14 及更早只返回不可用原因；采样线程不出现在样本里。
  - 落地偏差（实测，见执行日志）：
    - 采样间隔不在 `sample()` 参数里，`SampleProfiler` 从
      `collector.sample_interval_usec` 读取，所以 100 Hz 由自定义 collector 上的
      `sample_interval_usec = 10_000` 表达。
    - 帧元组的 `funcname` 实测就是 qualname（`Outer.method.<locals>.inner`），不需要额外
      取 `co_qualname`。
    - `ThreadInfo.thread_id` 是 OS 线程 id（`threading.get_native_id()`），不是
      `threading.get_ident()`，排除采样线程按 native id 做。
    - `SampleProfiler.sample` 会往 stdout 打统计行，采样器把
      `profiling.sampling.sample` 模块级 `print` 换成 no-op，避免污染应用 stdout。
    - GIL 构建上 `mode=cpu` 只能看到「此刻不在 Python 字节码里（已释放 GIL）且仍在 CPU 上」
      的线程：采样线程持 GIL 采样，其它纯 Python 线程一律被判为非 CPU。free-threaded
      3.15t 上无此限制。负责人已决定保持 `mode=cpu`（ADR 0014 修订：只在 Linux + 3.15t 且
      GIL 已关闭时采样，GIL 构建不采样），不改 wall/all。
    - 采样器是阻塞式窗口接口，不自己 sleep；退避以 `Sampler.backoff_seconds()` 暴露给
      S3 的窗口循环（S2 没有循环）。
  - ADR 0014 修订后的追加（已落地，见执行日志 2026-11-19）：
    - 可用性三道门在试采和私有模块导入之前：`sys.platform == "linux"`、
      `sysconfig.get_config_var("Py_GIL_DISABLED")` 为真、`sys._is_gil_enabled()` 为 False
      （属性不存在或调用抛错都按「GIL 已启用」处理）。原因码 `not-linux`、`gil-build`、
      `gil-enabled`；`<3.15` 仍先于它们判断。
    - `Sampler.recheck_gil()` 只重读 GIL 状态，供 S3 每个窗口开始前调用；GIL 已启用就把采样器
      永久置为停用（`reason()` 为 `gil-enabled`），不自行 warn，由 S3 打那一条 warning。
      `collect_window` 本身不重查。
    - 失败率停用：失败率 = `collect_failed_sample()` 次数 ÷（所有成功读取的尝试 +
      失败次数），成功尝试按 `len(timestamps_us)` 计，空栈和采样线程自身也算成功尝试。
      单个窗口的失败率 `> failure_rate_threshold`（`FAILURE_RATE_THRESHOLD = 0.2`，暂定，
      `Sampler` 的构造参数，不是用户配置）就丢弃该窗口、停用采样器并 warn；
      失败率经 `SamplingWindow.failure_rate` 与 `Sampler.last_failure_rate()` 暴露。
      尝试数低于 `MIN_FAILURE_RATE_ATTEMPTS = 100`（100 Hz 下约 1 s）的窗口不做失败率判定，
      照常返回、失败率照常暴露（审查补充：被截断的短窗口里一次失败就会超过 20%）。
      连续 3 次窗口级失败（`sample()` 抛错、编码失败）规则不变；一个窗口 = `collect_window` +
      `encode_window`，只有编码成功才清零连续失败计数（审查修复：原实现在 `collect_window` 成功时
      清零，编码器持续失败时永远凑不满 3 次、每个窗口都 warn）。
    - `WINDOW_SECONDS = 10.0`、`WINDOW_INTERVAL_SECONDS = 60.0`，与修订后的「每 60 s 采 10 s」
      对齐；循环本身仍归 S3。
    - `Sampler` 只供窗口循环单线程驱动，`recheck_gil`/`collect_window`/`encode_window` 不加锁。

### S3 公开 API 与生命周期

- [ ] 3. 开关、窗口与 shutdown
  - 交付：`Config.profiling: bool = False` 与 `normalize_config` 默认值；
    `build_client` 在开启时探测可用性，不可用（<3.15、GIL 构建、GIL 已启用、非 Linux）
    只打一条带原因的 warning；
    窗口循环与 Go SDK 一致：每 60 s 采 10 s（100 Hz、`mode=cpu`、`all_threads=True`），
    每个窗口开始前重查 GIL 状态，发现已启用就停用并 warning；每窗口
    `new_upload_id()` 后投递；`os.register_at_fork` 在子进程重启采样线程；
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
  - Red：`test_config.py` 断言 `profiling` 默认 False；`test_profiling_lifecycle.py`
    用 fake 采样器与 fake 上传器断言：不可用时只 warning 一次、每 60 s 一个 10 s 窗口、
    窗口前 GIL 重查为启用时停用并 warning、满 5 s 的窗口在 `shutdown` 内上传、不足 5 s 丢弃、
    fork 后子进程重新注册。
    命令：`mise run sdk-python`。
  - Green：接上开关与生命周期，只覆盖上述断言。
  - 验证：`mise run sdk-python`；3.15/3.15t job。
  - 验收：默认不采样；显式开启且满足启用条件（Linux + 3.15t + 无 GIL）时产生窗口；
    `shutdown` 不泄漏线程。

### S4 CI 与文档

- [ ] 4. CI 矩阵、README 与 CHANGELOG
  - 交付：`.github/workflows/ci.yml` 的 Python SDK job 矩阵：
    3.9、3.13、3.15、3.15t 阻塞，3.16 预发布非阻塞；3.9/3.13/3.15（GIL）只验证
    「开关打开打 warning、不采集」，真实采样只在 3.15t 上跑。`sdk/python/README.md` 与
    `docs/sdk/python.md` 标注 experimental 与启用方式，并写明：
    - 支持范围：Linux + CPython 3.15t，运行时无 GIL；
    - 打 warning、不采样的情况：3.9–3.14、3.15 GIL 构建、GIL 被 `PYTHON_GIL=1`/`-X gil=1` 或
      不兼容的 C 扩展重新启用、macOS/Windows；
    - 不支持 greenlet（gevent、eventlet 会自动停用；SQLAlchemy asyncio 等局部使用 greenlet 的库
      可能丢样本）；
    - 采样节奏（每 60 s 采 10 s）与实测开销。

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

## Green 后审查与重构

- [ ] S1 后：检查 `_pprof.py` 与 `_profile_upload.py` 是否只暴露必要符号；确认没有把
  `poc/` 代码或第三方依赖带进仓库；重跑 `mise run sdk-python` 与
  `go test ./internal/gateway -run PythonPprof`。
- [ ] S2 后：把采样器的可用性判断与窗口聚合拆开，重跑聚焦测试与 3.15 job。
- [ ] S3 后：确认 `Client.shutdown` 的预算与 Go SDK 一致，重跑全量 Python 测试。

## 最终验证

- [ ] `mise run sdk-python`（3.13 本地；CI 另覆盖 3.9/3.15/3.15t/3.16 预发布）。
- [ ] `mise run vet`、`mise run test`、`mise run fmt-check`、
  `GOLANGCI_LINT_CACHE=$(mktemp -d) mise run lint`（S1 改到 `internal/gateway`）。
- [ ] `mise run workflow-check`、`mise run ci-changes-test`（S4 改 CI 后）。
- [ ] `git status --short` 只包含 S1–S4 计划内文件，`poc/` 不在其中。

## 未决事项

- ~~S2 的「连续失败 N 次自动停用」中 N 的取值~~：已定，N = 3
  （`MAX_CONSECUTIVE_FAILURES`，负责人 2026-11 拍板）。
- S3 是否需要同时输出 profiling 自身的 diagnostics 计数（`dropped_telemetry`）：
  建议复用 `record_telemetry_drop`，S3 时确认。
- 失败率停用阈值：S2 已按建议值实现为 `FAILURE_RATE_THRESHOLD = 0.2`（`Sampler` 构造参数，
  不是用户配置），**待定**：负责人尚未最终确认，S3 开工前再定；改值只动这个常量与对应测试。
  另请一并确认「窗口内失败率」的口径：S2 按单个窗口内失败的采样尝试占比计，一个窗口超标就停用；
  不对「窗口级失败、成功、失败、成功…」这种跨窗口间歇序列单独设规则。审查建议（S2 已按此落地，
  负责人可推翻）：
  - 短窗口加最小尝试数保护，`MIN_FAILURE_RATE_ATTEMPTS = 100`；正常 10 s 窗口约 1000 次尝试，
    S3 只上传满 5 s 的窗口（约 500 次），保护只挡被截断的窗口和测试。
  - 跨窗口间歇失败暂只靠「连续 3 次」：3.15.2 里单次尝试的
    `RuntimeError`/`OSError`/`UnicodeDecodeError`/`MemoryError` 计入 `collect_failed_sample`，归失败率规则管；
    窗口级失败只剩建 unwinder 失败（`SampleProfiler.__init__` 转成 `SystemExit`，权限类，持续性的）、
    其它未预期异常和编码器 `TypeError`/`ValueError`，目前没有已知的间歇触发源；修好清零时机后，
    持续失败的编码器也能被「连续 3 次」停掉。代价是每次窗口级失败都 warn 一条。若 S3 的 diagnostics
    显示出现间歇的窗口级失败，再加「最近 M 个窗口失败 N 个」规则。
- 是否像 Go 的 `ProfilingInterval` 那样把采样间隔开放给用户配置：S3 开工前确认。
- greenlet 抓栈失败是否报给上游 CPython：由负责人决定，实现线程不自行提交。
- 后续立项（不在本计划内）：sidecar 外部 attach（覆盖 GIL 构建）与墙钟剖析（新增 profile
  类型，涉及协议面）。

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
  `constants.PROFILING_MODE_CPU = 1`；采样间隔取自
  `collector.sample_interval_usec`（10_000 → 100 Hz，实测 1 s 采到 100 个样本）；
  帧 = `(filename, LocationInfo, funcname, opcode)`，`funcname` 是 qualname；
  `ThreadInfo.thread_id` == `threading.get_native_id()`。
- 2026-11-19 S2 red：`mise run sdk-python` →
  `ImportError: cannot import name '_sampler' from 'lazyobserve'`（1 error，exit 1）。
- 2026-11-19 S2 green：实现 `_sampler.py` 后 `pytest -q tests` →
  3.13 `88 passed, 3 skipped`（exit 0）；3.14 `88 passed, 3 skipped`；
  3.15 `90 passed, 1 skipped`；3.15t `90 passed, 1 skipped`。
- 2026-11-19 S2 门禁：`mise run sdk-python` → `88 passed, 3 skipped`（exit 0）；
  `uv venv` + `uv pip install -e "sdk/python[test]"` 建 `/tmp/s2-py315`、`/tmp/s2-py315t`、
  `/tmp/s2-py314`，各自 `python -m pytest -q tests` 全绿（见上一条）。未跑 3.9：本机
  `uv python list` 只有 `<download available>`，未为此下载；改用
  `ast.parse(..., feature_version=(3, 9))` 校验 `_sampler.py` 与 `test_sampler.py` 的 3.9 语法。
- 2026-11-19 S2 稳定性：真实 3.15/3.15t 测试连跑 5 次 / 3 次均通过（采样窗口 2 s）。
- 2026-11-19 S2 发现：GIL 构建上 `mode=cpu` 只采到采样线程自己——同进程纯 Python 线程在
  采样瞬间都卡在 GIL 上，`status` 只有 `GIL_REQUESTED` 没有 `ON_CPU`，被
  `skip_non_matching_threads` 过滤掉；只有处于 GIL-releasing C 调用中的线程可见。
  3.15t（free-threaded）无此现象。已按决策 5 保留 `mode=cpu`；负责人已决定保持 `mode=cpu`
  （ADR 0014 修订：只在 Linux + 3.15t 且 GIL 已关闭时采样），不再等待默认 mode 的决定。
  这条记录当时的 hashlib 负载已在下面 2026-11-19 的追加里换成纯 Python 忙线程。
- 2026-11-19 S2 追加（ADR 0014 修订）开工：从 `d30b98c` 切 `slice/python-profiling-s2` 并
  rebase 到 `origin/main`（无冲突，得到 `b80a4e5`）；解释器与环境全部走 uv：
  `uv python install 3.9 3.13`（3.15、3.15t 已在 uv 里），`uv venv --python <ver>` +
  `uv pip install -e 'sdk/python[test]'` 建在 `~/.cache/lo-s2/py<ver>`，用各自的
  `python -m pytest -q tests` 跑（`mise run sdk-python` 内部是 `python -m venv` + pip，按约束没有运行，
  上面这组 uv 命令是它的等价替代）。
- 2026-11-19 S2 追加 red：`~/.cache/lo-s2/py3.13/bin/python -m pytest -q tests/test_sampler.py -x` →
  收集期 `AttributeError: module 'lazyobserve._sampler' has no attribute 'REASON_PLATFORM'`
  （1 error，exit 非 0）。
- 2026-11-19 S2 追加 green：实现三道门、`recheck_gil()`、失败率停用后，
  `pytest -q tests` → 3.9.25 `104 passed, 5 skipped`；3.13.18 `104 passed, 5 skipped`；
  3.15.2（GIL 构建）`104 passed, 5 skipped`；3.15.2t `106 passed, 3 skipped`，均 exit 0。
  真实 3.15t 的纯 Python 忙线程测试连跑 5 次均通过；`PYTHON_GIL=1` 下 3.15t 的
  「GIL 被重新启用」真实测试通过、采样类真实测试按预期 skip；真实 3.15 GIL 构建测试断言
  `probe_sampler()` 返回 `gil-build` 且 `collect_window` 返回 `None`。
- 2026-11-19 S2 审查：门禁顺序变异（把三道门挪到导入之后 / 试采之后，`-B` 无字节码缓存）→
  `test_probe_refuses_before_importing_or_trial_sampling` 5 个参数分别在 `imported == []`、
  `fake.calls == []` 处失败（各 6 failed），还原后 41 passed；真实 3.15t 连采 8 个 0.3 s 窗口，
  `/proc/self/fd` 4 → 4、线程数 1 → 1；16 个忙进程（8 核）超订下真实测试连跑 5 次均通过。
- 2026-11-19 S2 审查修复 red：新增「编码器持续失败跨窗口停用」「短窗口不判失败率」「最小尝试数起判」，
  默认值断言改为 `WINDOW_SECONDS == 10.0` → `pytest -q tests/test_sampler.py` 5 failed, 39 passed。
- 2026-11-19 S2 审查修复 green：清零移到 `encode_window` 成功、`MIN_FAILURE_RATE_ATTEMPTS = 100`、
  `WINDOW_SECONDS = 10.0`/`WINDOW_INTERVAL_SECONDS = 60.0` 后，`python -B -m pytest -q tests` →
  3.9.25 / 3.13.18 / 3.15.2 各 `107 passed, 5 skipped`，3.15.2t `109 passed, 3 skipped`；
  `PYTHON_GIL=1` 的 3.15t `tests/test_sampler.py` `44 passed, 5 skipped`；真实 3.15t 测试连跑 5 次通过。
