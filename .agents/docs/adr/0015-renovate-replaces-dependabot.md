# 依赖升级统一交给 Renovate，不再用 Dependabot 和自写的固定点追踪

Dependabot 只认清单文件（go.mod、pnpm-lock.yaml、pyproject.toml、Dockerfile、workflow），嵌入的 Victoria 二进制、Trivy、mise 引导版本、e2e 镜像这些写在 shell 脚本里的手工固定点它看不到。为此我们曾自写 `scripts/check-pinned-releases.sh`（约 780 行，另有约 530 行测试），加上每周的 `pinned-releases.yml` 追踪 issue，还因 aquasecurity 的 IP allow list 挡住 Actions token 而不得不抓 github.com 网页兜底。我们决定改用 Mend 托管的 Renovate GitHub App 统一管理全部依赖（配置 `.github/renovate.json`）：清单类依赖走 Renovate 原生 manager，手工固定点用 `customManagers` 正则；带 sha256 的固定点（mise 引导、Victoria）用 `github-release-attachments` datasource，由 Renovate 在同一个 PR 里同步改版本和 digest（Trivy 是例外，见「决策」），e2e 镜像用 `docker` datasource；所有依赖统一 `minimumReleaseAge: 7 days`。自写的固定点脚本、追踪 workflow、`ci.yml` 的 `pinned-releases` job 和 `dependabot.yml` 一并删除。方案细节、各 manager 的要点与负责人操作见 `.agents/docs/plans/renovate.md`。

## 决策

- **sha256 随版本一起升**：每个 sha 行行尾加 `# <release tag>`（`scripts/mise-release.env`、`scripts/download-storage-bins.sh`；Trivy 例外，见下条），让「版本 + digest」落在同一个正则匹配里；消费脚本不改，行尾注释对 bash 无影响。不用 `postUpgradeTasks`（Mend 免费托管不允许），不用 Actions 回写（要 PAT，且 Renovate 会把被别人改过的分支当作已放弃）。
- **Trivy 继续用脚本下载，不用 trivy-action 或 setup-trivy**（2026-03 这两个 action 被劫持，见 `scripts/install-trivy.sh` 头注释）。原先靠人工 `gh release verify` 核对 release 证明，现在改由 Renovate 升级，靠 7 天最小发布年龄和 sha256 校验；`gh release verify` 要调 API，会被 aquasecurity 的 IP allow list 拦住，不进 CI。
  **Trivy 的 sha 不由 Renovate 改**：托管 Renovate 上线后，`github-release-attachments` 对 aquasecurity 报 `lookupUpdates error`（推断是同一个 IP allow list 对 installation token 的 REST 调用返回 403），两个 sha 依赖从 Detected 里消失。所以 `renovate.json` 里 Trivy 只剩版本依赖（`trivy` 组），升级 PR 只改 `trivy_version`，两个 sha256 由 agent 从 `https://github.com/aquasecurity/trivy/releases/download/v<X>/trivy_<X>_checksums.txt` 补（release 下载地址，不走 API；命令见计划的审查清单）。sha 行也因此不再带行尾 `# <tag>`。补 sha 之前 `trivy-pin` 会红，这是预期的兜底。
- **sha 错配的兜底是 CI 自己变红，不写专门的一致性检查**：mise 引导（每个 job 的 `ci-setup-mise.sh` 与 Dockerfile）和 Victoria 存储二进制（e2e 等 job）的 sha 在现有 PR CI 里本来就会下载并 `sha256sum --check`。Trivy 的 sha 此前只在发版时才被校验，所以新增极简的 `trivy-pin` job：`scripts/install-trivy.sh` 改动时在 amd64、arm64 上各跑一次 `bash scripts/install-trivy.sh`（下载加 sha256 校验），不查 API、不算年龄、不要 token，计入 `ci-ok`。
- **安全更新**：`vulnerabilityAlerts` 开启并显式写 `minimumReleaseAge: "7 days"`（Renovate 默认安全更新不等待，必须显式写才与 pnpm 的 `minimumReleaseAge`、uv 的 `exclude-newer` 一致）；`osvVulnerabilityAlerts` 关闭。仓库里保持 Dependency graph 与 Dependabot alerts 开启、Dependabot security updates 关闭，Renovate 的 `[SECURITY]` PR 是唯一来源。安装 App 时要确认它拥有 Dependabot alerts 的 Read 权限。
- **Go**：gomod 照常周更，不设 Dashboard 批准。`internal/db/ent` 纳入 git 之后，根模块的 `go get -t ./...` 加 `go mod tidy` 可以全自动完成（`.agents/docs/research/ent-renovate.md` 实测），但进二进制的模块升级后 `THIRD_PARTY_NOTICES.txt` 要补生成，Victoria 升级要同步 `third_party/victoria/SOURCES.txt`，否则 `license-check` 变红（计划里的审查清单）。`entc.mod` 的 `entc.sum` 自动补不出来：`renovate.json` 让 `entgo.io/**` 在 `go.mod` 与 `entc.mod` 里同一 PR 升级（组 `ent`，版本必须一致），该 PR 很可能带 artifact 失败，合并前由 agent 按计划里的审查清单手工补 sum。
- **没有时间戳的依赖**：仅对确实拿不到发布时间的 5 项——Quay 上的 Victoria 集群镜像（vmstorage、vminsert、vmselect）、gcr distroless、`govulncheck`——设 `minimumReleaseAgeBehaviour: timestamp-optional`，有新版就单独自动开 PR，不设 `dependencyDashboardApproval`。**Docker Hub 镜像（`chrislusf/seaweedfs`、`postgres`、`debian`）不列入该豁免，统一走全局默认 `timestamp-required`（fail-closed）**：此前曾因本地 `--platform=local` 的 Renovate 44.253.4 抓 Hub 标签请求 `page_size=1000`、第 11 页起返回 403 导致整次查询回退成无时间戳，而把三者留在 `timestamp-optional` 作兜底；但 2026-11-21 PR #283（`chrislusf/seaweedfs:4.54` 仅发布 5 天即被提前开 PR）实测表明，托管环境偶发抓不到 Hub 分页时间戳时，`timestamp-optional` 会绕过 7 天门禁提前开 PR。改为默认 `timestamp-required` 后，正常抓取仍按满 7 天开 PR，偶发抓取失败只会在后台维持 pending 状态（可在 Mend Web Portal `https://developer.mend.io/github/eimsound/lazy-observe` 查看），待下一轮正常抓取后放行。e2e 的 Victoria 集群镜像目前仍用 Quay 地址、按无时间戳处理，是否改到 Docker Hub（2026-11-20 对 `v1.156.0-cluster-scratch` 三个镜像逐个比对同 tag 同 index digest 一致）是单独的决定。postgres、valkey 这类 e2e 夹具镜像只升 minor/patch，不升大版本。
- **关闭 Dependency Dashboard Issue**：`.github/renovate.json` 显式配置 `"dependencyDashboard": { "enabled": false }`（保留对象写法以满足 Renovate 44 `--strict` 规范），默认关闭 GitHub Dependency Dashboard Issue，避免未满 7 天最小发布年龄或未到周一排期的依赖长期挂在仓库 Issues 列表造成噪音；Renovate 仅在依赖真正满足发布年龄和排期条件时自动开 PR。查看 pending 状态、排期队列或告警/错误日志改走 Mend Web Portal（`https://developer.mend.io/github/eimsound/lazy-observe`）；如需启用改回 `{"enabled": true}` 即可。

