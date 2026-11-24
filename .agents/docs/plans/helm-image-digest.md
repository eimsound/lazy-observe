# Helm `image.digest` 与发版回填实施计划

## 目标与来源

- 源需求：路线图条目「Helm `image.digest`，由发版流程回填自家镜像 digest」；切片任务说明
  `feat: Helm 支持 image.digest，发版产物回填自家镜像 digest`。
- 最终状态：
  1. `deploy/helm/lazy-observe/values.yaml` 新增 `image.digest`，模板在 digest 非空时按
     `<repository>[:<tag>]@<digest>` 渲染，格式非法（非 `sha256:` + 64 位小写 hex）直接 `fail`。
  2. `mise run helm-template-test` 覆盖四种情况：空 digest 用 tag、有 digest、格式错误 fail、
     tag 与 digest 同时存在。
  3. 发版产物（GitHub Release 资产）里带一份 digest 固定的 Helm values 片段，digest 来自
     `docker/build-push-action` 的 `outputs.digest`；发版流程不向 `main` 提交任何东西。
  4. `docs/deployment.md` 的 Helm 一节说明 `image.digest` 的用法与 digest 在发版产物里的来源。
- 范围外：把 chart 打包成 tgz/OCI 并发布（路线图另一条，见「未决事项」）；不 bump
  `Chart.yaml` 的 `version`（chart 尚未发布任何版本，没有需要兼容的已发布版本）；不加依赖
  （不引入 `yq`）；不改 `requirements.md`、`.agents/skills/**`、`CHANGELOG.md`（按 `do` 流程
  CHANGELOG 走独立的 `chore/changelog-<topic>` 切片）。

## 仓库证据

- chart 现状：`deploy/helm/lazy-observe/Chart.yaml` 为 `version: 0.1.0`、`appVersion: "latest"`；
  `templates/deployment.yaml:30` 只有一条镜像渲染
  `image: "{{ .Values.image.repository }}:{{ default .Chart.AppVersion .Values.image.tag }}"`，
  没有 digest 概念。
- **chart 目前不发布**：`.github/workflows/release.yml` 里没有 `helm package` / `helm push` /
  `ghcr.io/.../charts` 之类的步骤；`.goreleaser.yaml` 的 `archives` 只打二进制 tar.gz 与
  `checksums.txt`（`files:` 是 README/LICENSE/third_party 通知）。仓库内也没有 OCI chart 引用
  （`rg -n 'helm (package|push)|oci://' .github deploy scripts` 无命中）。
  → 「回填某个已发布 chart 的默认 values」这条不成立，只能把 digest 作为发版产物交出去。
- digest 来源：`release.yml` 的 `docker` job 用 `docker/build-push-action`（`id: build`，`push: true`），
  该 action 的 `outputs.digest` 就是推送后的多架构 index digest；现有最后一步
  `Verify pushed manifest` 已经在用 `${{ steps.build.outputs.digest }}`（`docker buildx imagetools
  inspect "ghcr.io/eimsound/lazy-observe@$DIGEST"`），所以 digest 在 job 内可用且已被信任。
- 版本号来源：同一 job 的 `docker/metadata-action`（`id: meta`）给出 `steps.meta.outputs.version`
  （`type=semver,pattern={{version}}`），即 chart 里要写的 `image.tag`。
- job 顺序：`container-check`（双架构冒烟 + Trivy）→ `performance-gates` → `archive`
  （GoReleaser 创建 GitHub Release）→ `docker`（推镜像）。`docker` 的 `needs: archive` 保证
  Release 已经存在，digest 也只有在 `docker` 里才产生，所以回填只能发生在 `docker` job 的末尾。
- 第三方镜像的既有约定：`scripts/e2e-multi-replica.sh:15-26` 用
  `postgres:16.16-trixie@sha256:...`、`valkey/valkey:8.1.11-trixie@sha256:...` 这种
  **tag + digest 同时写**的形态（PR #4 的钉法）。
- 工具面：`mise.toml` 没有 `yq`，仓库里也没有 `yq` 用法；chart 侧只需要 helm 自身。
- 提交纪律：`.agents/skills/do/SKILL.md`「执行契约」禁止直接向 `main` 推送。

## 实施策略

