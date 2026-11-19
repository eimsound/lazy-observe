# 手工固定点的最小发布年龄防线与新版本追踪

## 目标与来源

- 负责人原话：「dependbot没报告要升级这些吗 应该追较新的版本 再加个防供应链攻击的 min release age」。
- 最终状态：
  1. 一个单一来源的最小发布年龄（默认 7 天，与 Dependabot `cooldown` 一致）。
  2. `mise run pinned-releases`：从固定点文件里解析 GitHub release 型手工固定点，**门禁**（发布不满 N 天就失败）与**报告**（当前版本 vs 已满 N 天的最新稳定版，落后标 outdated）两种模式。
  3. PR 改了固定点文件时 ci.yml 跑门禁；每周定时 workflow 跑报告，有 outdated 就变红并维护一个 issue。
  4. web、sdk/node、sdk/browser 的 pnpm 设 `minimumReleaseAge`（7 天）。
- 范围外：不改 Victoria / mise / Trivy 的版本号（V3 切片升级 Victoria，SC2 切片拆出 `scripts/install-trivy.sh`）；不改 `requirements.md`；不加依赖。

## 仓库证据

- `.github/dependabot.yml`：每个生态条目都有 `cooldown.default-days: 7`；头注释列出了手工固定点。Dependabot 只认 `go.mod`、`pnpm-lock.yaml`、`Dockerfile` 等清单。
- 手工固定点（机器可读位置）：
  - `scripts/download-storage-bins.sh`：`metrics_version=`、`logs_version=`、`traces_version=`（+ 每架构 sha256）。
  - `scripts/mise-release.env`：`MISE_BOOTSTRAP_VERSION=`（+ 两架构 sha256），`check-github-workflows.sh` 已校验格式。
  - `scripts/check-image-vulns.sh`：`trivy_version=`（+ 两架构 sha256）；SC2 合入后搬到 `scripts/install-trivy.sh`。
  - `scripts/e2e-multi-replica.sh`：`quay.io/victoriametrics/vm{storage,insert,select}:vX.Y.Z-cluster-scratch@sha256:…`，另有 SeaweedFS / Valkey / Postgres 镜像。
  - workflow 的 BuildKit `driver-opts: image=moby/buildkit:vX.Y.Z@sha256:…`；`mise.toml` 工具版本；`scripts/ci-setup-mise.sh` 工具清单；`entc.mod`/`entc.sum`。
- GitHub API 实测（2026-11-19）：
  - VictoriaMetrics 仓库同时有 LTS 线（`v1.136.x`、`v1.122.x`）和主线（`v1.148.x` 补丁线、`v1.156.0`）；LTS 补丁发布时间晚于主线新 minor，所以「最新」必须按 semver 取最大，不能按发布时间取最新。`-cluster`、`-enterprise` 后缀 tag 要排除。
  - VictoriaLogs `v1.54.0`（2026-11-13）、`v1.52.2`（2026-11-13）发布不满 7 天；VictoriaLogs 满 7 天的最新是 `v1.53.0`。
- pnpm 10.33.0：`pnpm config get minimumReleaseAge` 读 `pnpm-workspace.yaml` 里的 `minimumReleaseAge`（单位分钟），也认 `.npmrc` 的 `minimum-release-age`。

## 手工固定点清单与覆盖情况

| 固定点 | 位置 | 本切片 |
| --- | --- | --- |
| VictoriaMetrics / VictoriaLogs / VictoriaTraces 二进制 | `scripts/download-storage-bins.sh` | 门禁 + 报告 |
| e2e 的 vmstorage / vminsert / vmselect 镜像（tag 对应 VictoriaMetrics release） | `scripts/e2e-multi-replica.sh` | 门禁 + 报告（只查 tag 对应 release 的年龄，不验证 digest 是否属于该 tag） |
| mise 引导版本 | `scripts/mise-release.env` | 门禁 + 报告 |
| Trivy | `scripts/check-image-vulns.sh`，SC2 后 `scripts/install-trivy.sh` | 门禁 + 报告（位置自动探测） |
| BuildKit 镜像 digest | workflow `driver-opts` | 暂不检查：tag 对应的发布时间要走 Docker Hub 的镜像仓库 API，不是 GitHub release |
| e2e 的 SeaweedFS / Valkey / Postgres 镜像 | `scripts/e2e-multi-replica.sh` | 暂不检查：同上 |
| `mise.toml` 工具版本、`ci-setup-mise.sh` 清单 | — | 暂不检查：工具来源各异（aqua、go、github…），tag 前缀不统一；mise 自带的发布时间过滤是否适用于精确固定的版本，本切片未评估 |
| `entc.mod` / `entc.sum` | — | 暂不检查：Go 模块，可用 `go list -m -json` 的 `Time` 字段，留作后续 |

