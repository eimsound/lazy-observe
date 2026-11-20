# 依赖升级统一交给 Renovate，不再用 Dependabot 和自写的固定点追踪

Dependabot 只认清单文件（go.mod、pnpm-lock.yaml、pyproject.toml、Dockerfile、workflow），嵌入的 Victoria 二进制、Trivy、mise 引导版本、e2e 镜像这些写在 shell 脚本里的手工固定点它看不到。为此我们曾自写 `scripts/check-pinned-releases.sh`（约 780 行，另有约 530 行测试），加上每周的 `pinned-releases.yml` 追踪 issue，还因 aquasecurity 的 IP allow list 挡住 Actions token 而不得不抓 github.com 网页兜底。我们决定改用 Mend 托管的 Renovate GitHub App 统一管理全部依赖（配置 `.github/renovate.json`）：清单类依赖走 Renovate 原生 manager，手工固定点用 `customManagers` 正则；带 sha256 的固定点用 `github-release-attachments` datasource，由 Renovate 在同一个 PR 里同步改版本和 digest，e2e 镜像用 `docker` datasource；所有依赖统一 `minimumReleaseAge: 7 days`。自写的固定点脚本、追踪 workflow、`ci.yml` 的 `pinned-releases` job 和 `dependabot.yml` 一并删除。方案细节、各 manager 的要点与负责人操作见 `.agents/docs/plans/renovate.md`。

## 决策

- **sha256 随版本一起升**：每个 sha 行行尾加 `# <release tag>`（`scripts/mise-release.env`、`scripts/download-storage-bins.sh`、`scripts/install-trivy.sh`），让「版本 + digest」落在同一个正则匹配里；消费脚本不改，行尾注释对 bash 无影响。不用 `postUpgradeTasks`（Mend 免费托管不允许），不用 Actions 回写（要 PAT，且 Renovate 会把被别人改过的分支当作已放弃）。
- **Trivy 继续用脚本下载，不用 trivy-action 或 setup-trivy**（2026-03 这两个 action 被劫持，见 `scripts/install-trivy.sh` 头注释）。原先靠人工 `gh release verify` 核对 release 证明，现在改由 Renovate 升级，靠 7 天最小发布年龄和 sha256 校验；`gh release verify` 要调 API，会被 aquasecurity 的 IP allow list 拦住，不进 CI。
- **sha 错配的兜底是 CI 自己变红，不写专门的一致性检查**：mise 引导（每个 job 的 `ci-setup-mise.sh` 与 Dockerfile）和 Victoria 存储二进制（e2e 等 job）的 sha 在现有 PR CI 里本来就会下载并 `sha256sum --check`。Trivy 的 sha 此前只在发版时才被校验，所以新增极简的 `trivy-pin` job：`scripts/install-trivy.sh` 改动时在 amd64、arm64 上各跑一次 `bash scripts/install-trivy.sh`（下载加 sha256 校验），不查 API、不算年龄、不要 token，计入 `ci-ok`。
- **安全更新**：`vulnerabilityAlerts` 开启并显式写 `minimumReleaseAge: "7 days"`（Renovate 默认安全更新不等待，必须显式写才与 pnpm 的 `minimumReleaseAge`、uv 的 `exclude-newer` 一致）；`osvVulnerabilityAlerts` 关闭。仓库里保持 Dependency graph 与 Dependabot alerts 开启、Dependabot security updates 关闭，Renovate 的 `[SECURITY]` PR 是唯一来源。安装 App 时要确认它拥有 Dependabot alerts 的 Read 权限。
- **Go**：gomod 照常周更，不设 Dashboard 批准。`internal/db/ent` 纳入 git 之后，根模块的 `go get -t ./...` 加 `go mod tidy` 可以全自动完成（`.agents/docs/research/ent-renovate.md` 实测），但进二进制的模块升级后 `THIRD_PARTY_NOTICES.txt` 要补生成，Victoria 升级要同步 `third_party/victoria/SOURCES.txt`，否则 `license-check` 变红（计划里的审查清单）。`entc.mod` 的 `entc.sum` 自动补不出来：`renovate.json` 让 `entgo.io/**` 在 `go.mod` 与 `entc.mod` 里同一 PR 升级（组 `ent`，版本必须一致），该 PR 很可能带 artifact 失败，合并前由 agent 按计划里的审查清单手工补 sum。
- **没有时间戳的依赖**：Quay 上的 Victoria 集群镜像（vmstorage、vminsert、vmselect）、seaweedfs、postgres、Dockerfile 的 debian、`govulncheck`、gcr distroless 都拿不到发布时间，统一设 `minimumReleaseAgeBehaviour: timestamp-optional`，有新版就自动开 PR，不设 `dependencyDashboardApproval`。曾考虑把 Victoria 集群镜像改到 Docker Hub 来恢复 7 天检查：Docker Hub 上有同 tag、同 index digest 的发布（2026-11-20 对 `v1.156.0-cluster-scratch` 三个镜像逐个比对一致，Hub API 也有 `tag_last_pushed`），但 Renovate 44.253.4 抓 Hub 标签时请求 `page_size=1000`，Hub 每页只给 100 条、第 11 页起返回 403，标签超过 1000 的仓库（这三个各约 1900 个，debian、postgres、seaweedfs 同理）整次查询回退成无时间戳，实测 `releaseTimestamp` 仍为空、更新被搁置。所以 e2e 保持 Quay 地址，按无时间戳处理。postgres、valkey 这类 e2e 夹具镜像只升 minor/patch，不升大版本。

