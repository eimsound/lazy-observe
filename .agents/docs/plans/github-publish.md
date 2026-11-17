# 迁到 GitHub 公开发布（主开发仓）

## 目标

`github.com/eimsound/lazy-observe` 成为主开发仓：MIT 许可、单个初始提交、无内部基础设施痕迹；
SDK 并入 `sdk/`；CI 跑在 GitHub 托管 runner 上，按改动面增量执行，纯文档改动不跑业务 job。

完成条件：本仓库本地门禁全绿（见「验收」），`gitleaks` 与内部痕迹扫描为零命中，
历史压成单个提交；创建 GitHub 仓库与推送由负责人确认后执行。

## 已定决策（2026-11-17）

| 事项 | 决定 |
|---|---|
| 许可证 | MIT，版权人 ClanEver |
| 托管 | GitHub 主开发；Gitea 两个私有仓库只读归档，不删除 |
| 历史 | 重置为单个初始提交；`m1`–`m5` 放弃 |
| SDK | 并入 `sdk/{go,node,browser,python}`；Go SDK 为嵌套 module `github.com/eimsound/lazy-observe/sdk/go`，主模块 `replace => ./sdk/go` |
| CI runner | GitHub 托管（`ubuntu-24.04`、`ubuntu-24.04-arm`）；公开仓不挂自建 runner |
| 文档取舍 | 公开 ADR、需求基线、架构/部署/版本矩阵、技术调研；竞品分析、已完成的计划与规格、`poc/`、`opencode.json` 留在 Gitea 归档 |

## 备选路线

- **SDK 继续独立仓**：保留跨仓 `replace`、部署密钥与 SDK 对齐门禁；协议改动仍需两仓同步。否决：全新 clone 无法构建，发布链路多一道私有依赖。
- **workflow 级 `paths:` 过滤**：实现最简单，但被跳过的 workflow 不上报状态，required check 会卡住。否决，改用 `changes` 作业 + `ci-ok` 汇总。

## 切片

1. 快照导入：主仓 `git archive HEAD` + SDK `git archive HEAD`，删除不公开内容，Go SDK 改 module 路径。
   - green：`CGO_ENABLED=0 go build ./...`（主模块与 `sdk/go`）、`go mod tidy -diff`。
2. CI 迁 GitHub Actions：`changes` 作业、`ci-ok` 汇总、SDK 各语言 job、workflow 校验任务；job 直接跑在 runner 上，不用预构建 CI 镜像。
   - green：`mise run workflow-check`、`mise run ci-changes-test`、`mise run e2e-contract-check`。
3. 发布链路：GoReleaser 发 GitHub Release、镜像 ghcr、安装脚本、Helm 默认镜像、MIT `LICENSE`、第三方与 Victoria 声明、删除私有 SDK 拉取链路。
   - green：`mise run release-prerequisites-test`、`mise run release-publish-check`、`mise run install-check`、`mise run helm-lint`、`mise run helm-template-test`。
4. 文档与 agent 流程：README/CONTRIBUTING/CHANGELOG/AGENTS/`do`/issue tracker 改为 GitHub 与 `gh`；ADR 0012 记录本次决策。
   - green：内部痕迹扫描零命中（见下）。
5. 收尾：全量本地门禁、`gitleaks`、压成单个提交。

## 改动面分组（CI `changes` 作业）

| 改动路径 | 触发 |
|---|---|
| `*.md`、`docs/**`、`LICENSE`、`.agents/**` | 不跑业务 job |
| `sdk/node/**`、`sdk/browser/**`、`sdk/python/**` | 对应语言 SDK job |
| `sdk/go/**` | Go SDK job + 服务端 Go 检查、构建、e2e（服务端经 replace 编入 sdk/go） |
| `web/**` | web job |
| `internal/db/**`、`internal/meta/**` | + meta-pg |
| `cmd/**`、`internal/**` | Go 检查、构建、e2e、restart |
| `go.mod`/`go.sum` | 同上 + meta-pg |
| 打包链路 | artifact smoke + arm64 |
| `.github/**`、`mise.toml`、`scripts/ci-*`、`Dockerfile*` | 全量 |

## 验收

```bash
mise run vet && mise run test && mise run fmt-check && mise run lint
mise run license-check && mise run mod-tidy-check
mise run e2e-contract-check && mise run workflow-check
mise run install-check && mise run helm-lint && mise run helm-template-test
mise run release-prerequisites-test && mise run release-publish-check
gitleaks dir .
# 另跑一次内部痕迹扫描（旧域名、自建 runner 标签、私有 SDK 密钥名、内网网段）；模式含内部信息，不入库
mise run workflow-check   # 已内置拒绝 Gitea 上下文、代理与私有 SDK 拉取
```

## 执行日志
- 2026-11-17 快照导入后 `CGO_ENABLED=0 go build ./...`（主模块、sdk/go）、`go mod tidy -diff` → 通过
- 2026-11-17 三个并行分支（CI、发布、文档）完成；独立审查发现 3 个高危（声明文件排序依赖 locale、release 容器冒烟跑在宿主机、CI 镜像首推缺失）与若干中危，除首推镜像外均已修复
- 2026-11-17 `mise run vet/test/fmt-check/lint/alert-lease-clock-check/license-check/mod-tidy-check/migration-check/ent-generate-check/e2e-contract-check/workflow-check/ci-changes-test/install-check/helm-lint/helm-template-test/release-prerequisites-test/release-snapshot-check/sdk-go/sdk-node/sdk-browser/sdk-python` → 全部通过；`license-check` 在 C.UTF-8 下复跑 → 通过
- 2026-11-17 `mise run e2e` → 通过（401s）
- 2026-11-17 `mise run web` → lint、100 个单测、build 通过；`playwright install --with-deps` 需 sudo 失败，改为不带 `--with-deps` 手动安装后 `pnpm test:e2e` 20 passed，`check-js-licenses.sh web` 通过
- 2026-11-17 `gitleaks dir .` → no leaks found（`cmd/perf-gate/scale_test.go` 的测试夹具加 `gitleaks:allow`）
- 2026-11-17 `release-publish-check` → 本地无 `GITHUB_TOKEN`，按预期失败；本地 `docker buildx build` 拉 Docker Hub 超时，镜像构建留给 GitHub 首跑验证
- 2026-11-17 既有 flaky：`TestProfileAPIEndToEndAndProjectIsolation` 原仓 20 次失败 1 次、新仓 20 次 0 次，非迁移引入，另开 issue
- 2026-11-18 去掉预构建 CI 镜像：删除 `Dockerfile.ci`、`ci-image.yml`、`ci-net.sh`，13 个 job 去掉 `container:` 改跑 runner，mise 目录按 job 缓存；`check-container-smoke.sh` 改为发布 127.0.0.1 随机端口，`e2e-multi-replica.sh` 改为主机经容器 IP 访问依赖
- 2026-11-18 本地用假镜像跑 `check-container-smoke.sh` → PASS；主机访问 bridge 网络容器 IP → 通；`mise run workflow-check`、`ci-changes-test`、`e2e-contract-check` → 通过
- 2026-11-18 网关 e2e 拆成 3 个并行分片（storage / self-monitoring / alerts-scrape-tls），`LO_E2E_GATEWAY_TESTS` 选择分片，契约检查要求分片并集正好是全集；本地 `check-e2e-contract.sh` 在漏测与重复时均失败、拼错名字被 `e2e.sh` 拒绝，`alerts-scrape-tls` 分片 `mise run e2e` → 通过（119s）
