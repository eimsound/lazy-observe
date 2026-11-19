# JS/TS 工具链迁到 VoidZero / oxc / Rolldown 系：计划与记录

## 目标与来源

负责人原话：「把相关的东西都尝试迁移到 Vite/Rolldown 系，能迁移的话一个 PR 都涵盖」。承接 #85（web 升 Vite 8 / Rolldown）。
切片 `slice/voidzero-toolchain-1`：一个分支、一个 commit、一个 PR，涵盖下面四项。迁不动的不硬迁，记录原因与证据，不阻塞其它项。

完成条件：

- web：`typecheck`、`lint`、`pnpm test`（108 例）、`build`、Playwright（dev 与生产 preview 各一遍）、`scripts/check-js-licenses.sh web` 全绿；`web/dist` 与改前一致。
- SDK：四个包 build / typecheck / test 全绿，`dist/` 公开 API 与改前等价（脚本比对），`examples/*` 与 `mise run sdk-journeys` 用新产物跑通。
- 仓库：`mise run workflow-check`、`mise run ci-changes-test`、`mise run dist-cross`、`mise run gate-release-artifacts` 通过。
- 范围外：不改 `requirements.md`；不改应用行为；不引入格式化工具（见第 4 项）；CHANGELOG 另走 `chore/changelog-*` 分支。

## 版本选择（发布满 7 天，`minimumReleaseAge: 10080`）

2026-11-19 当天取数（`npm view <pkg> time`）：

| 包 | 采用 | 发布时间 | 说明 |
| --- | --- | --- | --- |
| `oxlint` | 1.93.0 | 2026-11-09 | 1.94.0（11-16）不满 7 天 |
| `tsdown` | 0.23.7 | 2026-11-06 | 带 `rolldown@1.3.2`、`rolldown-plugin-dts@0.30.2`（11-11） |
| `vitest`（web） | ^5.1.0（解析为 5.1.0） | 2026-11-02 | 与 `sdk/browser` 同主版本；5.1.1（11-18）不满 7 天 |
| `typescript` | ^7.0.2 | 2026-07-08 | 7.0.x 最新稳定版；7.1 目前只有 beta |
| `oxlint-tsgolint`（评估，未采用） | 7.0.2006 | 2026-11-05 | 7.0.2007（11-12）不满 7 天 |

许可证：oxlint、tsdown、rolldown、oxc 系、vitest 为 MIT；TypeScript 7 为 Apache-2.0。
`pnpm licenses list`（含 dev）在 web / sdk/node / sdk/browser 三处没有出现新的 GPL/AGPL/MPL；
web 与 sdk/browser 里原有的 `lightningcss`（MPL-2.0，`.agents/docs/licensing.md` 已登记）不变。生产依赖树没有变化，`check-js-licenses.sh` 通过。

---

## 1. web lint：ESLint → Oxlint

**现状**：`eslint 10.14.0` + `typescript-eslint 8.72.2` + `eslint-plugin-react-hooks 7.1.1` + `eslint-plugin-react-refresh 0.5.7` + `@eslint/js` + `globals`；
`web/eslint.config.js`：js recommended + tseslint recommended（无类型感知）+ react-refresh vite 预设 + `react-hooks/rules-of-hooks`、`exhaustive-deps` +
`no-unused-vars`（`^_` 豁免）+ `reportUnusedDisableDirectives: "error"`；`src` 里 9 处 `eslint-disable-next-line`。

**目标**：`oxlint` 配出等价规则集，删除全部 eslint 依赖与配置。

**做法**：

- `eslint --print-config src/main.tsx` 导出 ts 文件上实际生效的 72 条规则，逐条写进 `web/.oxlintrc.json`（`categories.correctness: off`，只开列出的规则，
  避免 Oxlint 的 correctness 分类带进 ESLint 时代没有的规则）。映射：`@typescript-eslint/*` → `typescript/*`；`react-hooks/*`、`react-refresh/only-export-components` →
  `react/rules-of-hooks`、`react/exhaustive-deps`、`react/only-export-components`（选项 `allowConstantExport`、`allowCompoundComponents` 照搬）；
  `@typescript-eslint/no-unused-vars` / `no-array-constructor` / `no-unused-expressions` → Oxlint 的同名基础规则（选项 `^_` 豁免照搬）。
  生效规则 71 条：`no-octal` 在 Oxlint 里没有对应规则，但 TS 与 Oxlint 的解析器本身就拒绝 `0755` 和 `"\08"`（TS1121、TS1487；Oxlint 报 deprecated octal），不构成缺口。
