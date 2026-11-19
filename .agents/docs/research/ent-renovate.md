# Ent 生成代码、`entc.mod` 与 Renovate

> 状态：技术调研（实测记录），不是 ADR。环境：Go 1.26.11 linux/amd64，`entgo.io/ent` v0.14.6，2026-11-20。
> 所有实验在 `~/.cache/ent-exp/` 下的 git 克隆里做，没有改动主仓库。

## 结论

1. **`internal/db/ent` 入库**。Renovate 克隆仓库没有被 git 忽略的 `internal/db/ent`，根模块 `go get -t ./...`
   会失败（实测：33 个包里 10 个加载失败，`go mod tidy` 同样失败）。入库后 59 个包全部可加载，见「根模块升级实测」。
2. **去掉 `tools/entgen/main.go` 的 `//go:build ignore`、再用 `goGetDirs` 覆盖它，不能让 `entc.sum` 自动更新**，本切片不做。
   原因：Renovate 的 `go get` 跑在根 `go.mod` 上，只改 `go.mod`/`go.sum`，从不碰 `entc.mod`/`entc.sum`；
   去掉 build ignore 只会把 Atlas 等 12 个间接依赖灌进根 `go.mod`（见下）。
3. **`entc.mod` 升级 PR 由 agent 补 sum**，命令见「entc.mod 补 sum 的确切命令」（已实测）。
4. 如果以后想彻底省掉 `entc.mod`，可选方案是把生成器依赖并入根 `go.mod`（见「备选：并入根 go.mod」）。
   那是设计变更（放弃 `entc.mod` 的隔离），需负责人决定，本切片未做。

## 现状：为什么有 `entc.mod`

`scripts/generate-ent.sh` 在 `internal/db` 目录下执行
`go run -mod=readonly -modfile=<root>/entc.mod <root>/tools/entgen/main.go ./schema <work> <pkg>`。
`entc.mod` 与根模块同名，只 `require entgo.io/ent`，`entc.sum` 记录生成器（含 Atlas）的校验和。
目的是让 Atlas 迁移工具不进入生产 `go.mod`。`tools/entgen/main.go` 因 `//go:build ignore` 不属于根模块的任何包。

## 实验 1：去掉 `//go:build ignore`

在 HEAD 的克隆里删除 `main.go` 开头的 `//go:build ignore` 两行（`E2`）。

| 命令 | 结果 |
| --- | --- |
| `CGO_ENABLED=0 go build ./...` | 失败：`go: updates to go.mod needed; to update it: go mod tidy` |
| `CGO_ENABLED=0 go vet ./tools/...` | 同上失败 |
| `CGO_ENABLED=0 go mod tidy -diff` | 要给根 `go.mod` 加 12 个 `// indirect`（`ariga.io/atlas`、`go-openapi/inflect`、`hashicorp/hcl/v2`、`golang.org/x/tools v0.52.0`、`golang.org/x/mod v0.42.0` 等），`go.sum` 加 26 行 |
| `go mod tidy` 后 `go build ./...`、`go vet ./tools/...` | 通过 |
| `go mod tidy` 后 `golangci-lint run ./tools/...`（v2.12.2） | `0 issues.` |
| `go list -deps ./cmd/lazy-observe` 里有无 `ariga.io/atlas`、`entc` | 无（生产二进制依赖图不变） |
| `go list -deps ./...` | 含 `ariga.io/atlas/{schemahcl,sql/migrate,…}`（`./...` 现在包含 `tools/entgen`） |

后果：主模块 build/vet/lint 在 tidy 之后不受影响，但（a）根 `go.mod` 多出 Atlas 等依赖，生成器与 `entc.mod` 的版本被 MVS 分叉
（根里 `golang.org/x/tools` 被 MVS 抬到 v0.52.0，ent 自己的 `go.mod` 只要求 v0.30.0）；（b）`govulncheck ./...`、`go test ./...` 的范围多了 `tools/entgen`。

### Renovate 式升级 + `goGetDirs=./tools/entgen`

在 `E2`（已 tidy）里：`go get -t entgo.io/ent@master`、`go get -t ./tools/entgen`、`go mod tidy`。
结果：只有 `go.mod`、`go.sum` 变化（相对 HEAD 共 +14/−1、+30/−2，其中 12/26 行来自去 ignore 的 tidy），`entc.mod`/`entc.sum` 原样不动；
此后 `go run -mod=readonly -modfile=entc.mod ...` 仍然通过，因为它还在用旧的 `entc.mod`。
即：**根模块的 `go get` 对 `entc.sum` 没有任何作用**，与「goGetDirs 限定后 `go get` 成功、`go run -modfile=entc.mod` 缺 sum」的旧现象一致。
（Renovate 自身是否支持非 `go.mod` 命名的文件、是否会给 `go get` 加 `-modfile`，本次没有读它的源码或实跑，不下结论；
即使支持，`go get` 对重依赖变动也不够，见实验 2。）