## 备选路线

- **A. 一个脚本，门禁 + 报告两种模式（选用）**：`scripts/check-pinned-releases.sh --gate|--report`，固定点从现有文件里直接解析（不另建清单，避免「清单与真实固定点漂移」）。门禁只查被固定的 release（调用少），报告另拉 release 列表。PR 里在 ci.yml 新 job 跑门禁，定时 workflow 跑报告。
- **B. 固定点集中到一个机器可读清单（如 `pins.json`），脚本与各安装脚本都读它**：数据源唯一，但要改 `download-storage-bins.sh`、`check-image-vulns.sh`、e2e 脚本、Dockerfile 的取值方式，波及打包链路与 HA e2e，且 V3、SC2 正在改这些文件，会产生冲突。
- 选 A：不动现有消费者，冲突面最小；解析时「恰好一行」才算数，格式漂移会失败而不是放行。代价是脚本里有解析规则，格式变了要同步改（测试会钉住）。

## 设计选择

- **年龄常量**：`scripts/release-age.env` 的 `MIN_RELEASE_AGE_DAYS=7`。`check-github-workflows.sh` 校验 `dependabot.yml` 每个条目的 `cooldown.default-days`、每个 `pnpm-workspace.yaml` 的 `minimumReleaseAge`（= 天数 × 1440）与它一致。
- **门禁放哪**：`workflow-check` 是离线的，加网络依赖会让它在限流/断网时无故变红，所以不放进去。放进 ci.yml 新 job `pinned-releases`，由 `scripts/ci-changes.sh` 新的 `pins` 标志控制，只在固定点文件变化时跑；`ci-ok` 汇总它。`workflow-check` 里只跑它的离线测试（`check-pinned-releases-test.sh`，假 API + 固定时间）。
- **门禁范围**：门禁检查所有固定点而不是「这个 PR 改了的那个」。固定点只会变老，不会变年轻，所以全量检查不会误伤。
- **认证与失败**：`GH_TOKEN`/`GITHUB_TOKEN`，没有就匿名并在 stderr 提示限流。网络错误、HTTP 非 200、固定点解析不到都退出 2，不会静默放行；curl 自带 3 次重试。
- **认证 403 的备选来源（PR #70 CI、main push run 44487084652 暴露）**：aquasecurity 组织开了 IP allow list，Actions 里任何带认证的 API 请求都 403，同一 job 里别的仓库是 200。#70 的对策「认证 403 非限流 → 匿名 API 重试一次」在 main 上失败：GitHub 托管 runner 的出口 IP 被大量用户共享，匿名额度（每 IP 每小时 60 次）常被别人用光（`anonymous HTTP 403 (API rate limit exceeded for 98.89.53.239 ...)`）。现在的来源顺序：带认证 API → （仅非限流的认证 403）github.com 的 release 页面 → 匿名 API（最后备选，只在页面失败时多试一次，不改变判定）。限流（429、`x-ratelimit-remaining: 0`、`retry-after`、message 含 `rate limit`）不走备选，直接退出 2；每次失败都打印响应体的 `.message`（非 JSON 时退回前 200 个字符）。全部来源失败退出 2。
  - 网页来源的选择：本地对 trivy 最近 12 个 release 以及 VictoriaMetrics（6）、VictoriaLogs（5）、VictoriaTraces（4）、mise（5）用真实 API `published_at` 对比三个候选（2026-11-19）：
    - (b) release 页面 `released this <relative-time datetime=...>`：31/31 与 `published_at` 逐秒相同。**选用**；页面带 `Pre-release` 标签的算 prerelease，draft 对匿名是 404（取不到 → 失败）；
    - (a) release 资产下载响应的 `last-modified`：是资产上传时间，比 `published_at` 早 4~8 秒（trivy）、1~18 分钟（VictoriaLogs/Traces、mise）、**最多近 3 天（VictoriaMetrics，起草 release 后很久才发布）**。会高估年龄、放过不满 7 天的版本，**不能用于门禁**；另外每个固定点要维护一个资产名；
    - (c) `releases.atom` 的 `<updated>`：只含最近约 10 条（VictoriaMetrics 里还混着 `-cluster`、`pmm-*` 条目），值是最近一次编辑时间，常晚于 `published_at`（最多晚 30 天），不是发布时间，也不稳定。**不用**。
  - 页面解析必须锚定在「released this」后面（页面里还有 tag 的提交时间等别的 `<relative-time>`）；解析不到（改版、tag 没有 release）算失败，不猜。这些网页端点从 Actions 访问是否也受 IP allow list 影响，本地无法验证，交给 PR CI。
  - 匿名 API 保留为最后备选而不是去掉：它是唯一与 `published_at` 字段同源的第二来源，成本是页面失败时多一次请求；它失败只会让门禁退出 2，不会放行。
  - release 列表（报告模式的「最新合格版」）没有网页等价物，被拒绝的仓库降级：`https://github.com/OWNER/REPO/releases/latest` 重定向拿 GitHub 标记的 Latest tag，再用上面的页面取它的发布时间。Latest 严格匹配 `<前缀>X.Y.Z`、非 prerelease、已满 N 天：它是「最新」，固定点等于它为 ok、低于它为 outdated；否则（不满 N 天、不是严格 tag、比固定点还旧、prerelease）标 `unknown`，不算 outdated、不算 finding，每周 job 不变红，报告和 issue 里写明原因。`releases/latest` 或 Latest 的页面取不到仍然退出 2。
  - `--latest-eligible` 对被拒绝的仓库**不降级，直接退出 2**：GitHub 的 Latest 不一定是 semver 最大（VictoriaMetrics 有 LTS 补丁线），而 `check-storage-bin-vulns.sh` 用它把「pin >= 最新合格版」的命中降为警告，给错答案就是放行。该脚本本来就把退出 1、2 都当作阻塞（fail-closed）。
  - 离线测试：`<owner>__<repo>/web/tags/<tag>.html`（release 页面）、`web/latest`（迷你 HTTP 响应，302 + location）、`<文件>.web-error`；请求日志多一类 `web`，断言每种场景各来源被请求的次数。