- `options.reportUnusedDisableDirectives: "error"` 对应原来的 `linterOptions.reportUnusedDisableDirectives`。
- 9 处 disable 注释改写为 Oxlint 原生写法：`oxlint-disable-next-line react/only-export-components`（7 处）、`oxlint-disable-next-line no-control-regex`（2 处）。
  （Oxlint 默认也认 `eslint-disable*`，但原生写法更直白，删除 ESLint 后不留误导。）
- `package.json`：`lint` 改为 `oxlint --max-warnings 0 .`；删除 `eslint`、`@eslint/js`、`eslint-plugin-react-hooks`、`eslint-plugin-react-refresh`、`globals`、`typescript-eslint`；删 `eslint.config.js`。
  `mise.toml` 的 web 任务一直是 `pnpm lint`，不用改。

**证据**（临时副本里跑，副本放 `~/.cache`，用完即删；规则映射后按规则名比较，行号与文案不比）：

1. 当前代码：ESLint `pnpm lint` 0 诊断（exit 0）；Oxlint `219 个文件、71 条规则、0 诊断`（exit 0）。
2. **真实代码变异**：取 `HEAD` 的 `web/src`，删掉 9 处 disable 注释，两边各跑一遍：
   ESLint 9 条（7 条 only-export-components + 2 条 no-control-regex），Oxlint 同样 9 条，文件与规则逐一对应。
3. **构造违规**（11 个文件，覆盖 ≥ 6 类）：

   | 类别 | ESLint | Oxlint |
   | --- | --- | --- |
   | 条件里调 hook（`rules-of-hooks`） | 报 | 报 |
   | 循环里调 hook、嵌套函数里调 hook、非组件函数里调 hook | 报 | 报 |
   | effect / useMemo / useCallback 漏依赖（`exhaustive-deps`） | 报 | 报 |
   | 多余依赖（outer scope 值）、`ref.current` 在 cleanup 里读、每次渲染都变的函数依赖 | 报 | 报 |
   | 非组件导出混在组件文件（`only-export-components`） | 报 | 报 |
   | 未使用变量 / 参数 / catch 绑定 / 函数；`_` 前缀豁免 | 报 | 报，豁免一致 |
   | 多余的 `eslint-disable` / `oxlint-disable` 注释 | 报 | 报 |
   | `no-explicit-any`、`ban-ts-comment`、`no-namespace`、`no-require-imports`、`no-this-alias`、`no-unsafe-function-type`、`no-wrapper-object-types`、`no-empty-object-type`、`no-array-constructor`、`no-non-null-asserted-optional-chain`、`no-duplicate-enum-values`、`no-unnecessary-type-constraint` | 报 | 报 |
   | `no-debugger`、`no-var`、`prefer-const`、`use-isnan`、`valid-typeof`、`no-prototype-builtins`、`no-case-declarations`、`no-fallthrough`、`no-duplicate-case`、`no-empty`、`no-control-regex`、`no-useless-escape`、`no-empty-character-class`、`no-unused-expressions`、`no-async-promise-executor`、`no-self-assign`、`no-dupe-else-if`、`no-constant-condition`、`no-unused-labels`、`no-unused-private-class-members`、`no-loss-of-precision`、`no-sparse-arrays` | 报 | 报 |

   合计 11 个构造文件逐文件对比：规则集合只有两处差异，都是 ESLint 报、Oxlint 不报：
   - **hook 在 `for` 循环里时，该 hook 的 `exhaustive-deps` 不报**（Oxlint 只报 `rules-of-hooks`）。这个写法本身就是 `rules-of-hooks` 错误，已被拦住，不影响门禁。
   - `no-useless-assignment`：我构造的用例（`const [_x, y] = …` 后面 `void y` 在 `return` 之后的不可达代码里）只有 ESLint 报；
     同一规则在 `let y = 2; y = 3;` 这类常规用例上两边都报（v10 文件），差异只在不可达代码的数据流分析。
   其余：诊断文案不同，行号一致。
