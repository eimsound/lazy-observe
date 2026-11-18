# Python 3.15 `profiling.sampling`（Tachyon）接入 `/api/profiles` 技术调研

> 状态：技术调研，不是 ADR、已确认规格或实施计划。
>
> 本文归档一次本地 spike 的实测结论，只说明候选路线是否可行、代价在哪里，不代表项目一定采用。
> 是否进入产品、走哪条路线，需另行通过正式决策流程确认。
>
> 后续：本 spike 只测了持续忙的负载。S2 审查发现，GIL 构建上的进程内采样看不见短 CPU 段，
> greenlet 下抓栈会失败。见 [`python-cpu-profiling-gil.md`](python-cpu-profiling-gil.md) 与
> ADR 0014 的 2026-11 修订。

## 背景

Python 3.15 标准库新增 `profiling.sampling`（Tachyon），可以在不修改目标代码的情况下按固定频率采样
Python 调用栈。lazy-observe 已有 pprof 摄入路径 `POST /api/profiles` 和火焰图查询
`GET /api/profiles/flamegraph`。本次 spike 要回答的问题是：Python SDK 能否用 Tachyon 采样，把结果转成
pprof，按现有契约上传，并在平台上得到正确的火焰图。

## 环境

- 日期：2026-11-18。
- Debian 13，Linux 6.12 amd64，`/proc/sys/kernel/yama/ptrace_scope = 0`（宿主机值，未修改），无 sudo。
- 解释器用 uv 安装：`cpython-3.15.2-linux-x86_64-gnu`、`cpython-3.15.2+freethreaded`、`cpython-3.14.10`。
  下文 `PY=~/.local/share/uv/python/cpython-3.15.2-linux-x86_64-gnu/bin/python3.15`。
- 平台用当时的 `main`（`197e273`）执行 `mise run build-binary && mise run storage-bins` 构建。

## 结论

技术上可以接入。三条候选路线都能产出被平台接受的 pprof，并得到比例正确的火焰图：

| 路线 | 做法 | 需要 ptrace 权限 | 备注 |
| --- | --- | --- | --- |
| (a) 进程内自采样 | SDK 起后台线程，用 `profiling.sampling.sample.sample` 对本进程 PID 采样，自定义 `Collector` 直接聚合成 pprof | 否 | 依赖文档未写的 Python API |
| (b) sidecar | 子进程 attach 到父进程 PID，采样后转 pprof | 是 | 开销落在另一个核上 |
| (c) CLI collapsed | `python -m profiling.sampling attach --collapsed`，再把 collapsed 文本转成 pprof | 是 | 会丢完整文件路径 |

三条路线都不需要改协议面：上传字段与 Go SDK 相同，走 `/api/profiles` 现有契约。

路线 (a) 的主要代价是依赖 3.15 文档里**没有写**的 Python API（`profiling.sampling.sample.sample` 和
`Collector`）。在本机 3.15.2 上，`inspect.signature(profiling.sampling.sample.sample)` 的结果是：

```text
(pid, collector, *, duration_sec=None, all_threads=False, realtime_stats=False, mode=0,
 async_aware=None, native=False, gc=True, opcodes=False, blocking=False)
```

`mode` 对应 CLI 的 `--mode {wall,cpu,gil,exception}`。`profiling.sampling.constants` 里
`PROFILING_MODE_WALL = 0`，所以默认 `mode=0` 就是 wall 模式：线程无论是否在 CPU 上都会被计入样本。
`all_threads=False` 时只采主线程（CLI 对应 `-a/--all-threads`）。spike 的路线 (a) 用的就是这组默认值，
即 wall 模式、只采主线程。如果走这条路线，接口需要按 CPython 小版本锁定，并保留降级路径。

## 实测（命令 → 结果）

1. `$PY -I target.py --mode inproc --seconds 10 --rate 100`
   结果：`samples=1000 failed=0 pprof_bytes=427`。进程内对自身 PID 采样能用。
2. `$PY -I target.py --mode sidecar --seconds 10 --rate 100`
   结果：`sidecar_samples=1000 failed=0`，`sidecar_exit=0`。
3. `$PY -I -m profiling.sampling attach --collapsed -o x.collapsed -d 5 -r 100 <pid>`，再把输出转成 pprof。
   结果：`stacks=4 samples=500 bytes=301`。collapsed 的帧格式是 `basename:func:line`，完整路径会丢失。