### 推荐路线 A（采用）：release 资产附带一份 digest 固定的 values 片段

- 形态：新增 `scripts/render-helm-release-values.sh`，从参数生成

  ```yaml
  image:
    repository: ghcr.io/eimsound/lazy-observe
    tag: "<version>"
    digest: "sha256:<64 hex>"
  ```

  的 values 片段；`release.yml` 的 `docker` job 在镜像推送后用 `steps.build.outputs.digest`
  调它生成 `dist/helm-values-lazy-observe-<version>.yaml`，再用 `gh release upload --clobber`
  挂到本次 Release 上。chart 继续从仓库目录（`deploy/helm/lazy-observe`）消费，用户
  `helm upgrade --install ... -f helm-values-lazy-observe-<version>.yaml` 就得到固定 digest 的部署。
- 为什么合适：digest 是构建产物，产生的时点晚于 Release 创建，用「资产」承接最自然；
  不需要写回仓库，因而不违反「不推 main」；脚本是纯函数（参数 → YAML），可以在本机用假
  digest 跑一遍，不依赖凭据与网络。

### 备选路线 B（否决）：发版流程把 digest 提交回 `main`

- 形态：`docker` job 生成 `values.yaml` 补丁，开 PR 或直推 `main`（提交里写 `image.digest`），
  下一个 chart 版本默认就带上一个镜像的 digest。
- 否决理由：① 直推 `main` 违反执行契约；开 PR 需要人工合并，发版流程会把「发布完成」卡在
  一次评审上，而且 `main` 的 chart 会指向**上一个**版本的镜像（每次发版都滞后一版），
  等于把一个不变量写成「回填」的假象；② digest 随每次重建变化，提交回仓库会产生纯噪声 diff；
  ③ 需要在发版 runner 上配写仓库的 token，扩大凭据面。

### 备选路线 C（否决）：写进 Release notes

- 形态：`gh release edit --notes` 把 digest 追加进说明正文。
- 否决理由：人可读但机器不可消费，`-f` 拉不动；而且和 GoReleaser 生成的 notes 抢夺同一个字段，
  容易在重跑时覆盖。

### tag 与 digest 同时存在时的渲染规则

沿用仓库里第三方镜像的既有约定（`scripts/e2e-multi-replica.sh`）：

| `image.tag` | `image.digest` | 渲染结果 |
| --- | --- | --- |
| 空 | 空 | `<repository>:<appVersion>`（保持今天的行为，`latest`） |
| 空 | 有 | `<repository>@<digest>`（不从空 tag 编出 `latest@digest` 这种误导写法） |
| 有 | 空 | `<repository>:<tag>`（今天的行为） |
| 有 | 有 | `<repository>:<tag>@<digest>`（README 里保留可读版本，Kubernetes 与容器运行时按 digest 拉取） |

### digest 校验

`^sha256:[0-9a-f]{64}$`。只接受小写 hex（registry 与 `docker buildx` 输出的规范形态），
不合法时 `fail` 并打印实际值，避免把 `sha256:` 前缀写错、位数写短、或用 `sha512:` 的拼写
悄悄退回按 tag 拉取（那正是这条路线图要消灭的「可变引用」）。

## 已确认的测试接缝

- `helm template` 渲染出的 Deployment 容器 `image:` 行（`templates/deployment.yaml`）：证明四种
  tag/digest 组合与渲染结果一致。
- `helm template` 的退出码与 stderr：证明非法 digest 让渲染失败且错误信息可定位。
- `scripts/render-helm-release-values.sh` 的命令行契约（`--repository/--tag/--digest/--output`，
  stdout 与退出码）：证明发版回填的输入输出，可在本机用假 digest 跑。
- `scripts/check-github-workflows.sh`（`mise run workflow-check`）：静态证明 `release.yml` 的
  `helm-values` job 确实从 `needs.docker.outputs.digest` 生成并上传 values 片段、`docker` job 保持
  `contents: read`——发版流程本身不能在本地真跑，这些静态断言是它唯一的门禁。

## 依赖顺序

1 → 2 → 3（chart 渲染链路）→ 4（chart 断言）→ 5（回填脚本）→ 6（release.yml 接线 +
workflow-check 断言）→ 7（文档）。

## 实施步骤