4. 报错语义：未使用的 disable 注释在 Oxlint 里是 error 级（`Unused oxlint-disable directive (no problems were reported)`），退出码非 0；`--max-warnings 0` 同 ESLint。

**类型感知规则（tsgolint）评估**：装 `oxlint-tsgolint@7.0.2006`，`oxlint --type-aware` 在 `web` 上约 0.9 秒跑完。
试开 11 条常见规则的新增诊断数：`no-floating-promises` 127、`no-misused-promises` 21、`no-unnecessary-type-assertion` 21、`no-base-to-string` 9、`no-unsafe-assignment` 6、
`require-await` 3，其余（`await-thenable`、`no-for-in-array`、`no-implied-eval`、`only-throw-error`、`restrict-template-expressions`）0。
抽样：`no-floating-promises` 的 127 条里，`queryClient.invalidateQueries(...)` 18 条、`navigate({ to })` 一批、`node:test` 的 `test(...)` 一批——
都是 TanStack Query / Router / 测试框架的惯用写法，不是 bug；全部修掉要在 100+ 处加 `void`，是行为无关的大面积改动，超出本切片范围。
**结论：本 PR 不开类型感知规则**（`.oxlintrc.json` 与 `lint` 脚本都不带 `--type-aware`）。注意 pnpm 的 `auto-install-peers` 会把 `oxlint` 的可选 peer `oxlint-tsgolint` 一并装进锁文件（7.0.2006，MIT，原生二进制，满 7 天），所以依赖树里有它，只是不启用。后续若要开，建议单独切片，先开 `no-floating-promises`
并统一 `void` 约定，其余规则按噪声排序逐条放开。（此结论已被下面的后续切片取代。）
**后续切片（`slice/web-no-floating-promises-1`）：已开启 `typescript/no-floating-promises`**（负责人决定：命中处在调用前写 `void`，不靠白名单）。
- `web/package.json` 的 `lint` 改为 `oxlint --type-aware --max-warnings 0 .`；`oxlint-tsgolint` 精确 pin `7.0.2006` 成为直接 devDependency（MIT，2026-11-05 发布，满 7 天；只作为可选 peer 落在 `.pnpm` 下时 `oxlint --type-aware` 找不到它的可执行文件）。`.oxlintrc.json` 只新增这一条（error），其它类型感知规则一律不开；实测 `--type-aware` 不会隐式启用别的规则（探针里的 `no-misused-promises`、`no-base-to-string`、`require-await`、`no-for-in-array`、多余类型断言都不报）。
- 白名单：**没有配** `allowForKnownSafeCalls`。当前 `web/src`、`web/e2e` 只 import `vitest` 和 `@playwright/test`（没有 `node:test`，上面抽样里的那批 `test(...)` 已不存在），它们的 `test`/`describe`/`it`/各 hook 返回 void，探针实测不命中，没有需要放行的函数；`navigate`、`invalidateQueries` 等业务 API 一律不进白名单。`e2e/` 不在 `tsconfig.json` 的 `include` 里，但同样拿到类型信息、在检查范围内。探针里没 `await` 的 `page.goto`、`expect(locator).toBeVisible()`、`test.step`、`expect.poll`、`expect(fn).toPass()`、vitest 的 `vi.waitFor`、`expect(p).resolves` 都报——它们不等就是真漏 `await`，不能放进白名单；`test.describe.configure` 也不报。
  若以后某个测试框架函数开始返回 Promise，用 `{ "from": "package", "package": "<包名>", "name": ["<函数名>"] }` 加进 `allowForKnownSafeCalls`（实测 package specifier 能匹配方法名，例如 `@tanstack/query-core` 的 `invalidateQueries`）。
- 命中处理：25 处，全部是故意不等的 fire-and-forget（`queryClient.invalidateQueries` 18 处、`navigate` 7 处），在调用前加 `void`，行为不变；没有发现真正漏 `await` 的 bug。`void` 不会吞错：`invalidateQueries` 不传 `throwOnError` 时返回的 promise 不因单个查询失败而 reject（query-core 5.107.1 `refetchQueries` 注释）；客户端 `navigate` 返回的 `_commitPromise` 只有 resolve（router-core 1.171.39 `commitLocation`），加载错误走路由的错误组件。这些调用之后的代码都不依赖刷新或导航完成。
- 变异验证：去掉一处 `void` → lint 失败；临时把 `invalidateQueries` 放进白名单并去掉该处 `void` → 通过；白名单只放 `invalidateQueries` 时去掉一处 `navigate` 的 `void` 仍失败（测完已还原）。

