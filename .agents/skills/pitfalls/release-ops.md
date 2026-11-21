# 部署、CI 与发布链路防坑参考（`deploy/`、`.github/`、`Dockerfile*`、`scripts/`）

适用于容器构建、发布工作流与脚本、性能门禁及依赖边界相关改动与审查。

> 已毕业项：Helm PSS/PDB、不可变 digest 与 BuildKit 最小权限、`ci-changes.sh` 兜底、裸 `mise exec --` 及许可证白名单已分别由 `helm-template-test`、`workflow-check`、`ci-changes-test` 及各语言许可证检查（`license-check` / `check-js-licenses.sh` / `check-python-licenses.sh`）静态拦截，不再重复收录。

## 1. 容器归档与多包发布链路（`#179`、`#180`、`#301`）

- BuildKit `type=oci` 归档不能在经典 `overlay2` Docker daemon 直接 `docker load`，需先封装为 Docker save 归档（如 `scripts/merge-oci-layouts.sh`）再验活（`#301`）。
- 多包发版脚本按构建依赖顺序预构建前置包（如 `sdk-browser` 发版前先完成 `bundler-plugin` 预构建），避免发布阶段因缺失产物必败。
- 独立 SDK 发布工作流（如 `sdk-release.yml`）出包前执行对应语言完整测试套件（含 Python `pytest`），防止未验证代码出包（`origin/main` 祖先等静态约束由 `workflow-check` 守护）。

## 2. 性能门禁断言口径（`#179`、`#180`）

- 性能基准与门禁脚本（如 `ingest-bench`）综合断言吞吐、错误计数（`RequestsFailed`）与尾延迟（`P99LatencyMS`），确保高失败率或尾延迟恶化不会被吞吐指标掩盖。

## 3. 最低依赖版本、跨平台与依赖边界隔离（`#180`、`#186`、`#190`）

- 各语言清单（`pyproject.toml`、`package.json`、`go.mod`）声明的最低依赖版本与代码实际调用的 API（如 OTel `explicit_bucket_boundaries_advisory`、`LogRecord` 构造参数）保持一致，并在 CI 中覆盖下界版本与 Windows 导入等跨平台校验。
- 严格隔离 SDK 运行时与测试/开发依赖边界：避免把仅测试需要的重依赖（如 `sdk/go` 中的 `modernc.org/sqlite`）留在发布的 `go.mod` 中污染下游依赖树；Go SDK 构造 OTel Resource 使用 `resource.NewSchemaless` 规避 MVS 下的 `ErrSchemaURLConflict`。
