# 部署、CI 与发布链路防坑参考（`deploy/`、`.github/`、`Dockerfile*`、`scripts/`）

适用于 Helm Chart、容器镜像构建、GitHub Actions、发布脚本、性能门禁及依赖边界相关改动与审查。

## 1. Helm Chart 安全上下文与高可用模板（`#179`）

- 为工作负载显式声明符合 Kubernetes Pod Security Standards `restricted` 规范的 `securityContext`（非 root、禁用提权、drop `ALL` capabilities、启用 `seccompProfile`），并为持久化卷设置与镜像 UID 匹配的 `fsGroup`（如 distroless nonroot `65532`），防止 `allinone` PVC 挂载后出现 `EACCES`。
- 仅在多副本场景（`replicas > 1`）渲染 `PodDisruptionBudget`，或确保 `minAvailable < replicas`，避免单副本部署渲染 `minAvailable: 1` 锁死节点维护与 drain。

## 2. Dockerfile、镜像供应链与发布流水线（`#179`、`#180`）

- 镜像漏洞扫描与最终推送绑定同一不可变 digest，避免扫描镜像与发布镜像脱节；将镜像构建与仓库推送拆分为最小权限步骤，不向 BuildKit 构建阶段授予 `packages: write`。
- 多包发版脚本按构建依赖顺序预构建前置包（如 `sdk-browser` 发版前先完成 `bundler-plugin` 预构建），避免发布阶段因缺失产物必败。
- 独立 SDK 发布工作流（如 `sdk-release.yml`）在出包前校验目标提交位于 `origin/main` 并执行对应语言的完整测试套件（含 Python `pytest`），防止未验证或分叉提交发布。

## 3. CI 变更分组与性能门禁脚本契约（`#179`、`#180`）

- 调整 `scripts/ci-changes.sh` 路径映射时，确保安全与跨层门禁不被意外跳过（如 7 天发布年龄防线、元数据路径对应的 `meta-pg`、`sdk/go` 关联检查），维持「未分类路径或取不到 base 一律全量」的 fail-closed 兜底。
- 性能基准与门禁脚本（如 `ingest-bench`）综合断言吞吐、错误计数（`RequestsFailed`）与尾延迟（`P99LatencyMS`），确保高失败率或尾延迟恶化不会被吞吐指标掩盖。

## 4. 最低依赖版本、跨平台与依赖边界隔离（`#180`、`#186`、`#190`）

- 各语言清单（`pyproject.toml`、`package.json`、`go.mod`）声明的最低依赖版本与代码实际调用的 API（如 OTel `explicit_bucket_boundaries_advisory`、`LogRecord` 构造参数）保持一致，并在 CI 中覆盖下界版本与 Windows 导入等跨平台校验。
- 严格隔离 SDK 运行时与测试/开发依赖边界：避免把仅测试需要的重依赖（如 `sdk/go` 中的 `modernc.org/sqlite`）留在发布的 `go.mod` 中污染下游依赖树；Go SDK 构造 OTel Resource 使用 `resource.NewSchemaless` 规避 MVS 下的 `ErrSchemaURLConflict`；引入任何新依赖先对照 `AGENTS.md` 许可证白名单。