- **`--latest-eligible OWNER/REPO`**：只在 stdout 打印「已满 N 天的最新稳定版」tag，规则与报告一致（严格 `vX.Y.Z`、排除 draft/prerelease、按 `published_at` 截止、semver 取最大），不读固定点文件；没有合格 release 退出 1，API 失败（含被 IP allow list 拒绝）退出 2。给 SC2 的扫描门禁用：判断「pin 已是最新合格版 → CVE 只警告」。
- **报告判定**：「最新」= 已满 N 天、严格匹配 `v<数字>.<数字>.<数字>`、非 draft / prerelease 里 semver 最大者。当前 < 最新 → outdated；当前发布不满 N 天 → too-new；两者都让报告退出 1。
- **每周追踪**：`pinned-releases.yml`（`schedule` + `workflow_dispatch`），有 outdated 就让 job 变红，并在固定标题的 issue 里记录清单。issue 的取舍：只靠红 job，通知只会发给最后改 cron 的人，没有持久的清单、也没人跟进进度；所以开 issue，但做到「标题固定，已有未关闭的就更新正文，没有才新建，全部追平后自动关闭」，不会重复开。`issues: write` 只在这个 job 上，`check-github-workflows.sh` 校验别的 workflow 不得持有。脚本出错（退出 2）时不动 issue，只让 job 变红。
- **pnpm**：写在各工程的 `pnpm-workspace.yaml`（pnpm 10 的工程级设置位置；`.npmrc` 的 `minimum-release-age` 也实测可用，两者择一即可）。web 原本没有该文件，新建并显式 `packages: ["."]`。实测 `--frozen-lockfile` 不受影响（见下）。
- **Python（`sdk/python`）结论：记为后续**。`pip install` 在 pip ≥ 26 有 `--uploaded-prior-to P7D`（实测 pip 26.3：`P7D` 可装，`P36500D` 报 `No matching distribution`，行为符合预期；旧 pip 不认这个选项，需先 `pip install --upgrade pip`）；uv 的对应机制是 `exclude-newer`。不顺手加的原因：`sdk-python` 任务用 pip，仓库约定的 Python 环境是 uv，要改 CI 的 `sdk-python` profile 就得顺带决定「是否切 uv」，不是一行改动；而且 `sdk/python` 没有锁文件，依赖是下限范围，该项只影响 CI 与开发环境的解析，不影响发布给用户的包。后续做法二选一：在 `mise.toml` 的 `sdk-python` 里给 `pip install -e` 加 `--uploaded-prior-to "P${MIN_RELEASE_AGE_DAYS}D"`（先 source `scripts/release-age.env`），或切 uv 后在 `pyproject.toml` 设 `[tool.uv] exclude-newer = "7 days"`。