其余类型感知规则（`no-misused-promises`、`no-unnecessary-type-assertion`、`no-base-to-string`、`no-unsafe-assignment`、`require-await`）仍未开，按噪声排序逐条放开。

**结论：迁了。** 规则等价（71 条逐条对应），差异见上两条，无门禁缺口。

---

## 2. SDK 构建：tsup → tsdown（四个包）

**现状**：`@lazy-observe/node`、`@lazy-observe/browser`、`@lazy-observe/react`、`@lazy-observe/bundler-plugin` 用 `tsup 9`（`tsup.config.ts`）输出 ESM + CJS + d.ts + d.cts + map；
tsup README 已声明不再积极维护、推荐 tsdown。

**目标**：四个包改用 `tsdown 0.23.7`，产物文件清单、`package.json` 入口、运行时导出、d.ts 公开符号、source map、external 与改前等价。

**做法**：`tsup.config.ts` → `tsdown.config.ts`，`build` 脚本 `tsup` → `tsdown`，tsconfig 的 `include` 同步改名；`devDependencies` 里 `tsup` → `tsdown`（精确版本，0.x 的 minor 就可能有 breaking）。

| 选项 | 取值 | 原因 |
| --- | --- | --- |
| `format` | `["esm", "cjs"]` | 不变 |
| `fixedExtension` | `false` | tsdown 在 `platform: node` 下默认输出 `.mjs` / `.cjs`；包的 `exports` 指向 `.js` / `.cjs`（`type: module`），必须关掉，才能保持文件名不变 |
| `platform` | node 包 `node`；browser / react 用 `neutral`；bundler-plugin `node` | tsdown 说明 `cjs` 固定面向 Node，`platform: "browser"` 对它无效（会告警），所以浏览器包用 `neutral` |
| `target` | browser / react `es2022`，bundler-plugin `node20`，node 包取 `engines`（`node22.15.0`） | 原值照搬；node 包 tsup 时未设，tsdown 自动取 `engines.node` |
| `external` → `deps.neverBundle` | browser：`rrweb`、`web-vitals`；react：`react`、`@lazy-observe/browser`；bundler-plugin：`unplugin` | `external` 在 tsdown 里已 deprecated |
| `deps.onlyBundle: []`（新增） | 四个包 | 白名单为空：`node_modules` 里任何包被打进产物就直接构建失败。比 tsup 时代只靠肉眼更严；负向实测：把 `rrweb` 强制打包，构建报 `rrweb is located in node_modules but is not included in deps.onlyBundle option` |
| `outputOptions.exports: "named"`（bundler-plugin） | | 入口同时有具名与 default 导出，rolldown 会发 `MIXED_EXPORTS` 警告；显式 `named` 与 tsup 的 CJS 形状一致（`exports.default` + 具名） |
| `outputOptions.strict: true`（四个包） | | rolldown 的默认值 `"auto"` 只沿用源码里写出来的 `"use strict"`；源码是 ESM（隐式严格），不写就让 CJS 产物退回非严格模式。tsup 的 CJS 四个包都带 `"use strict"`，显式打开保持一致 |
| `outputOptions.esModule: true`（node） | | rolldown 默认 `"if-default-prop"`，只在有 default 导出时加 `__esModule`；tsup（esbuild）时代 node 包的 CJS 有 `__esModule: true`，显式打开保持一致。browser / react 改前就没有（tsup 走 rollup 的 treeshake 路径），bundler-plugin 有 default 导出自动带上，均不设 |
| `clean`、`sourcemap`、`treeshake` | 不变 | |
| tsup 的 `splitting` | 删 | 单入口，无意义 |
| `dts` | `{ generator: "oxc" }` | 见第 3 项 |