- [x] 1. `values.yaml` 增加 `image.digest: ""`，`_helpers.tpl` 增加 `lazy-observe.image` 模板
  - 交付：chart 接受并渲染 digest 引用；非法 digest 让渲染失败。
  - 阻塞于：无。
  - 涉及位置：`deploy/helm/lazy-observe/values.yaml`（`image:` 分组，含注释说明来源与优先级）、
    `deploy/helm/lazy-observe/templates/_helpers.tpl`（新 define `lazy-observe.image`）。
  - Red：`helm template test deploy/helm/lazy-observe --set image.digest=sha256:<64hex>`
    今天渲染 `image: "ghcr.io/eimsound/lazy-observe:latest"`（digest 被忽略），
    且 `--set image.digest=sha256:abc` 今天也能渲染成功。
  - Green：helper 按上表拼引用，digest 非法时 `fail`。
  - 验证：`mise run helm-lint`、`mise run helm-template-test`。
  - 验收：四种组合的 `image:` 行与表格一致；非法 digest 渲染失败并带 `image.digest` 字样。

- [x] 2. `templates/deployment.yaml` 改用 helper
  - 交付：Deployment 里的 `image:` 由 helper 产出，不再有第二份拼接逻辑。
  - 阻塞于：1。
  - Red：步骤 1 的 helper 若不被调用，渲染结果不变（断言在步骤 3 里落地）。
  - Green：`image: {{ include "lazy-observe.image" . | quote }}`（或等价写法）。

- [x] 3. `scripts/helm-template-test.sh` 补四种情况的断言
  - 交付：`mise run helm-template-test` 覆盖空 digest、有 digest、格式错误 fail、tag+digest。
  - 阻塞于：2。
  - Red：断言先加、实现后加时失败（本切片按 1→2→3 顺序落地，red 证据用「未改模板」的
    `helm template` 输出记录在下方执行日志）。
  - Green：不改脚本逻辑，只加断言。
  - 验证：`mise run helm-template-test`。

- [x] 4. 新增 `scripts/render-helm-release-values.sh`
  - 交付：参数 + 校验 + 生成 values 片段（`--output` 或 stdout）。
  - 阻塞于：1（片段里的字段名必须与 chart 一致）。
  - Red：脚本不存在时 `helm-template-test.sh` 的新断言失败（脚本缺失 / 生成的 `image:` 行不匹配）。
  - Green：实现 `--repository`（默认 `ghcr.io/eimsound/lazy-observe`）、`--tag`、`--digest`、
    `--output`，digest/tag 校验复用同一条正则；错误走 stderr、退出码 2。
  - 验证：`bash -n scripts/render-helm-release-values.sh`；假输入本地跑一遍；`helm-template-test.sh`。

- [x] 5. `release.yml` 新增 `helm-values` job，把 digest 回填到 Release 资产
  - 交付：tag 发布时 Release 上出现 `helm-values-lazy-observe-<version>.yaml`。
  - 阻塞于：4。
  - 涉及位置：`docker` job 只新增 `outputs`（`version`/`digest`，后者来自 `steps.build.outputs.digest`，
    前者来自 `steps.build-meta.outputs.version`，与 GoReleaser 的 `.Version` 同源），权限保持
    `contents: read`；新 job `helm-values`（`needs: docker`、`if: github.event_name == 'push'`）
    只给 `contents: write`，checkout 后调生成脚本，再用
    `gh release upload … --repo "$GITHUB_REPOSITORY" --clobber` 上传。
  - 为什么拆 job（最小权限）：`docker` job 会把 `GITHUB_TOKEN` 当 BuildKit secret 挂进构建
    （`secrets: github_token=…`），构建期代码不该同时拿到写仓库的权限。
  - Red：`mise run workflow-check` 的新断言在接线前失败。
  - Green：见上；断言按 job 定位（变异测试见执行日志）。
  - 验证：`mise run workflow-check`、`mise run release-snapshot-check`。

- [x] 6. `scripts/check-github-workflows.sh` 增加静态契约（按 job 定位）
  - 交付：`docker` job 变回 `contents: read`、不再上传资产；`helm-values` job 缺 `needs: docker`、
    `if: push`、`contents: write`、`GH_TOKEN`、`needs.docker.outputs.digest`、生成脚本、
    `gh release upload`、`--clobber`、`--repo "$GITHUB_REPOSITORY"` 任一项时 `workflow-check` 就失败。
  - 阻塞于：5。
  - 实现：新增 `release_job <file> <job>` 定位器 + 一段假 workflow 的自测，避免按全文子串误判。

