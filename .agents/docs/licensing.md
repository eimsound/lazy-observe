# 依赖许可证

硬规则在根 `AGENTS.md`：禁止引入 AGPL/GPL 的生产或运行时依赖，白名单以检查脚本为准。本文补充范围、例外的边界和理由。

## 白名单与检查范围

| 侧 | 脚本 | 覆盖 | 允许 |
| --- | --- | --- | --- |
| Go | `scripts/check-go-licenses.sh`（`mise run license-check`） | 服务端与 `sdk/go` | Apache-2.0、MIT、BSD-2-Clause、BSD-3-Clause |
| JS | `scripts/check-js-licenses.sh` | `web`、`sdk/node`、`sdk/browser` | 上面四项，另加 ISC、0BSD、Unlicense |

ISC、0BSD、Unlicense 是宽松许可证，可以与 MIT 一起分发，所以 JS 侧放行。白名单外的许可证，先改 `AGENTS.md` 和对应脚本，再引入依赖。

两个脚本都只扫**生产**依赖。

## 开发期例外

### Air（GPL-3.0）

由 mise 管理的本地开发 CLI（`mise run dev` 用它重建并重启网关）。它只在开发者机器上作为独立进程运行：
不得链接进 Go 代码，不得打包进任何产品产物（二进制、容器镜像、发布归档）。

### MPL-2.0：目前有 `lightningcss` 与 `pathspec`

`lightningcss` 是 Vite 8 和 `@tailwindcss/vite` 的 CSS 处理器，`web` 已经通过 `@tailwindcss/vite` 使用它；`pathspec` 是文档静态站构建工具 Zensical（`tools/docs/pyproject.toml`）的传递依赖，仅用于文档静态站构建。

- 只能出现在 devDependencies 或构建工具的依赖树里（其中 `pathspec` 仅限文档静态站构建工具依赖树）。
- 不得进入任何生产或运行时依赖树，不得链接进 Go 二进制，不得打包进产品。
- 理由：MPL-2.0 是文件级 copyleft，约束的是 MPL 文件本身的修改和分发；它作为构建期工具产出的 CSS 或静态文档站点等构建结果不属于其衍生作品，
  所以不会把许可证义务带进产物。
- `scripts/check-js-licenses.sh` 只扫生产依赖，并继续拒绝生产依赖里的 MPL-2.0；`lightningcss` 与 `pathspec` 因为只在各自构建工具的依赖树里，不会进入生产或运行时产物。

新增 MPL-2.0 依赖：先确认它满足上面的边界，再把它写进 `AGENTS.md` 的例外里，不要直接放行。