## 与其它切片的接口

- **SC2**（`slice/storage-bin-trivy-2`）把 Trivy 安装抽到 `scripts/install-trivy.sh`：脚本按 `scripts/install-trivy.sh`、`scripts/check-image-vulns.sh` 的顺序找第一个含 `trivy_version=`（或 `TRIVY_VERSION=`）的文件；**SC2 合入后无需改脚本**。要同步改的只有：`scripts/ci-changes.sh` 的 `pinned_release_paths` 里已经包含 `scripts/install-trivy.sh`（预留），`dependabot.yml` 头注释里提到了两个位置。如果 SC2 把变量改了名，改 `check-pinned-releases.sh` 里「Trivy」那段的变量名列表和 `scripts/check-pinned-releases-test.sh` 里对应的 `sed`。
- **V3**（升级 Victoria 到满足最小发布年龄的最新版）：只改 `download-storage-bins.sh` 的版本号与 sha，本切片不动。注意 e2e 的 VictoriaMetrics 集群镜像（`scripts/e2e-multi-replica.sh`，`v1.147.0-cluster-scratch`）也被报告覆盖，不升级会一直显示 outdated；V3 若不打算同批升级它们，需要在报告里知道这一点。

## Tracer Bullet 与 red/green

一个切片，按下面的顺序（每步先红后绿）：

1. 年龄常量与一致性校验
   - red：把 `scripts/release-age.env` 改成 14（或把 `dependabot.yml` 某个 `default-days` 改成 3、`sdk/node/pnpm-workspace.yaml` 的 `minimumReleaseAge` 改成 1440），`bash scripts/check-github-workflows.sh` 失败；
   - green：还原后 `mise run workflow-check`。
2. 检查脚本（离线）
   - red/green：`bash scripts/check-pinned-releases-test.sh`（假 API、固定时间）。覆盖：LTS/`-cluster`/`-enterprise`/prerelease/draft/不满 7 天的过滤、门禁的太新判定与「还要等多久」、恰好 7 天的边界、固定点格式漂移、API 取不到时 fail closed。
3. 真实 API
   - 报告：`mise run pinned-releases`，预期 Victoria 与 mise 标 outdated；
   - 门禁：`mise run pinned-releases -- --gate`，预期通过；
   - 负向：临时副本里把 VictoriaLogs 固定到 `v1.54.0` / `v1.52.2`，门禁必须失败。
4. CI 接线
   - red/green：`bash scripts/ci-changes-test.sh`（`pins` 标志）；`mise run workflow-check`（含 `actionlint`、ci-ok needs、`issues: write` 只在追踪 workflow）。
5. pnpm
   - `pnpm install --frozen-lockfile`（web、sdk/node、sdk/browser）+ `mise run sdk-node`、`mise run sdk-browser`、web 的 `pnpm build`；负向：在临时目录用极大的 `minimumReleaseAge` 做非冻结的 `pnpm add`，必须被拒绝，证明设置真的生效。

## 执行日志

见提交时的报告；真实命令输出不在此重复。