## 实验 2：模拟 `entc.mod` 升级，看 `entc.sum` 能否补全

当前 v0.14.6 已是最新发布版，所以用两种方式模拟升级：

- **U1**：基线 = ent v0.14.5（根与 `entc.*` 都降到 v0.14.5 并重新生成、提交），升到 v0.14.6（轻量，依赖变动小）。
- **U2**：基线同上，升到 `entgo.io/ent@master`（= `v0.14.7-0.20261014065818-8037bc89bcab`，依赖变动大：`x/tools` v0.52.0、`x/sync` v0.24.0、`google/uuid` v1.6.0）。

| 步骤 | 观察 |
| --- | --- |
| U1：`go get -modfile=entc.mod entgo.io/ent@v0.14.6` 后 `scripts/generate-ent.sh`（`-mod=readonly`） | 通过（`go get` 自己写入了足够的 sum），但 `entc.sum` 留着 v0.14.5、旧 Atlas、旧 `x/mod` 共 6 行陈旧条目 |
| U2：`go get -modfile=entc.mod entgo.io/ent@master` 后 `-mod=readonly` | **失败**：`golang.org/x/tools@v0.52.0: missing go.sum entry for go.mod file`、`x/sync@v0.24.0` 同样缺 |
| HEAD 克隆 + master，`go mod tidy -modfile=entc.mod` | 不可用：`entc.mod` 被灌到 109 行（把根模块全部依赖当 tidy 目标，因为 `tools/entgen` 被 build ignore 排除在包集合外），随后 `-mod=readonly` 仍缺 `go-openapi/inflect` 的 sum |
| HEAD 克隆 + master，`go get -modfile=entc.mod entgo.io/ent/entc entgo.io/ent/entc/gen`（不带 `@版本`） | 不可用：不带 `@版本` 会把 ent 升到最新；sum 补齐了，但（只升 `entc.mod`、根 ent 还是 v0.14.6 的错位下）编译 `.entc` 临时程序报 `field.Descriptor has no field or method BlobCheckRefs`；在 v0.14.5 基线上构造 U1 起点时还观察到它给 `entc.mod` 加了 15 行 `// indirect` |
| HEAD 克隆 + master，`go mod download -modfile=entc.mod all` | 不可用：`entc.sum` +76 行，同样因版本错位在 `.entc` 编译失败 |
| **U2：`go run -mod=mod -modfile=entc.mod …`（与脚本同一条命令，只把 `-mod=readonly` 换成 `-mod=mod`）** | 通过，随后 `-mod=readonly` 通过；`entc.mod` 保持最小形态（只多一行 `github.com/google/uuid v1.6.0 // indirect`），`entc.sum` +10 行 |
| **U1、U3：同上，先 `rm entc.sum`**（U3 只用 `sed` 改 `entc.mod` 里的版本号） | 通过；`entc.mod`、`entc.sum` 与 HEAD 里 v0.14.6 的逐字节一致（`diff` 无输出），陈旧条目消失 |

陈旧条目的处理：先 `rm entc.sum` 再跑 `-mod=mod` 那条命令。U3 实测（只用 `sed` 把 `entc.mod` 里的版本号改成 v0.14.6、`rm entc.sum`）：
`-mod=readonly` 先失败（`missing go.sum entry for module providing package entgo.io/ent/entc/gen`），
`-mod=mod` 通过之后，`entc.mod` 与 `entc.sum` 与 HEAD 里的 v0.14.6 版本逐字节一致（`diff` 无输出）。

## entc.mod 补 sum 的确切命令

`entc.mod` 升级 PR（Renovate 或手工改了 `entc.mod` 的版本，且根 `go.mod` 的 `entgo.io/ent` 已同版本）里，由 agent 在仓库根目录执行：

```bash
rm entc.sum
work=$(mktemp -d -p ~/.cache)
(cd internal/db && env -u GOAMD64 -u GOARM64 CGO_ENABLED=0 \
  go run -mod=mod -modfile=../../entc.mod ../../tools/entgen/main.go ./schema "$work" \
  github.com/eimsound/lazy-observe/internal/db/ent)
rm -rf "$work"
mise run ent-generate        # -mod=readonly 重新生成，同时确认 sum 已补全
git add entc.mod entc.sum internal/db/ent go.mod go.sum
mise run ent-generate-check
```

