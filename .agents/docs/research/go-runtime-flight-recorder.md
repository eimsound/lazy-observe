# Go Runtime Flight Recorder 技术调研

> 状态：技术调研，不是 ADR、已确认规格或实施计划。
>
> 本文只记录候选方案及其适用范围，不代表项目一定采用。是否进入产品需另行完成 PoC、开销测试、存储与
> UI 评估，并通过正式决策流程确认。

## 背景

当前 Go SDK 已通过 OpenTelemetry 显式 span 记录 HTTP、数据库和业务阶段耗时，并通过
`runtime/pprof` 周期性采集 CPU、heap 和 goroutine profile。需求基线同时要求 SDK 使用显式插桩，
不安装全局运行时 hook，并保持 middleware 每请求开销可预测。

仍有一类问题不容易只靠普通 trace 和 pprof 回答：某个请求偶发变慢、服务短时卡住或健康检查失败时，
当时是否发生了 goroutine 阻塞、调度延迟、系统调用等待或 GC 活动。Go 1.25 引入的
`runtime/trace.FlightRecorder` 是这一问题的候选诊断手段。

## 方案 1：使用 `runtime/trace.FlightRecorder`

Flight Recorder 在进程内持续保留最近一段 Go runtime execution trace。触发条件出现后，SDK 可以调用
`WriteTo` 导出包含触发前现场的快照，而不必等问题发生后才启动 trace。

候选流程如下：

```text
SDK 初始化
└─ 启动一个进程级 FlightRecorder
   └─ 内存中滚动保留最近一段 runtime trace

请求进入
└─ Fiber middleware 创建普通 OTel server span
   └─ 请求结束后判断是否满足慢请求阈值
      └─ 满足：触发 runtime trace 快照并关联 OTel trace ID
```

Flight Recorder 适合提供“请求发生时的调度、阻塞、GC 和 CPU 现场”，不应被描述为“单请求逐函数耗时”。
它记录的是进程级 runtime trace，会包含同一时间窗口内的其他请求和后台 goroutine，也不会自动为每个
普通 Go 函数产生 enter/exit 事件。

### 候选模块 seam

如果采用，建议把 runtime trace 的生命周期、触发去重、冷却、写出和上传封装在独立深模块中。Fiber
适配层只依赖一个小接口，不直接管理 `runtime/trace`：

```go
type RuntimeSnapshotter interface {
	Trigger(context.Context, RuntimeTraceTrigger) error
}

type RuntimeTraceTrigger struct {
	Reason          string
	TraceID         string
	Route           string
	RequestDuration time.Duration
}
```

该接口只是调研草案，不是已确认的公共 SDK 接口。实现内部至少需要负责：

- 一个进程最多只能运行一个 Flight Recorder；
- `MinAge`、`MaxBytes` 和进程内存开销控制；
- 并发触发合并、冷却时间和单位时间快照上限；
- 快照开始/结束时间及触发原因的元数据；
- 有界异步上传，上传失败不得阻塞应用请求；
- SDK shutdown 时停止 recorder 并排空已接受的上传任务。

### 触发策略建议

第一阶段只考虑以下触发源：

1. **慢请求**：middleware 在请求结束后发现耗时超过阈值，保存包含请求执行期的最近现场。
2. **卡死或超时**：由独立 watchdog 在请求仍未完成时触发；不能依赖请求结束后的 middleware。
3. **健康检查失败**：应用或 SDK 观察到连续失败后触发，并设置冷却时间避免快照风暴。

默认关闭，作为高级诊断功能显式开启。具体阈值、采样率、窗口长度和内存上限必须经过 PoC 与 benchmark
后再确定，不在本文中固化。

## 摄入与存储建议

如果采用，应使用独立摄入路径，例如：

```text
POST /api/runtime-traces
```

不要把 runtime trace 混入现有 `/api/profiles`：后者存储和处理的是 pprof payload，而 Flight Recorder
输出的是 Go execution trace，两者的数据格式、查询方式和 UI 都不同。

候选上传元数据包括：