**证据**：`sdk-dist-snapshot.mjs`（本目录 `voidzero-toolchain/`）对每个包的 `dist/` 出快照，迁移前后各一份比对：文件清单与大小、`package.json` 的 main/module/types/exports 与指向文件是否存在、
`node` 下 ESM `import()` 与 CJS `require()` 的导出名与 `typeof`（`exports` 里每个子路径、`import`/`require` 条件各探一次）、d.ts / d.cts 导出的符号与类型文本（TypeScript 5.9 checker，`strict`）、
每个 JS 的 `sourceMappingURL` 与 `.map` 可解析性、JS 与 d.ts 里引用的裸模块说明符。另有 `sdk-dts-equal.mjs`：对旧 d.ts 的每个导出生成「新旧类型互相可赋值」断言再用 tsc 编译
（带 private 成员的 class 是名义类型，改比声明文本）；负向实测（把 `dsn: string` 改成 `number`）会报错，说明它能抓到成员变化。

最终产物（tsdown + TypeScript 7.0.2 + oxc d.ts）对比改前（tsup + TypeScript 5.9.3）：

| 项 | node | browser | react | bundler-plugin |
| --- | --- | --- | --- | --- |
| dist 文件清单（6 个：`index.js/.cjs/.d.ts/.d.cts` + 2 个 `.map`） | 相同 | 相同 | 相同 | 相同 |
| `main/module/types/exports` 与指向文件 | 相同，无缺失 | 相同 | 相同 | 相同（含 `./vite`、`./webpack` 子路径） |
| ESM `import()` / CJS `require()` 导出名与类型 | 相同（各 5 个） | 相同（各 14 个） | 相同（各 2 个） | 相同（各 5 个，含 `default`；`./vite`、`./webpack` 子路径同） |
| d.ts / d.cts 符号与类型文本 | 相同 | 相同 | 相同 | 相同 |
| 互相可赋值检查（d.ts 与 d.cts） | 13 个导出，0 诊断 | 27 个（2 个 class 比文本），0 诊断 | 3 个，0 诊断 | 7 个，0 诊断 |
| source map | 都有、可解析、含 `sourcesContent` | 相同 | 相同 | 相同 |
| 裸模块说明符（external 是否一致） | 一致（仅内置模块加了 `node:` 前缀） | 完全一致（`rrweb`、`web-vitals`） | 完全一致（`react`、`@lazy-observe/browser`） | 一致（`unplugin` 仍 external；内置模块加 `node:` 前缀） |

有意/可接受的差异（逐项）：

- Node 内置模块引用从 `crypto` 变成 `node:crypto`（node、bundler-plugin 两个包）：`engines` 分别是 `>=22.15` 与 `node20` 目标，都支持。
- CJS 产物多了 `exports[Symbol.toStringTag] = "Module"`（rolldown 的命名空间标记），只影响 `Object.prototype.toString` 的结果；`require()` 得到的导出名、`default`、`__esModule` 与改前逐包一致（审查时用真实 `require` 复核）。
- source map 多了 `file` 字段；node 包 CJS 的 map 少列一个 `index.ts`（纯 re-export 的入口文件）。
- JS 内容随打包器变化：产物体积（字节，改前 → 改后）node `index.js` 45,162 → 43,113、browser `index.js` 38,213 → 36,034；react 的 CJS 多了 rolldown 的 `__toESM` 默认导入互操作 helper（1,398 → 2,308），语义不变。
- **d.ts 的顺带修复**：改前 `bundler-plugin` 的 `webpack` 导出类型引用了没有 import 的 `WebpackPluginInstance`（tsup 产物里悬空，靠 `skipLibCheck` 才不报）。现在 `vite` / `webpack` 显式标注为
  `UnpluginInstance<LazyObservePluginOptions>["vite" | "webpack"]`，解析后的函数类型与改前相同。

下游：`sdk/browser/examples/{react,vanilla}`、`sdk/node/examples/express` 用新产物 build / typecheck 通过；`bundler-plugin` 的 3 个测试另对 `dist/index.js`（ESM import）与 `dist/index.cjs`（require）各跑一遍，6 例通过；`mise run sdk-journeys`（go、node、browser）见「验证」。

**结论：迁了。**

---

## 3. TypeScript 7

**现状**：web 与 SDK 都是 TS 5.9.3；`.github/dependabot.yml` 对 `/sdk/node`、`/sdk/browser` 有 `typescript >=7.0.0` ignore（tsup 的 d.ts 生成调用 `ts.parseJsonConfigFileContent`，TS 7 npm 包没有 JS 编译器 API）；
web 被 typescript-eslint 的 peer `typescript <6.1.0` 卡住。