## 备选方案

- **继续 Dependabot + 自写脚本（拒绝）**：Dependabot 管不了的部分只能自己维护，脚本复杂、易碎，还要配测试，维护成本与收益不成比例。
- **Renovate 只管手工固定点，与 Dependabot 并存（拒绝）**：两个 bot 两套配置、两套排期和 cooldown 规则，没有必要。
- **Actions 自托管 Renovate（拒绝）**：`GITHUB_TOKEN` 开的 PR 不触发 CI，也改不了 workflow 文件，需要另建 GitHub App 或 PAT 存进 secret，并维护 action 本身；托管 App 的写权限与 Dependabot 相当，信任面没有扩大。
- **用 mise 管 Trivy 等工具（不单独采用）**：mise 有 `minimum_release_age`，但只安装本机架构，不适合嵌入双架构产物的 Victoria，而且它本身也需要有人来提升级，最终仍落到 Renovate 的 mise manager。
- **写 `check-pin-tags.sh` 检查 sha 行的 tag 与版本变量一致（拒绝）**：现有 CI 的下载校验已经覆盖 mise 与 Victoria，只有 Trivy 缺口，用一个只跑安装脚本的 `trivy-pin` job 补上，不再增加一套静态断言。
- **用 trivy-action / setup-trivy 让 Renovate 直接管 Trivy（拒绝）**：见上，这两个 action 的 tag 被劫持过。

## 后果

- 手工固定点（Victoria 存储二进制、e2e 镜像、Trivy、mise 引导版本）不设每周排期，满 7 天就开 PR；其余依赖每周一批。
- 「是否落后上游」不再由脚本判断：发版前由负责人在 Dependency Dashboard 勾选重跑 Renovate，确认没有待合并的固定点升级（见 ADR 0016）。
- 7 天年龄闸只管 Renovate 自己开的 PR：人手改的固定点不再有年龄检查（旧 `pinned-releases` job 的功能），也没有 `gh release verify`；首个固定点 PR 和首个 actions PR 要人工看 diff（见计划里的审查清单）。
- GHCR、Quay、GCR 不提供发布时间，Docker Hub 上标签超过 1000 个的仓库也拿不到，这些依赖不受 7 天限制（`timestamp-optional`）。这与主流做法一致：Renovate 的 `config:best-practices` 也只对 npm 做年龄限制。
- Renovate 本身是 AGPL-3.0，但它是外部托管服务，不进依赖树、不进产物，不受许可证白名单约束；也因此不要在仓库 CI 里运行 `renovate-config-validator`，配置校验靠本地一次性运行和 Mend App 对 `renovate/reconfigure` 分支的检查。
- Python 依赖没有 lockfile，运行时的最小发布年龄由 `sdk/python/pyproject.toml` 的 `[tool.uv] exclude-newer = "7 days"` 保证（#119 已落地）；Renovate 的 `minimumReleaseAge` 只约束它自己开的 PR。`scripts/check-github-workflows.sh` 校验 `scripts/release-age.env`、`renovate.json`（顶层与 `vulnerabilityAlerts`）、各 pnpm 工程和 `exclude-newer` 的天数一致，并要求 `renovate.json` 的 `internalChecksFilter` 全是 `strict`（否则年龄闸不拦 PR）。
