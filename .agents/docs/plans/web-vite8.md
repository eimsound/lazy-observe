# web 升级到 Vite 8（rolldown）计划与记录

## 目标与来源

- 切片：`web/` 的 vite 从 6.4.3 升到 8.3.8，取代 Dependabot PR #79（#79 的分支不动）。先例：#43（browser SDK examples 升 Vite 8.3.8）。
- 完成条件：`web` 的 typecheck / lint / unit / build / Playwright 全绿，`mise run dist-cross` 与 `mise run gate-release-artifacts` 通过，
  首屏 JS 体积不比升级前差。
- 范围外：不改 `requirements.md`，不改应用代码，不加依赖（`vite` 自身升级带入的 `rolldown`、`lightningcss` 见「许可证」）。

## 根因（#79 为什么红）

Vite 8 用 rolldown 打包，`build.rollupOptions.output.manualChunks` 只剩函数形式，**对象形式不再支持**。
`web/vite.config.ts` 原来用对象形式（`uplot` / `replay-engine` / `promql` / `vendor` 四个 key）：

- `tsc --noEmit`：`vite.config.ts(21,11): TS2769 … 'uplot' does not exist in type 'ManualChunksFunction'`；
- 绕过 tsc 直接 `vite build`：`manualChunks is not a function`。

`rollupOptions` / `manualChunks` 在 Vite 8 里都是 deprecated 别名，推荐 `build.rolldownOptions.output.codeSplitting.groups`。
（`@tailwindcss/vite@4.3.5`、`@vitejs/plugin-react@5.2.0` 的 peer 都已含 `^8`，不用升。）

## 迁移方式

`web/vite.config.ts`：`build.rollupOptions` → `build.rolldownOptions.output.codeSplitting.groups`，每个旧 key 对应一个 group：

| 旧 key（Rollup 对象形式） | 新 group | 说明 |
| --- | --- | --- |
| `uplot: ["uplot"]` | `{ name: "uplot", test: packages("uplot") }` | |
| `"replay-engine": ["rrweb"]` | `{ name: "replay-engine", test: packages("rrweb") }` | |
| `promql: [@codemirror/*, @lezer/highlight, @prometheus-io/lezer-promql]` | `{ name: "promql", test: packages(...) }` | |
| `vendor: [react, react-dom, @tanstack/react-router, @tanstack/react-query]` | `{ name: "vendor", test: packages(...) }` | |
| （无，新增） | `{ name: "shell-vendor", test: /node_modules/, tags: ["$initial"] }` | 见下 |

语义核对（rolldown `CodeSplittingGroup` 的 d.ts 文档）：

- 旧对象形式 = 列出的包的入口模块**及其静态依赖**进同名 chunk，按 key 顺序先到先得。
  rolldown group 的 `includeDependenciesRecursively` 默认 `true`，同优先级时按数组下标先后，语义对得上，所以不用显式设 `priority`。
- `test` 用 `packages(...)` 生成的正则 ``node_modules[\\/](?:react|react-dom|…)[\\/]``：只命中列出的包本身，
  结尾的分隔符挡住 `react-i18next`、`@radix-ui/react-*`、`lucide-react` 等前缀同名包，开头锚定最内层 `node_modules`（pnpm 路径安全）。
  实测先用裸 `/node_modules[\\/]react/` 会把一堆 react-* 包吞进 vendor，所以不用。
- 不设 `minSize`：旧形式没有大小阈值。
- 额外的 `shell-vendor`：rolldown 没有 Rollup「入口已加载就把共享模块并回入口」的优化，会把入口和多个懒加载路由共享的模块拆成许多小公共 chunk
  （不加任何 group 时首屏要 16 个 JS 文件；只加前四个 group 时 11 个，index.html 里 11 条 modulepreload）。
  `tags: ["$initial"]`（入口静态依赖链上的模块）+ `test: /node_modules/` 把其余首屏 node_modules 代码合进一个 chunk，
  相当于旧 `index` 里那部分第三方代码；必须放在最后，前面的 group 先认领各自的包。
  与其它 group 的实测：`priority: -1` 会让它抢走 react 等包，同优先级放最后才符合预期，注释里写明了顺序。

唯一有意的语义差异：旧 `vendor` 里 `react-dom` 只含 `react-dom` 入口，`react-dom/client` 和 `scheduler`（约 185 kB）因为不在入口的依赖闭包里，
**意外落进了 `index`**；新 group 按包匹配，它们进 `vendor`，符合 key 的本意（「框架库」）。

另外：`package.json` 里 `pnpm.onlyBuiltDependencies: ["esbuild"]` 是 Vite 6 带 esbuild 的遗留；vite 8 不再依赖 esbuild
（锁文件里只剩 `vite` 的可选 peer），`pnpm install --frozen-lockfile` 去掉这段后仍干净，一并删除。

## 分包对比（`pnpm build`，main@8446801 vs 本切片）

大小为 raw / gzip（字节，Python `gzip -9` 重算，与 vite 日志的 gzip 数略有出入）。