**目标**：1、2 完成后把 web 与 SDK 升到 TS 7.0.x，撤掉 dependabot 的 TS7 ignore。

**做法与结果**：

- **web**：去掉 typescript-eslint 后 `typescript@^7.0.2`，`tsc --noEmit` 通过，`tsconfig.json` **不需要任何改动**
  （没有用 `baseUrl`、`moduleResolution: node10`、`esModuleInterop: false` 之类 TS 7 移除/改默认的选项；主线已删除 `baseUrl`，`paths` 不依赖它；
  `types` 默认变为空数组也没影响，`vite.config.ts` 通过 `import "node:url"` 带入 `@types/node`，`vite-env.d.ts` 提供 `import.meta.env`）。
  负向验证：故意写 `const n: number = "x"` 与未使用局部变量，`tsc` 报 TS2322 / TS6133，exit 1。
- **SDK**：`typescript@^7.0.2`（workspace 根 devDependency），四个包 `tsc --noEmit` 通过。关键是 tsdown 的 d.ts 生成器。有三条路，实测如下：
  1. **tsc 生成器**：TS 7 没有 JS API，不可用。
  2. **tsgo 生成器**（`rolldown-plugin-dts` 在 TS 7 上的默认值）：能用，node / browser / react 三个包零改动通过；
     但打印 `The tsgo generator on TypeScript 7.0 is deprecated and will be removed in the next minor. Upgrade to TypeScript 7.1 or later`，7.1 目前只有 beta；
     且 `bundler-plugin` 失败：`vite` / `webpack` 导出靠推断类型，tsgo 把类型展开成 `unplugin` 内部引用，一路拖进 `postcss` 的 CommonJS d.ts（130 个错误，
     `uses CommonJS dts syntax. CommonJS dts modules cannot be bundled`）。
  3. **`isolatedDeclarations` + oxc 生成器**（采用）：`tsc --noEmit --isolatedDeclarations` 在 `src` 里一共只有 7 处要补显式类型（node 4、browser 3，bundler-plugin 用第 2 条里的显式标注已经满足，react 0）：
     `HTTP_SERVER_REQUEST_DURATION_BUCKETS_SECONDS: number[]`、`cpuProfileToPprof` / `heapProfileToPprof` 的 `start: Date`、`end: Date`、`MinuteRateLimiter` 的 `now: () => number`、
     `ReplayRingBuffer` 相关两处 `timestamp: number`。全部是纯类型标注，不改运行时；标注取的是原来推断出的类型，d.ts 不变（见第 2 项比对）。
     代价合理（7 行），所以不退到 TS 6。`dts: { generator: "oxc" }` 显式指定，不依赖 tsgo 的弃用路径，也不依赖 TypeScript 的 JS API，
     TS 5 / 6 / 7 下产物一致，构建更快（node 包整个 `tsdown` 约 1.0 s → 0.1 s）。
     负向验证：把 `rate-limit.ts` 的标注撤掉，构建报 `TS9007: Function must have an explicit return type annotation with --isolatedDeclarations`，不会静默放过。
- `dependabot.yml`：撤掉 `/sdk/node`、`/sdk/browser` 两处 `typescript >=7.0.0` ignore。
- `sdk/browser/examples/{react,vanilla}` 的 `typescript` 也从 `^5.9.0` 升到 `^7.0.2`（否则撤掉 ignore 后 Dependabot 会单独给 examples 提 TS 7）：`examples/react` 的 `tsc --noEmit` 与两个 example 的 `vite build` 都通过，`sdk/browser` 锁文件里只剩 `typescript@7.0.2`。

风险（写在这里，不是阻塞）：`rolldown-plugin-dts@0.30.2` 对 `typescript` 的 peer 写作 `^5 || ^6 || ~7.0.0 || ~7.1.0-0`；TypeScript 7.2 出来时若不在 peer 范围，pnpm 会报 peer 警告
（oxc 生成器本身不调用 TypeScript，构建不受影响，只是 `pnpm install` 的警告）。web 不受影响。

**结论：迁了（web 与 SDK 均 TS 7.0.2），dependabot 的 TS7 ignore 已撤。**

---

## 4. 评估项

### 4.1 web 单测：`node --test` → Vitest —— 迁了