## 备选方案

- **继续 Dependabot + 自写脚本（拒绝）**：Dependabot 管不了的部分只能自己维护，脚本复杂、易碎，还要配测试，维护成本与收益不成比例。
- **Renovate 只管手工固定点，与 Dependabot 并存（拒绝）**：两个 bot 两套配置、两套排期和 cooldown 规则，没有必要。
- **Actions 自托管 Renovate（拒绝）**：`GITHUB_TOKEN` 开的 PR 不触发 CI，也改不了 workflow 文件，需要另建 GitHub App 或 PAT 存进 secret，并维护 action 本身；托管 App 的写权限与 Dependabot 相当，信任面没有扩大。
- **用 mise 管 Trivy 等工具（不单独采用）**：mise 有 `minimum_release_age`，但只安装本机架构，不适合嵌入双架构产物的 Victoria，而且它本身也需要有人来提升级，最终仍落到 Renovate 的 mise manager。
- **写 `check-pin-tags.sh` 检查 sha 行的 tag 与版本变量一致（拒绝）**：现有 CI 的下载校验已经覆盖 mise 与 Victoria，只有 Trivy 缺口，用一个只跑安装脚本的 `trivy-pin` job 补上，不再增加一套静态断言。
- **用 trivy-action / setup-trivy 让 Renovate 直接管 Trivy（拒绝）**：见上，这两个 action 的 tag 被劫持过。

## 后果

- 手工固定点（Victoria 存储二进制、e2e 镜像、Trivy、mise 引导版本）不设每周排期，满 7 天就开 PR；其余依赖每周一批。
- 「是否落后上游」不再由脚本判断：发版前由负责人在 Mend Web Portal（`https://developer.mend.io/github/eimsound/lazy-observe`）查看 pending/运行状态并确认仓库没有待合并的固定点升级（见 ADR 0016；Dependency Dashboard Issue 默认关闭以减少 Issues 噪音，如需临时启用改回 `{"enabled": true}` 即可）。
- 7 天年龄闸只管 Renovate 自己开的 PR：人手改的固定点不再有年龄检查（旧 `pinned-releases` job 的功能），也没有 `gh release verify`；首个固定点 PR 和首个 actions PR 要人工看 diff（见计划里的审查清单）。
- `timestamp-optional` 仅限真正不提供时间戳的 5 项（Quay 的 vm*、GCR distroless、`govulncheck`）；Docker Hub（含标签超过 1000 个的 seaweedfs、postgres、debian）统一走全局默认 `timestamp-required`（fail-closed），避免标签分页偶发失败时绕过 7 天发布年龄门禁。这与主流做法一致：Renovate 的 `config:best-practices` 也只对 npm 做年龄限制。
- Renovate 本身是 AGPL-3.0，但它是外部托管服务，不进依赖树、不进产物，不受许可证白名单约束；也因此不要在仓库 CI 里运行 `renovate-config-validator`，配置校验靠本地一次性运行和 Mend App 对 `renovate/reconfigure` 分支的检查。
- Python 依赖没有 lockfile，运行时的最小发布年龄由 `sdk/python/pyproject.toml` 的 `[tool.uv] exclude-newer = "7 days"` 保证（#119 已落地）；Renovate 的 `minimumReleaseAge` 只约束它自己开的 PR。`scripts/check-github-workflows.sh` 校验 `scripts/release-age.env`、`renovate.json`（顶层与 `vulnerabilityAlerts`）、各 pnpm 工程和 `exclude-newer` 的天数一致，并要求 `renovate.json` 的 `internalChecksFilter` 全是 `strict`（否则年龄闸不拦 PR）。
