# Python SDK 用 CPython 3.15 采样接口做进程内 CPU 剖析

Python SDK 一直没有剖析信号：`sdk/python/README.md` 之前直接写「CPU/pprof profiling is not
implemented in Python」。CPython 3.15 新增 `profiling.sampling`（Tachyon），调研
[`python-315-sampling.md`](../research/python-315-sampling.md) 实测确认进程内对自身 PID
采样能产出平台可接受的 pprof，且不需要 ptrace 权限，也不改协议面。我们决定：

- **只在 CPython ≥ 3.15 启用**。3.9–3.14 上用户打开开关只打一条 warning，不采样，也不走
  3.14 的私有模块 `_remote_debugging`。SDK 整体兼容范围仍是 `>=3.9`。
- **只做进程内、对自身 PID 的自采样**。sidecar（attach 其他 PID）和 CLI collapsed 两条路线
  不进 SDK：前者要 ptrace 权限、开销在另一个进程，后者会丢完整文件路径。
- **依赖未文档化接口**。采样线程调用
  `profiling.sampling.sample.sample(pid, collector, *, all_threads=True, mode=cpu)`，
  自定义 `profiling.sampling.Collector` 聚合栈。这两个符号不在 3.15 文档的公开 API 里，
  签名或语义可能随 CPython 小版本变化，因此启动时做特性探测（检查签名并试采），
  运行中连续采集失败就自动停用并 warning，而不是维护版本白名单。
- **只采 CPU**：`mode=cpu`、`all_threads=True`，上传 `type=cpu`，pprof sample type 沿用
  Go SDK 的 `samples/count` + `cpu/nanoseconds`。wall 模式的墙钟时间与 `cpu` 语义不一致，
  墙钟类型以后单独立项。
- **默认关闭**：用户显式设置 `profiling=True` 才启用；Python SDK README 标 experimental。
- **prefork 每个 worker 各自采样**：用 `os.register_at_fork` 在子进程重启采样线程；
  `upload_id` 每个窗口取 128 bit 随机数（`secrets.token_hex(16)`，与 Go SDK 的
  `newEventID` 一致），`os.urandom` 本身 fork 安全，不需要重新播种；labels 不带 pid。
- **100 Hz、60 s 窗口**，投递复用 `sdk/python/lazyobserve/delivery.py` 的 `DeliveryQueue`。
- **零依赖自写 pprof 编码器**：只用标准库把聚合栈编码成 gzip pprof；用 Go 侧
  `github.com/google/pprof/profile`（`internal/gateway` 已依赖）回读测试，不引入新依赖。
- **不改协议面**：仍走 `POST /api/profiles`，字段与 Go SDK 相同，`type=cpu`；
  `docs/version-matrix.md` 不需要改。

## 备选方案

- **sidecar 子进程采样（拒绝）**：需要 ptrace 权限，且 free-threaded 与带 GIL 的解释器之间
  不能互相 attach；SDK 里多一个子进程的生命周期比自采样更难保证。
- **CLI collapsed 转换（拒绝）**：丢完整文件路径，还要解析别人的文本格式，契约更脆。
- **3.14 及更早用 `_remote_debugging`（拒绝）**：私有模块、接口与 3.15 不同，维护两条实现
  的成本高于覆盖收益。
- **用墙钟模式顺带当 CPU（拒绝）**：wall 样本包含不在 CPU 上的等待时间，火焰图会把
  `Thread.join` 之类的阻塞算成热点；sample type 必须与 mode 一致。
- **版本白名单（拒绝）**：未文档化接口的变更不会跟着版本号走，白名单需要持续维护；
  特性探测能直接回答「这台解释器能不能采」。
- **引入第三方 pprof 编码库（拒绝）**：为 profile.proto 的一个子集新增运行时依赖，还要过
  许可证与依赖门禁，收益不抵成本。

## 后果

- Python SDK 多一条只在 3.15+ 生效的剖析路径；3.9–3.14 的行为不变，只是开关打开时多一条
  warning。
- CPython 升级可能打破采样：失效时 SDK 自动停用并 warning，不抛错、不中断业务；CI 固定跑
  最新 3.15.x，另有一个非阻塞的 3.16 预发布 job 提前暴露签名变化。
- pprof 编码器成为 SDK 的内部契约，由 Go `pprof` 解析器回读测试钉住；Python 侧只做结构断言，
  避免自己解码自己。
- 采样开销落在应用进程内（调研实测示例负载约 −2% 到 −3% 吞吐），需要按 100 Hz 的默认值使用。