迁移成本低：23 个测试文件全是 `import test from "node:test"` + `import assert from "node:assert/strict"`，一个测试 API（`test(name, fn)`）。
做法：23 处 `import test from "node:test"` → `import { test } from "vitest"`；断言仍是 `node:assert/strict`（408 处，不改）；
`vite.config.ts` 的 `defineConfig` 改从 `vitest/config` 引入，加 `test: { include: ["src/**/*.test.ts"], environment: "node" }`；`test` 脚本改为 `vitest run`。
证据：改前 `node --test`：23 个文件、108 例通过（旧脚本的三个 glob 有重叠，shell 展开 34 个参数，node 去重后实际也是 23 个文件）；改后 `vitest run`：23 个文件、108 例通过；`src/**/*.test.ts` 与旧 glob 的文件集合相同。
价值：与 `sdk/browser` 统一测试运行器、有 watch 模式；代价：多一个 devDependency（MIT）。`mise.toml` 的 web 任务仍是 `pnpm test`，不用改。
注释里提到 `node --test` 的两处（`token-usage.ts`、`project-scope.ts`）与两份 README / CONTRIBUTING 同步改。

### 4.2 格式化 —— 不引入

仓库没有 prettier / biome / dprint 配置，也没有 JS 格式化门禁（Go 侧用 `golines`，JS 侧没有）。没有现成门禁，就不引入新工具（Oxfmt 也一样）；若要引入，应作为单独的风格决策切片。

### 4.3 其它 Vite / Rolldown 系替换 —— 已查，其余不动

`rg` 查 `mise.toml`、各 `package.json` scripts、`.github`：

- `sdk/browser/examples/react` 的 `node --test test/*.test.mjs`（dev proxy 的小测试，在 example 里）：保留，不是被发布的包，迁到 Vitest 没有收益，也不在 `sdk-browser` 任务的测试范围内。
- 顺带补的缺口：`sdk/browser/package.json` 的 `test` 脚本原来只 `--filter` browser 与 react，`packages/bundler-plugin` 的 3 个测试不在 `mise run sdk-browser` / CI 里；现在加上 `--filter @lazy-observe/bundler-plugin`。
- `unplugin` 3 暂缓（`dependabot.yml` 的 `/sdk/browser` 加 `unplugin >=3.0.0` ignore）：unplugin 3 只发 ESM，发布的 `bundler-plugin` `dist/index.cjs` 会 `require("unplugin")`，Node <22.12 报 `ERR_REQUIRE_ESM`，且它把 engines 抬到 `^20.19.0 || >=22.12.0`；它的可选 peer 会把工作区的 vite → lightningcss（MPL-2.0）链进 `pnpm licenses list --prod`，许可证门禁失败。等迁移切片定下 engines、CJS 取舍与许可证门禁口径后撤掉（Dependabot #90 的审查结论）。
- `sdk/node` 的 vitest：Dependabot #76 已先合入（`^5.1.0`），本切片 rebase 后沿用；sdk/browser 的 jsdom 31（#78）同样先合入后沿用。
- Playwright（e2e）：不是 VoidZero 系，保留。
- `tsx` / `esbuild` / `rollup` 作为直接依赖：没有；`sdk/node` 的 vitest 5.1.0 仍经 `vite@7` 依赖 `esbuild`（pnpm 提示其构建脚本被忽略，改前就是这样）。
- 没有 `webpack`、`babel`、`jest`、`prettier`、`eslint`、`tsup` 的直接依赖残留（`rg` 查 `package.json`、lockfile 顶层、`mise.toml`、`.github`、脚本）。

---

## 验证

见汇报；命令与结果（工作目录在本 worktree，Node 26.13.1，pnpm 10.33.0）。

## 复现比对

```bash
# 迁移前（tsup）与迁移后（tsdown）各跑一次，再 diff JSON
SNAPSHOT_TS_DIR=<装了 typescript@5.9.x 的目录> \
  node .agents/docs/plans/voidzero-toolchain/sdk-dist-snapshot.mjs sdk/node/packages/node /tmp/x.json
SNAPSHOT_TS_DIR=… node .agents/docs/plans/voidzero-toolchain/sdk-dts-equal.mjs <旧 index.d.ts> <新 index.d.ts>
```

`sdk-dist-snapshot.mjs` 的 d.ts 比对用固定的 TypeScript 5.9 JS API（TS 7 没有），与被测包构建用的 TypeScript 版本无关。