- project、service、release 和 environment；
- 快照开始时间、结束时间和字节数；
- trigger reason；
- 触发请求的 OTel trace ID；
- 规范化 HTTP route 和请求耗时；
- Go version、GOOS 和 GOARCH。

原始 execution trace 可复用 blob 存储 seam，元数据使用独立索引。是否新增数据表、保留期和清理任务，
需要在方案进入实施前另行设计。

## UI 关联建议

普通 OTel trace 仍是请求分析的主入口。对于存在 runtime trace 快照的慢请求，UI 可以提供：

```text
慢请求 OTel trace
└─ 查看同一时间窗口的 Go runtime trace
```

runtime trace 页面应明确展示它覆盖的是进程级时间窗口，而不是只属于当前请求。首版若不能提供足够可靠的
Web 分析体验，可以先提供受控下载，通过 `go tool trace` 离线查看；这同样需要产品与安全评估。

## 与当前能力的关系

三种信号承担不同职责，不互相替代：

| 信号 | 回答的问题 | 当前状态 |
| --- | --- | --- |
| OTel trace | 单次请求经过哪些显式业务阶段，各阶段墙钟耗时多少 | 已实现，继续作为主路径 |
| pprof | 一个时间窗口内 CPU、内存和 goroutine 热点在哪里 | 已实现，继续作为 Profiling 主路径 |
| Flight Recorder | 异常发生前后 runtime 如何调度，阻塞、GC、系统调用发生了什么 | 候选调研，尚未决定采用 |

Go Execution Tracer Overhaul 是 Flight Recorder 所依赖的 Go runtime 基础设施，不是项目需要在方案 1
之外单独选择或实现的第二种 SDK 方案。项目升级到相应 Go 版本后已经获得该底层能力。

## 风险与未知项

- 繁忙进程的 trace 产生速率、常驻内存和 `WriteTo` 延迟需要实测。
- 进程级快照可能包含与触发请求无关的并发活动，关联结果只能作为诊断证据，不能当作严格归因。
- 需要验证与当前周期性 `pprof.StartCPUProfile` 同时运行时的开销和数据价值。
- 高并发慢请求可能造成快照和上传风暴，必须有进程级限流与冷却。
- execution trace 可能暴露函数名、文件路径和业务 region/log 内容，需要权限、保留期和下载审计。
- Web 端解析、展示和大文件处理的实现成本尚未评估。
- All in One、Standalone 和 HA 模式下的 blob 存储、索引及清理行为尚未设计。
- 需要验证 `CGO_ENABLED=0`、linux/amd64 和 linux/arm64 发布产物上的行为与开销。

## 进入实施前的验证门槛

只有完成以下工作后，才应考虑把方案转为规格或 ADR：

1. 在 `poc/go-flight-recorder/` 建立带 `FINDINGS.md` 的 PoC。
2. 测量关闭、仅运行 recorder、触发快照三种状态的吞吐、P50/P95/P99 延迟、CPU 和 RSS 差异。
3. 验证慢请求、锁竞争、GC pause、网络等待和 goroutine 泄漏是否能得到可行动的诊断信息。
4. 验证 Flight Recorder 与现有 pprof 周期采集并行时是否冲突或产生不可接受的开销。
5. 评估独立摄入、blob、元数据索引、保留期清理和 UI 的端到端成本。
6. 形成采用或拒绝的 ADR；在此之前不得把本调研文档当作产品承诺。

## 参考资料

- [Go `runtime/trace` package](https://pkg.go.dev/runtime/trace)
- [Go proposal #63185: runtime/trace flight recording](https://github.com/golang/go/issues/63185)
- [Execution tracer overhaul design #60773](https://github.com/golang/proposal/blob/master/design/60773-execution-tracer-overhaul.md)
- [Go Blog: Flight Recorder in Go 1.25](https://go.dev/blog/flight-recorder)
- [Go SDK 文档](../../../docs/sdk/go.md)（SDK 源码位于本仓库 `sdk/go`）
- [项目需求基线](../requirements.md)