4. 用 `prctl(PR_SET_DUMPABLE, 0)` 模拟其他进程对本进程的 ptrace 被拒：
   - inproc：`samples=301 failed=0`，**仍然能采**；
   - sidecar：`PermissionError(13, 'Permission denied')`，退出码 3。
5. 目标进程带 `PYTHON_DISABLE_REMOTE_DEBUG=1`：inproc 和 sidecar 都能采。这个开关只关掉 `sys.remote_exec`
   的注入能力，挡不住只读采样。
6. 吞吐对比（示例负载，单位是请求/秒，3 次取平均）：

   | rate | none | inproc | sidecar |
   | --- | --- | --- | --- |
   | 100 Hz | 507.4 | 495.1（−2.4%） | 527.1（约 0） |
   | 1000 Hz | 524.3 | 509.2（−2.9%） | 526.0（约 0） |

   单次运行的噪声在 ±5% 左右；第一次 1000 Hz 单跑 inproc 测到过 −7%。sidecar 的开销落在另一个核上，
   外部采样器自身的 CPU 用量（getrusage）如下：100 Hz 跑 5 s 用了 0.12 s（约 2.4% 核），
   1000 Hz 跑 5 s 用了 0.41 s（约 8% 核），RSS 约 31 MB。负载的栈只有约 5 层，栈更深时开销会更高。
7. free-threaded 3.15.2t：inproc `samples=300 failed=0`，sidecar `samples=301`。
   用 free-threaded 解释器 attach 到带 GIL 的进程会被拒：
   `Cannot attach from a free-threaded Python process to a process running a non-free-threaded version`。
8. 3.14.10：没有 `profiling` 包；私有的 `_remote_debugging.RemoteUnwinder(os.getpid()).get_stack_trace()`
   能用，但它是私有模块，接口和 3.15 不同。
9. 平台端到端（`bin/lazy-observe server --data-dir /tmp/spike-lo-XXXX --listen 127.0.0.1:3199
   --component-port-offset 700 --storage-bin-dir .cache/storage-bin`）：
   - 只用 `--data-dir` 启动时失败：`read embedded storage manifest ... (use --storage-bin-dir or mise run dist)`。
     `build-binary` 产物不嵌存储二进制，需要显式给 `--storage-bin-dir`。
   - `POST /api/profiles`（project token）：inproc 返回 201；同一个 upload_id 用同样内容再传一次返回 200；
     同一个 upload_id 换成不同内容返回 409 `profile upload_id conflicts with accepted content`；
     sidecar 和 cli 都返回 201。
   - project token 读 `/api/profiles/services` 返回 403 `ingest_only_token`。改用 admin 登录（200），
     改密码（204），之后 `/api/profiles/services` 返回 `["py315-cli","py315-inproc","py315-sidecar"]`。
   - `GET /api/profiles/flamegraph?service=py315-inproc&type=cpu` 返回的树是
     `<module> → main → handle_request → hot_parse 7.26s / hot_render 1.28s`，比例与负载
     （`hot_parse` 约 75%、`hot_render` 约 25%）一致，sidecar 和 cli 的结果也一致。
   - Web UI 截图**没有做**：`build-binary` 产物不嵌前端，`/` 和 `/profiling` 都返回 404。前端火焰图的数据来源
     就是上面验证过的 `/api/profiles/flamegraph`。

## PoC 脚本

PoC 脚本只保存在本地分支 `spike/python-315-sampling`（提交 `a96fbb4`），不进仓库。按仓库约定，PoC 代码放在
本地 `poc/<topic>/` 下且已被 git 忽略，需要保留的结论写进本文。脚本清单如下，仅作说明：

| 脚本 | 作用 |
| --- | --- |
| `target.py` | 示例负载：`hot_parse` 约占 75%，`hot_render` 约占 25%，另有一个 sleep 线程。`--mode none/inproc/sidecar/wait`，`--nodump` 用来模拟 ptrace 被拒 |
| `pprof_collector.py` | 自定义 `profiling.sampling.Collector`，把叶子在前的帧聚合成 pprof 栈 |
| `pprof_encode.py` | 零依赖的 pprof protobuf 编码器，sample types 为 `samples/count` 和 `cpu/nanoseconds`，输出 gzip |
| `sidecar.py` | 路线 (b)：子进程 attach 到父进程 PID |
| `collapsed_to_pprof.py` | 路线 (c)：把 CLI 的 `--collapsed` 输出转成 pprof |
| `upload.py` | 只用标准库的 multipart 上传，字段与 Go SDK 相同 |
| `bench.sh` | 各模式吞吐对比 |