- [x] 7. `docs/deployment.md` 的 Helm 一节补 `image.digest` 与发版来源
  - 交付：部署者知道怎么写 digest、digest 从哪个发版产物来、chart 本身不单独发布。
  - 同时提醒 `image.tag` 写成带引号的字符串（不写会被 YAML 当成数字，`0.10` 变 `0.1`）。

## 行为变化（需在 PR 正文里说清）

- `image.repository` 为空时现在渲染失败（`required "image.repository must not be empty"`）。
  改前空 repository 会渲染出 `:latest` 这种引用，Kubernetes 只会报 InvalidImageName。
- `image.digest` 非空但格式不对时渲染失败（改前该字段不存在，写错会被静默忽略）。
- 数值型 `image.tag`（YAML `tag: 0.1`、`--set image.tag=2`）现在经 `toString` 渲染成正常
  版本串；改前（本切片第一版）会输出 `%!s(float64=0.1)`，渲染出 InvalidImageName 而 helm 不报错。

## Green 后审查与重构

- [x] 镜像引用只有一处拼接（helper），`deployment.yaml` 不再直接拼 `repository:tag`。
- [x] 重跑 `mise run helm-lint`、`mise run helm-template-test`。

## 最终验证

- [x] `mise run helm-lint`
- [x] `mise run helm-template-test`
- [x] `mise run workflow-check`
- [x] `mise run ci-changes-test`
- [x] `mise run release-prerequisites-test`
- [x] `mise run release-publish-check`（HEAD 无 `v*` tag 时按设计失败，记录真实结果）
- [x] `bash -n scripts/helm-template-test.sh scripts/render-helm-release-values.sh scripts/check-github-workflows.sh`

## 未决事项

- chart 自身是否要发布成 OCI（`oci://ghcr.io/eimsound/charts`）或 tgz 资产。本切片只把
  digest 交出去，chart 仍从仓库目录消费；打包发布是另一条路线图条目，需要单独决策（会引入
  `helm package`/`helm push` 与 `Chart.yaml` 版本策略）。
- 是否要把 Release 资产里的 values 片段同时写进 Release notes（当前不写，避免与 GoReleaser
  生成的 notes 抢字段）。

## 执行日志

- 基线：`git log --oneline -3` → HEAD `b64a609`（origin/main），分支 `slice/helm-image-digest-1`。
- 执行时间：2026-11-18（UTC），helm 3.21.3（mise）。
- 现状：`rg -n 'helm (package|push)|oci://' .github deploy scripts` → 无命中（exit 1），确认 chart 未打包/未发布。
- 旧行为（red）：`git archive HEAD deploy/helm/lazy-observe | tar -x -C /tmp/origchart` 后用 helm 3.21.3 渲染：
  - `helm template test /tmp/origchart/deploy/helm/lazy-observe --set image.digest=sha256:<64hex>`
    → `image: "ghcr.io/eimsound/lazy-observe:latest"`（digest 被忽略）；
  - `helm template ... --set image.digest=sha256:abc` → 渲染成功（非法 digest 被忽略）。
  - `git cat-file -e HEAD:scripts/render-helm-release-values.sh` → absent at HEAD（脚本不存在）。
- 接线 red：对 `git show HEAD:.github/workflows/release.yml` 的 docker job 逐条匹配，`bash scripts/render-helm-release-values.sh`、
  `gh release upload`、`contents: write` 三条均 MISSING（`steps.build.outputs.digest` 已存在）。
- green：`mise run helm-lint` → `1 chart(s) linted, 0 chart(s) failed`（exit 0）。
- green：`mise run helm-template-test` → `PASS: Helm templates satisfy standalone-ha, clustered HA, and allinone assertions` +
  `PASS: Helm image references honour image.digest and the release values fragment`（exit 0）。
- green：`mise run workflow-check` → check-github-workflows 通过、`pipefail pipeline check: ok`、
  `PASS: ci-setup-mise …`、`PASS: E2E coverage …`、`PASS: ci-changes grouping rules`（exit 0）。