| chunk | 位置 | 改前 | 改后 |
| --- | --- | --- | --- |
| `index`（应用壳） | 首屏 | 476,740 / 157,338 | 112,397 / 40,974 |
| `vendor` | 首屏 | 139,850 / 46,283 | 325,133 / 101,992 |
| `shell-vendor` | 首屏 | — | 153,305 / 49,915 |
| `rolldown-runtime` | 首屏 | — | 726 / 431 |
| `promql`（CodeMirror + lezer） | 懒加载 | 358,806 / 119,051 | 357,109 / 116,129 |
| `replay-engine`（rrweb） | 懒加载 | 280,179 / 87,156 | 271,073 / 81,963 |
| `uplot` | 懒加载 | 52,341 / 23,111 | 50,984 / 21,860 |
| `index-*.css` | 首屏 | 72,908 / 12,264 | 73,289 / 12,271 |
| rrweb CSS（`style-*` → `replay-engine-*.css`） | 懒加载 | 1,923 / 926 | 1,865 / 910 |
| uPlot CSS（`uplot-plugins-*.css` → 拆成 `uplot-*.css` + `uplot-plugins-*.css`） | 懒加载 | 2,860 / 1,128 | 1,644 / 689 + 1,198 / 564 |

首页加载（`dist/index.html` 的 `<script>` + `modulepreload` + `<link rel=stylesheet>`，再沿静态 import 取闭包）：

| | 改前 | 改后 | 变化 |
| --- | --- | --- | --- |
| 首屏 JS 文件数 | 2 | 4 | +2 |
| 首屏 JS raw | 616,590 | 591,561 | −25,029（−4.1%） |
| 首屏 JS gzip | 203,621 | 193,312 | −10,309（−5.1%） |
| 首屏 CSS | 1 个，72,908 / 12,264 | 1 个，73,289 / 12,271 | +381 / +7 |

全量产物：JS 136 个 2,009,644 / 680,631 → 152 个 2,011,475 / 672,328；CSS 3 个 → 4 个；`dist/assets` 共 139 → 156 个文件。

判断：

- 首屏变好：字节数略降（−4% raw / −5% gzip），首屏拆成 4 个文件（入口 + runtime 0.7 kB + `vendor` + `shell-vendor`），
  请求数多 2 个，对同源 go:embed 静态服务影响很小；好处是 `vendor`（react / tanstack）几乎不随发版变化，缓存更稳。
- 懒加载 chunk（promql / replay-engine / uplot）体积不变或略降，仍不进首屏。
- 差异：资产总数 +17（rolldown 的公共 chunk 切法与 Rollup 不同，多出若干小的公共 chunk）；uPlot / rrweb 的 CSS 现在跟随各自 chunk 单独输出，
  只在对应懒加载页面才取，对首屏无影响。
- 口径：首屏集合只含 `index.html` 静态可见的部分；路由级懒加载 chunk 的细粒度变化没有逐个比对。

## 其它 Vite 6 → 8 breaking change 的核对

- `build.target` 默认值：`'baseline-widely-available'` 在 vite 8.3.8 里解析为 chrome111 / edge111 / firefox114 / safari16.4 / ios16.4
  （vite 6 文档给的是 chrome107 / edge107 / firefox104 / safari16，未在本地实测）。`web/vite.config.ts` 没有显式 target，`tsconfig` target 是 ES2022；
  对管理后台这是可接受的浏览器下限抬高，不另设 target。
- Node 要求：vite 8 `engines.node` 为 `^20.19.0 || >=22.12.0`；`mise.toml` 钉的是 Node 26.13.1，CI 与 `Dockerfile` 都经 mise 装同一版本，满足。
- 插件：`@tailwindcss/vite@4.3.5`、`@vitejs/plugin-react@5.2.0` peer 含 `^8`，不升级，构建与 dev server 实测正常。
- vitest：web 不用 vitest，单测是 `node --test`，不受影响。
- `import.meta.glob` / `?raw` / `?url` / `?worker` / `optimizeDeps` / `esbuild` 配置：`web/src`、`web/e2e`、配置文件里 `rg` 都没有；
  只用 `import.meta.env.VITE_MOCK`，行为不变。
- CSS：Vite 8 默认用 lightningcss 压缩 CSS（vite 6 用 esbuild）；`index-*.css` +381 字节，Playwright 过，未见样式回归。
- dev server：`server.host/port/strictPort/proxy` 选项未变；Playwright 的 `webServer` 用 `pnpm dev`，30 例通过。

## 许可证

vite 8 把 `lightningcss`（MPL-2.0）变成直接依赖，只在 dev / 构建依赖树；`.agents/docs/licensing.md` 已登记该例外。
`scripts/check-js-licenses.sh web` 只扫生产依赖，通过。`rolldown`、`@oxc-project/types` 等为 MIT。

## 验证

见提交后的汇报；命令与结果（工作目录在本 worktree）：

- `mise run web`：lint、`node --test`（108 例）、build 通过；最后的 `playwright install --with-deps` 需要 sudo，本机无终端密码，
  失败（改宿主机环境，不绕过）。改为手动 `pnpm exec playwright install --only-shell chromium` 后 `CI=true pnpm test:e2e`：
  dev server 30/30 通过（1.5 min）；再用 `VITE_MOCK=1 vite build` + `vite preview` 对**生产构建**跑同一套 30 例，30/30 通过（50 s）。
- `bash scripts/check-js-licenses.sh web`：PASS。
- `mise run workflow-check`：通过。
- `mise run dist-cross`：通过（约 100 s）；`mise run gate-release-artifacts`：通过（amd64 110,571,682 B、arm64 100,991,138 B，均 < 150,000,000；
  空闲 RSS 439,574,528 B < 524,288,000；零配置 smoke 通过）。