要点：

- 版本必须与根 `go.mod` 的 `entgo.io/ent` 一致（生成的 `.entc` 临时程序按根模块的 ent 编译，错位会报 `has no field`，见 U2 的 master 失败）。
- 不要用 `go mod tidy -modfile=entc.mod`，它会把 `entc.mod` 膨胀成根模块的镜像。
- 第一条 `go run` 的输出目录是临时目录，只为触发依赖解析与补 sum；真正入库的代码由 `mise run ent-generate` 产出。
- U1/U2/U3 中，该命令都让随后的 `go run -mod=readonly` 通过；U3 证明结果与规范 `entc.sum` 完全相同。

## 备选：并入根 go.mod（未实施）

`E4`：在去掉 build ignore 且 tidy 后的克隆里，直接 `cd internal/db && go run -mod=readonly ../../tools/entgen ./schema <work> <pkg>`（只用根 `go.mod`），
退出码 0，生成结果与已入库 `internal/db/ent` 一致（`diff -r` 无差异，`migrate`/`enttest` 由钩子删除）。
这样可以删掉 `entc.mod`/`entc.sum`，生成器与运行时永远同版本，Renovate 的 `go get -t` 与 `go mod tidy` 就能覆盖全部校验和。
代价：根 `go.mod` 多 12 个间接依赖、`go.sum` 多 26 行；`go build ./...`、`govulncheck ./...`、`go test ./...` 的范围多了 `tools/entgen`（和 Atlas）；
放弃「Atlas 不得出现在生产 `go.mod`」的隔离（生产二进制依赖图本身不含 Atlas，已验证）。
还有一个没有试的方向：把 `tools/entgen` 做成嵌套模块（自带 `go.mod`，`replace` 回根模块），Renovate 可以原生管理，
但 `entc` 加载 schema 要在根模块上下文里编译临时程序，需要另行验证。

## 根模块升级实测（干净克隆）

方法：`git clone` 本 worktree 的 HEAD 到 `~/.cache/ent-exp/`，模拟 Renovate 的 `go get -t ./...`（先改 `go.mod` 版本号）和 `go get <dep>@<ver>`，再 `go mod tidy`。

**之前**（基线提交 `43bc7ae`，`internal/db/ent` 被忽略，克隆里不存在）：

```text
$ go list -e ./... | wc -l                      → 33
$ go list -e -f '{{if or .Error .DepsErrors}}…' ./... | wc -l → 10   （10 个包加载失败）
$ sed -i 's#jackc/pgx/v5 v5.10.0#jackc/pgx/v5 v5.13.0#' go.mod && go get -t ./...
    …/internal/db/ent/userapitoken: cannot find module providing package …   rc=1
$ go mod tidy
    …/internal/db/ent/userapitoken: no matching versions for query "latest"   rc=1
```

**之后**（HEAD，`internal/db/ent` 入库）：

| 依赖 | 命令 | 结果 |
| --- | --- | --- |
| `go list -e ./...` | — | 59 个包，加载错误 0 |
| `github.com/jackc/pgx/v5` v5.10.0 → v5.13.0 | 改 `go.mod` → `go get -t ./...` → `go mod tidy` | 均 rc=0；`go.mod` +2/−2、`go.sum` +4/−4（连带 `jackc/puddle/v2` v2.2.2 → v2.2.3）；`go list -e` 59 个包 0 错误；`go mod tidy -diff` rc=0；`CGO_ENABLED=0 go build ./...`、`go vet ./...` rc=0 |
| `github.com/alicebob/miniredis/v2`（测试依赖）v2.38.0 → v2.40.0 | `go get github.com/alicebob/miniredis/v2@v2.40.0` → `go mod tidy` | 均 rc=0；`go.mod` +1/−1、`go.sum` +2/−2；`go mod tidy -diff` rc=0；`go build ./...`、`go vet ./...` rc=0 |
| 空操作 | `go get -t ./...` | rc=0，工作区无改动 |

`go get` 打印的 `module github.com/gofiber/fiber/v2 is deprecated` 只是提示，与本切片无关。
根模块的 `entgo.io/ent` 升级（Renovate 的 `ent` 分组）仍要同批升 `entc.mod`，按上一节命令补 `entc.sum` 并重新生成。