- green：`mise run ci-changes-test` → `PASS: ci-changes grouping rules`（exit 0）。
- green：`mise run release-prerequisites-test` → `PASS: release prerequisite checker accepts only safe snapshot and release inputs`（exit 0）。
- 跳过：`mise run release-publish-check` → 无 token 时 `release prerequisite check failed: GITHUB_TOKEN must be set and non-empty`（exit 1）；
  用假 token 时 `HEAD must have exactly one release tag; found none`（exit 1）。按设计需要真凭据 + `v*` tag，本地不能通过，
  改为用同脚本的 snapshot 模式 `mise run release-snapshot-check` 验证 GoReleaser 配置 → `PASS: snapshot release configuration is valid`（exit 0）。
- 脚本本地跑（假输入）：`bash scripts/render-helm-release-values.sh --tag 1.2.3 --digest sha256:<64×0>` → 输出 values 片段（repository/tag/digest）；
  `--digest sha256:abc`、`--digest SHA256:<64hex>`、缺 `--tag`、缺 `--digest` → 均 exit 2 并在 stderr 说明原因。
- `bash -n scripts/helm-template-test.sh scripts/render-helm-release-values.sh scripts/check-github-workflows.sh` → 通过。### 审查回合 1（H1）修复记录

- B1（数值型 `image.tag` 渲染出 `%!s(float64=0.1)` / `%!s(int64=2)`，helm 不报错）：
  - red：先加断言（`-f` 灌 `tag: 0.1`、`--set image.tag=2` + digest）→
    `mise run helm-template-test` 报 `FAIL: a numeric YAML image.tag must render as a plain version string`（exit 1）。
  - 修复前实测（HEAD `9e6c3f8`）：`image: "ghcr.io/eimsound/lazy-observe:%!s(float64=0.1)"`、
    `image: "ghcr.io/eimsound/lazy-observe:%!s(int64=2)"`、`…%!s(int64=2)@sha256:…`。
  - green：helper 改 `{{- $tag := toString (default .Chart.AppVersion .Values.image.tag) -}}` →
    `mise run helm-template-test` exit 0；`mise run helm-lint` exit 0。
- 权限拆分：`docker` 回 `contents: read`，只新增 `outputs.version`（`steps.build-meta.outputs.version`）
  与 `outputs.digest`（`steps.build.outputs.digest`）；新 job `helm-values`（`needs: docker`、
  `if: github.event_name == 'push'`、`contents: write`）checkout 后渲染并 `gh release upload … --repo "$GITHUB_REPOSITORY" --clobber`。
- 生成脚本：`--repository` 加校验（`^[a-z0-9]([a-z0-9./_-]*[a-z0-9])?$`，拒绝大写与首尾分隔符），
  三个字段输出时全部加引号；`helm-template-test.sh` 同时断言大写 / 前导 `/` / 尾随 `/` 三种仓库名被拒绝。
- `assert_contains`/`assert_not_contains` 改为 `grep -Eq -- "$pattern"`：新断言以 `--repository` 开头时
  不再被 grep 当成选项（首轮因此假红过一次）。
- 静态契约按 job 定位：新增 `release_job <file> <job>` + 假 workflow 自测（docker 块不会跑进 helm-values）。
  变异测试（备份→改→跑→还原，`release.yml` 与备份 `diff` 一致）：
  - `docker` 的 `contents: read` → `write` → `docker must stay contents: read (it mounts the token into the build)`（exit 1）；
  - 删 `--clobber` → `helm-values must pass --clobber so a re-run replaces the asset`（exit 1）；
  - `helm-values` 的 `needs: docker` → `needs: archive` → `helm-values must need docker so the digest exists before it runs`（exit 1）；
  - 删 `helm-values` 的 `if: github.event_name == 'push'` → `helm-values must only run on tag pushes …`（exit 1）；
  - 还原后 `bash scripts/check-github-workflows.sh` → 通过。
- 门禁（最终提交前在本地重跑）：`mise run helm-lint` 0、`mise run helm-template-test` 0、
  `mise run workflow-check` 0、`mise run ci-changes-test` 0、`mise run release-prerequisites-test` 0、
  `mise run release-snapshot-check` 0、`bash -n` 三条脚本 0。
