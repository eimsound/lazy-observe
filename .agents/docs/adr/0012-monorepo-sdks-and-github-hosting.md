# SDK 并入单仓，GitHub 作为公开主开发仓

lazy-observe 原先由两个私有仓库组成：服务端仓库与独立的客户端 SDK 仓库（Go / Node / Browser / Python）。服务端二进制本身 import Go SDK 做自监控，于是服务端 `go.mod` 用 `replace` 指向兄弟目录的 SDK checkout，CI 用只读部署密钥克隆私有 SDK，发版前还要过一道「SDK 对齐」门禁把 SDK 固定到最新 tag。全新 clone 无法单独构建，协议改动要在两个仓库之间靠纪律同步。我们决定在首次公开发布前：

- **SDK 并入本仓库** `sdk/{go,node,browser,python}`。Go SDK 是嵌套模块 `github.com/eimsound/lazy-observe/sdk/go`，有自己的 `go.mod`，SDK 使用者不会被拖进服务端依赖；根 `go.mod` 用 `replace github.com/eimsound/lazy-observe/sdk/go => ./sdk/go`。SDK 以带前缀的 tag 发布：`sdk/go/vX.Y.Z`（Go 嵌套模块约定）、`sdk-node-vX.Y.Z`、`sdk-browser-vX.Y.Z`、`sdk-python-vX.Y.Z`；服务端仍用 `vX.Y.Z`。
- **GitHub（`eimsound/lazy-observe`）是公开主开发仓**，许可证 MIT，版权人 ClanEver。issue、PR、CI、Release 与容器镜像（`ghcr.io/eimsound/lazy-observe`）都在 GitHub 上。原 Gitea 上的两个私有仓库只读归档，不删除，旧提交与工单可在那里追溯。
- **公开仓以单个初始提交起步**，不带私有开发历史。历史中只建在本地、从未推送的 `m1`–`m5` 里程碑 tag 随之放弃；`CHANGELOG.md` 把它们合并为一段不带 tag 链接的「Pre-release history」。
- **公开仓不挂自建 runner**。所有 workflow 跑在 GitHub 托管 runner（`ubuntu-24.04`、原生 arm64 `ubuntu-24.04-arm`）上。CI 由 `changes` 作业按改动路径挑选业务 job，`ci-ok` 汇总为唯一 required check；纯文档改动不跑业务 job。job 直接跑在 runner 上，工具由 mise 安装并按 job 缓存，不使用预构建的 CI 容器镜像：镜像要先构建推送才能被 CI 拉取，ghcr 包默认私有导致 fork 拉不到，还要维护镜像版本，而它装的工具 mise 都能直接装。

## 备选方案

- **SDK 保持独立仓并一起公开（拒绝）**：去掉了私有密钥，但服务端构建仍依赖另一个仓库的版本，协议改动仍跨两个 PR，发版仍需对齐门禁。
- **单模块（SDK 作为服务端模块的子包）（拒绝）**：`go get` SDK 会把 Ent、Fiber、存储客户端等服务端依赖带进使用者的模块图。
- **保留私有历史后公开（拒绝）**：历史里有内部基础设施地址、私有仓库引用和过程记录，逐条改写历史的成本与遗漏风险高于重置。
- **继续用自建 runner 跑公开仓 CI（拒绝）**：任何 fork PR 都能在维护者机器上执行代码。
- **workflow 级 `paths:` 过滤做增量 CI（拒绝）**：被跳过的 workflow 不上报状态，设为 required 的检查会一直等待。

## 后果

- 全新 clone 不需要任何兄弟目录或密钥即可构建；`ci-prepare-sdk.sh`、`ci-sdk-git.sh`、SDK 对齐门禁与 SDK 侧的服务端兼容构建验证一并删除。
- 协议面改动、`docs/version-matrix.md` 与两端代码在同一个 PR 里修改，同一次 CI 覆盖。
- 不能用 `go install github.com/eimsound/lazy-observe/cmd/lazy-observe@<version>` 安装服务端（主模块带 `replace`）；服务端只以发布归档、容器镜像和 Helm chart 分发。
- 性能门禁在共享 runner 上噪声较大，只在手动触发和发版前运行，不挡普通 PR。
- 公开仓文档不再引用私有历史的提交哈希；需要追溯时去 Gitea 私有归档查。