## 已知问题

- inproc 的火焰图里有 `Thread.join` 占 14.6%。PoC 用的是 wall 模式、只采主线程，而主线程在结束时阻塞在
  `join` 上等采样线程，wall 模式照样把这段等待计入样本。换成 cpu 模式，或者不让主线程 `join` 采样线程，
  预计这部分会消失（未实测）。
- 样本类型标成 `cpu/nanoseconds`，但 wall 模式下样本包含不在 CPU 上的时间，严格说是墙钟时间。真正接入时
  需要让 sample type 与 `mode` 一致。
- collapsed 路径会丢掉完整文件路径；直接用 Collector 路径能保留。
- 函数名只用了 `funcname`（3.15 下就是 qualname）。没有给 pprof 的 `mapping` 填值，这对服务端合并和渲染没有影响。
- 旧版本平台把 bootstrap admin 密码打在日志里、不写 `<data_dir>/initial-admin-password` 文件；
  这一行为已在 PR #12（`4ccb3b6`）修复，与本 spike 的结论无关。

## S2 实施期补充实测（2026-11-19，CPU 模式与 GIL）

计划 S2 落地采样器时补测了这一轮 spike 没做的组合（`mode=cpu` + `all_threads=True`），
结论会直接影响默认 mode 的选择，故记在这里。环境同上（3.15.2 与 3.15.2t，`-I`）。

| 组合 | 纯 Python 忙线程 | GIL-releasing C 调用中的线程 | 采样线程自己 |
| --- | --- | --- | --- |
| 3.15.2（GIL）`mode=cpu` | 不出现在样本里 | 出现 | 出现（需自己排除） |
| 3.15.2（GIL）`mode=wall`/`all` | 出现 | 出现 | 出现（需自己排除） |
| 3.15.2t（free-threaded）`mode=cpu` | 出现 | 出现 | 出现（需自己排除） |

原因：进程内采样线程在调用 `sample()` 期间持有 GIL，`_remote_debugging` 在这一刻读线程状态，
其它 Python 线程的 `status` 只有 `THREAD_STATUS_GIL_REQUESTED`、没有 `THREAD_STATUS_ON_CPU`，
于是被 `skip_non_matching_threads` 过滤掉；只有已经释放 GIL 的线程（C 扩展、IO）仍算「在 CPU 上」。
`blocking=True` 不改变这一行为。`run`/`attach` 两条 CLI 路线不受影响：它们的采样器在另一个进程里。

其它实测补充：

- 采样间隔不在 `sample()` 参数里，`SampleProfiler` 从 `collector.sample_interval_usec` 读，
  10_000 us 即 100 Hz（1 s 实测 100 个样本）。
- 帧元组是 `(filename, LocationInfo, funcname, opcode)`，`funcname` 实测就是 qualname
  （`Outer.method.<locals>.inner` 能区分同名方法）。
- `ThreadInfo.thread_id` 是 OS 线程 id，等于 `threading.get_native_id()`，不是
  `threading.get_ident()`。
- `SampleProfiler.sample` 每次结束后用 `print` 往 stdout 打三行统计（外加漏采警告）；SDK 采样器
  把 `profiling.sampling.sample` 模块的 `print` 换成 no-op 来消掉，不去动 `sys.stdout`。

## 风险与未知项

- 路线 (a) 依赖未文档化的 API，CPython 小版本升级可能改签名或语义。
- 3.14 及更早版本没有公开入口，只能走私有模块或不支持。
- free-threaded 与带 GIL 的解释器之间不能互相 attach，sidecar 路线必须用同一构建。
- 开销只在约 5 层栈的示例负载上测过，真实应用的深栈和多线程场景需要另测。
- `all_threads=True` 与各 `mode` 组合下的开销和数据形状没有测。

## 参考资料

- [Python 3.15 `profiling.sampling` 文档](https://docs.python.org/3.15/library/profiling.sampling.html)
- [Python SDK 文档](../../../docs/sdk/python.md)（SDK 源码位于本仓库 `sdk/python`）
- [Go Runtime Flight Recorder 技术调研](go-runtime-flight-recorder.md)（同类调研的先例）
- [项目需求基线](../requirements.md)
