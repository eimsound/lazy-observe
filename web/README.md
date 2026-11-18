# lazy-observe — Web 前端

lazy-observe 一体化可观测性平台的生产前端。它采用侧栏 + 顶栏布局、shadcn/ui 组件语言、CSS 变量主题系统和中英双语 i18n，覆盖指标、日志、链路、Issue、Replay、性能与剖析、基础设施、告警、项目管理、AI Assistant 和 LLM 应用可观测性。所有页面都使用真实网关契约；`VITE_MOCK=1` 仅作为离线开发与显式状态演示层。

## 技术栈与关键决策

| 领域 | 选择 | 理由 |
|---|---|---|
| 构建 | **Vite 6 + React 19 + TypeScript** | 快、生态成熟。 |
| 路由 | **纯 Vite SPA + TanStack Router**（代码式路由） | 见下方「为什么不用 TanStack Start」。 |
| 数据请求 | **TanStack Query** | 查询缓存、加载/错误态、取消（AbortSignal）开箱即用。 |
| UI 组件 | **shadcn/ui**（手写进仓库）+ Radix + Tailwind v4 | 组件即代码、可完全受主题变量驱动；除布局骨架外不引入第二套组件库。 |
| 图表 | **uPlot 1.6** | 直接封装轻量 Canvas 时序渲染；通过 `useChartTheme` 消费同一套 CSS 变量。 |
| MetricsQL 编辑器 | **CodeMirror 6** + vmui 风格上下文补全 + Prometheus Lezer PromQL 0.313.1 | 指标、标签和值由项目隔离网关提供；Lezer 仅用于高亮、查询树和 Explain，不启用会误报 MetricsQL 扩展语法的 PromQL linter。 |
| i18n | **i18next + react-i18next** | 从第一行代码起中英双语，默认中文。 |
| 样式 | **Tailwind v4**（CSS-first，`@theme inline`） | 视觉参数天然收敛为 CSS 变量，契合主题系统架构要求。 |
| 包管理 | **pnpm** | — |

### 为什么是「纯 Vite SPA + TanStack Router」而不是 TanStack Start

需求允许二选一。本期选择 **纯 Vite SPA + TanStack Router**：

1. **部署形态匹配**：All-in-One 模式下前端产物由 Go 后端 `go:embed` 成静态资源、经 Fiber 网关同源提供（需求 §5.1）。这天然是一个静态 SPA 场景，不需要 TanStack Start 的 Node SSR/server 运行时——引入 SSR 反而多一层与 Go 网关重叠、且无法被 `go:embed` 的运行时依赖。
2. **成熟度与确定性**：Start 仍在快速演进；SPA + Router 组合稳定，contract test、`tsc` 与 production build 可确定性复现。
3. **可演进**：路由、Query、Table、Form 仍全部来自 TanStack 家族，未来若需要 SSR/流式，迁移到 Start 的成本集中在入口层，页面与数据层不动。

采用**代码式路由**（`src/router.tsx`）而非文件式，避免 codegen 步骤，保证 `tsc --noEmit` 自包含、可复现。

## 快速开始

```bash
# 从仓库根目录启动真实后端、VM 套件和 Vite HMR
mise run dev

# 开发（无后端，内置正弦波假数据）——推荐首次体验方式
VITE_MOCK=1 mise exec -- pnpm dev

# 开发（对接真实后端，vite 代理 /api -> http://localhost:3100）
mise exec -- pnpm dev

# 类型检查 / 生产构建
mise exec -- pnpm typecheck
mise exec -- pnpm build
```

- 无头浏览器冒烟测试：`mise run web` 会安装 Chromium headless shell，并在
  `VITE_MOCK=1` 下运行 Playwright。单独调试时可在本目录执行
  `mise exec -- pnpm test:e2e`；失败时会把 trace 与截图写入
  `test-results/`。
- 开发代理：`vite.config.ts` 将 `/api/*` 代理到 `http://localhost:3100`（可用 `VITE_API_TARGET` 覆盖）。生产同源，无需代理。
- 仓库根目录的 `mise run dev` 会并行启动 Air 后端和 Vite；开发页面由
  Vite 的 5173 端口独占提供，3100 端口只提供后端 API，根路径和前端路由
  返回 404。
- `VITE_MOCK=1`：API 层切换到内置 mock（`src/lib/api/mock.ts`），返回多序列正弦波数据，保证无后端也能开发与演示。
  - Mock 彩蛋 token：查询里包含 `__empty` → 触发空态；包含 `__error` → 触发错误态。
- 指标查询页按 Prometheus Expression Browser 的功能模型在本项目组件栈中重写；每个查询保持独立卡片、独立时间/分辨率/视图状态，并将非默认状态编码到 `gN.*` URL 参数（默认值不写入 URL）。编辑器使用 CodeMirror 6 和 vmui 风格的项目隔离 MetricsQL 补全，查询树和 Explain 使用官方 Apache-2.0 解析器，版权说明见 `THIRD_PARTY_LICENSES.md`。

## 目录结构

```
web/
├─ index.html              # 含「预上色」脚本：React 挂载前套用持久化的主题，避免闪烁
├─ vite.config.ts          # 插件、@ 别名、/api 代理、uPlot 分包
├─ src/
│  ├─ main.tsx             # 入口：装配 QueryClient / Theme / Sidebar / Tooltip / Router Provider
│  ├─ router.tsx           # 代码式路由表（根路由 = AppShell 布局，叶子 = 页面）
│  ├─ styles/globals.css   # ★ 主题单一事实源：全部 CSS 变量、色板预设、圆角档位
│  ├─ config/
│  │  └─ nav.ts            # 侧栏导航模型（分组 + 图标 + i18n key + 权限）
│  ├─ lib/
│  │  ├─ utils.ts          # cn() className 合并
│  │  ├─ settings.ts       # 当前项目（localStorage）+ IS_MOCK
│  │  ├─ api/              # ★ API 抽象层（见下）
│  │  │  ├─ client.ts      #   fetch 封装：session/project 优先、token 后备、统一 /api 与 /api/v1 错误处理
│  │  │  ├─ metrics.ts     #   queryRange()：调用 /query_range 并归一化为 TimeSeries
│  │  │  ├─ mock.ts        #   VITE_MOCK 假数据（正弦波多序列）
│  │  │  ├─ ranges.ts      #   时间范围预设（15m/1h/6h/24h）
│  │  │  └─ types.ts       #   Prometheus 响应类型 + 归一化 TimeSeries
│  │  ├─ i18n/             # ★ i18n（见下）：index.ts + locales/{zh,en}.ts
│  │  └─ theme/            # ★ 主题（见下）：theme-provider.tsx + presets.ts
│  ├─ hooks/
│  │  └─ use-sidebar.tsx   # 侧栏折叠状态（持久化）
│  ├─ components/
│  │  ├─ ui/               # shadcn 原语（button/input/card/select/popover/...）
│  │  ├─ layout/           # app-shell / sidebar / topbar / breadcrumbs / page
│  │  ├─ theme/            # theme-switcher（模式/色板/圆角面板）
│  │  ├─ i18n/             # language-switcher
│  │  └─ charts/           # uPlot 时序图、日志柱状图 + use-chart-theme（读 CSS 变量）
│  └─ routes/              # 全部产品页面；路由与项目/角色守卫集中在 router.tsx
```

## ★ 主题变量约定（新增页面**必须遵守**）

主题系统把「全部视觉参数收敛为 CSS 变量」，是架构级约束。四条正交轴，全部由 `ThemeProvider` 与 `index.html` 预上色脚本套用并持久化到 localStorage：

| 轴 | 载体 | 取值 | 存储 key |
|---|---|---|---|
| 模式 | `<html>` 上的 `.dark` 类 | 亮 / 暗 / 跟随系统 | `lo-theme-mode` |
| 色板 | `<html>` 上的 `data-palette` 属性 | 中性 / 蓝 / 紫 / 翠绿 / 玫红 / 琥珀 / 青色 | `lo-theme-palette` |
| 圆角 | `<html>` 上的内联 `--radius` 变量 | 0 / S / M / L / XL | `lo-theme-radius` |
| 字号 | `<html>` 上的内联 `--font-scale` 变量 | 小 / 中 / 大 | `lo-theme-font` |

**规则（写任何新页面/组件前先读这几条）：**

1. **禁止硬编码颜色 / 圆角。** 一律走语义 token：Tailwind 工具类（`bg-background`、`text-muted-foreground`、`border-border`、`rounded-lg`…）或 `var(--token)`。所有 token 定义在 `src/styles/globals.css`，并通过 `@theme inline` 映射为 Tailwind 颜色。
2. **图表颜色也走变量。** 图表用 `--chart-1`…`--chart-5`。任何图表组件通过 `useChartTheme()`（`src/components/charts/use-chart-theme.ts`）在运行时 `getComputedStyle` 读取这些变量并在主题变化时重读——**不要在图表里写死 hex**。新增序列色 = 加 `--chart-N`。
3. **新增色板 = 纯 CSS。** 在 `globals.css` 里加一段 `[data-palette="xxx"] { --primary/--ring/--chart-* ... }`（及可选 `.dark[data-palette="xxx"]`），再在 `src/lib/theme/presets.ts` 的 `PALETTES` 加一项。零组件改动——这就是「自定义主题即覆盖 CSS 变量集，无需构建」。
4. **只通过 `useTheme()` 改主题**（`setMode/setPalette/setRadius/setFont/reset`）。它负责套用 DOM + 持久化 + 触发 `version` 自增，图表据此重上色。绕过它直接改 class 不会触发图表刷新。

## ★ i18n 约定

- **默认中文**，中英双语从第一行代码起（需求 §1.3-4）。
- **所有 UI 文案走词典**，组件内**禁止**内联字面量。用 `const { t } = useTranslation()` + `t("group.key")`。
- 词典在 `src/lib/i18n/locales/`：`zh.ts` 是**事实源**；`en.ts` 用 `typeof zh` 静态约束——缺 key / 改名会在 `tsc` 报错，杜绝漏翻。
- 新增文案：先加到 `zh.ts`，再在 `en.ts` 补对应 key（否则 typecheck 失败）。带插值用 `t("metrics.seriesCount", { count })` 对应 `"{{count}} 条序列"`。
- 语言持久化到 localStorage（key `lo-language`），并同步 `<html lang>`。新增语言 = 加一个 `locales/xx.ts`（`typeof zh` 约束）并在 `src/lib/i18n/index.ts` 注册。

## ★ API 抽象层

所有后端访问收口到 `src/lib/api/`，好处：鉴权头、mock 切换、base path 各只有一处。

- **`client.ts` — 传输层。** `apiGet<T>(path, { params, signal })`：
  - 在**唯一**位置携带 `credentials: include`，并注入当前 `X-LO-Project`。
  - 支持 `/api` 与 `/api/v1` 两类真实端点：开发经 Vite 代理到 Fiber 网关，生产由 `go:embed` 同源提供，调用方无需感知。
  - 统一错误：抛 `ApiError`（携带后端 `error` 消息 + HTTP status）；正确透传 `AbortSignal`。
- **`metrics.ts` — 领域层。** `queryRange(params, signal)` 封装 `GET /api/v1/query_range?query=&start=&end=&step=`（Prometheus 标准契约，VM 兼容），把 matrix 响应归一化为 `TimeSeries[]`（值转数字，NaN/Inf → 断点）。
- **Mock 模式。** `IS_MOCK`（`VITE_MOCK=1`）时 `metrics.ts` 走 `mock.ts` 的正弦波数据，不发 HTTP；其余代码路径不变。
- **新增接口**：在 `api/` 下加一个领域文件，复用 `apiGet`；组件用 TanStack Query 的 `useQuery` 消费，天然拿到加载/错误/缓存/取消。

## 布局

`AppShell`（`components/layout/app-shell.tsx`）= 固定左侧栏 + 顶栏 + 可滚动内容区。

- **侧栏**（GitLab 风格）：分组导航、角色过滤、路由高亮、可折叠为 56px 图标栏（折叠态用 tooltip 显示标签），状态持久化。导航模型在 `config/nav.ts`。
- **顶栏**：面包屑、项目切换器、AI Assistant、语言切换、完整主题面板和用户菜单。
- **新增一个 section** = `config/nav.ts` 加一项 + `router.tsx` 加一条路由。

## ★ 日志流页（Logs）

`src/routes/logs-page.tsx` — 项目隔离的 LogsQL Explorer，覆盖普通查询、字段探索、统计图、分组/表格展示与实时 tail，视觉走本项目 shadcn + CSS 变量语言。

**结构**
- **查询栏**：CodeMirror 6 LogsQL 编辑器，结合字段/字段值/管道补全、查询历史、自动刷新、相对或绝对时间窗与可取消请求。
- **过滤器**：字段与 stream 元数据由 VictoriaLogs 项目隔离端点提供；支持正向、反向、正则过滤以及可序列化到 URL 的结构化过滤器。
- **图表**：`components/charts/logs-histogram.tsx` 使用 uPlot 展示 hits 或 `stats_query_range` 分组结果，支持 top-N、Other、堆叠、累计、步长和拖选时间范围。
- **结果视图**：列表、表格、分组和 live tail 四种视图；表格支持字段选择、列宽和分页，文本可按单行、Markdown、ANSI 等受控方式展示。
- **分页列表**：日志视图按页渲染（默认每页 100 行/组）；每行保留时间、level、stream、消息和 trace 跳转。
- **level 色点**：语义 token `--level-{error,warn,info,debug,trace}`（`globals.css` 定义、`@theme inline` 映射为 `bg-level-*`），亮/暗各一套值，见 `components/logs/level-indicator.tsx`。
- **详情抽屉**：`components/logs/log-detail-drawer.tsx`（`ui/sheet.tsx` + `ui/tabs.tsx`）。字段键值表（每值可「添加为过滤条件」回填查询栏、可复制）/ 原始 JSON 双视图；含 `trace_id` 时「查看链路」跳 `/traces/$traceID`。
- **跨页联动**：查询、时间窗、过滤器、图表和展示状态均写入 URL（等于默认值的项不写入）；从 Trace span 抽屉跳转时按 `trace_id` 预填并自动执行。

## ★ 链路检索/瀑布页（Traces）

Jaeger JSON 契约。检索页 `src/routes/traces-page.tsx`、详情页 `src/routes/trace-detail-page.tsx`（路由 `/traces/$traceID` 可直达分享），信息架构参考 Jaeger UI。

**结构**
- **检索区**：service 下拉（`/traces/services`）→ operation 下拉联动（`/traces/services/:svc/operations`）+ 时间范围 + limit + tag 过滤输入。
- **结果列表**：每行 = 根 span 名 · 根服务 · span 数 · 服务数 · trace id · 错误标记 · 总时长 · 时间；含相对时长条形微图。汇总由 `api/traces.ts#summarizeTrace` 计算。
- **瀑布图（自研）**：`components/traces/trace-waterfall.tsx`。见下「瀑布图实现思路」。
- **span 抽屉**：`components/traces/span-detail-drawer.tsx`——摘要（时长/占比/相对起点/spanID）+ tags 表 + process 表；「查看关联日志」按 `trace_id` 预填跳 Logs 页。

### 瀑布图实现思路（自研，非图表库）

布局模型收敛在 `src/lib/trace-tree.ts`（纯函数，无渲染）：

1. **建树**：`buildTree()` 从每个 span 的 `references`（`CHILD_OF`）还原父子，兄弟按 `startTime` 排序；支持多根（孤儿 span 作为独立根列出）。
2. **时间轴**：`bounds()` 求全 trace 的 `[start,end]`，每个 span 表达为 `offset = (span.start-traceStart)/total`、`width = span.duration/total` 两个 `[0,1]` 比例（`width` 下限 0.4% 保证极短 span 可见）。
3. **扁平化 + 折叠**：`flatten(collapsed)` 前序 DFS 产出带 `depth` 的可见行，跳过被折叠子树。
4. **着色**：`serviceColorIndex()` 给每个服务分配 `--chart-N`（1..5 循环）槽位；DOM 内联 `background: var(--chart-N)`——是 token 不是 hex，主题切换自动跟随。错误 span 用 `var(--level-error)`。

渲染层：左列为可折叠 span 树（按 `depth` 缩进、服务色点、错误 `AlertTriangle` 红标、操作名 + 服务名），右列为按比例定位的时间轴条形（`left/width` 用百分比），顶部标尺按 `total×{0,25,50,75,100}%` 标注时长；条形 hover（Radix Tooltip）显示时长与占总时长百分比；行点击开 span 抽屉，选中行高亮。**无任何硬编码色值**，全部走 `--chart-N` / `--level-*` / 语义 token。

## ★ API / mock 扩展

- **`api/logs.ts`**：通过统一表单传输访问 query、hits、`stats_query_range`、字段/stream 元数据、查询时间窗和 tail；NDJSON 支持增量 UTF-8 解码，所有有限请求正确透传取消信号。
- **`api/traces.ts`**：`getServices/getOperations/searchTraces/getTrace`，类型忠实 Jaeger（时间单位 μs）；`summarizeTrace/findRootSpan/isErrorSpan` 领域工具。
- **mock 层**（`VITE_MOCK=1`）：`mock-random.ts`（可复现 PRNG + hex id）、`mock-logs.ts`（多 stream、level 分布、~18% 带 trace_id，按 `trace_id` 过滤时全量命中）、`mock-traces.ts`（**纯 traceID 函数**生成 5–30 span、多层级、并行+串行、~30% 含错误路径的 Jaeger trace——保证 `getTrace(id)` 与检索列表结构一致、且日志的 trace_id 必可解析）。彩蛋 token：查询含 `__empty` → 空态、`__error` → 错误态。

## ★ 认证 / 会话守卫 / 身份（B1）

登录会话是浏览器默认身份来源；token 仍作为无会话后备保留。

- **传输层（`api/client.ts`）**：所有请求带 `credentials: "include"`（携带 httpOnly 会话 cookie）。项目范围由 `identityHeaders()` 单点注入为 `X-LO-Project: <slug>`。`apiSend()` 处理 JSON 体的 POST/PATCH/PUT/DELETE，`API_ROOT` 指向平台/鉴权端点 `/api`。
- **认证域（`api/auth.ts`）**：`getMe()`（`GET /api/auth/me`，401 归一化为 `null`）、`login()`、`logout()`。角色 `admin/member/viewer`。Mock 下用 localStorage 假会话：用户名含 `admin`→管理员、含 `viewer`→只读、其它→成员，密码任意。
- **全局状态（`lib/auth/auth-provider.tsx`）**：`AuthProvider` 用 TanStack Query 持有 `me` 资源，暴露 `useAuth() → { user, role, status: loading|anon|authed, login, logout }`。挂在 `main.tsx` 的 `QueryClientProvider` 内、`RouterProvider` 外。
- **路由守卫（`components/layout/app-layout.tsx`）**：无路径的 layout route 包裹所有应用页；`loading`→骨架、`anon`→`<Navigate to="/login" search={{redirect}}/>`（**仅取 pathname+search，且对 `/login` 短路**，避免重定向自引用循环）、`authed`→`ProjectProvider + AppShell`。登录页 `routes/login-page.tsx` 在 layout 之外独立渲染；登录成功后 `window.location.assign(redirect)` 回到原目标 URL。
- **权限（`lib/auth/permissions.ts`）**：`canManageProjects/Users`（admin）、`canManageAlerts`（admin|member）。页面据此隐藏/禁用操作并给 tooltip 提示；后端仍二次校验——这是 UX 而非安全边界。

## ★ 项目切换器 / X-LO-Project

- **`lib/project/project-provider.tsx`**：挂在已登录 layout 内，拉取 `/api/projects`，自动选中首个/持久化选择（`lib/settings.ts#get/setProjectSlug`，localStorage key `lo-project`）。切换项目 → 写 slug + `queryClient.invalidateQueries()` 让各数据页在新 scope 下重取。顶栏切换器（`topbar.tsx`）消费 `useProject()`；用户菜单显示用户名/角色/登出。

## ★ 平台管理页

- **项目（`routes/projects-page.tsx`，`/settings/projects`）**：项目列表 + 新建、重命名、删除（admin，内置自监控项目 `lazy-observe` 不可编辑/删除）；每项目「管理令牌」对话框——顶部标注「仅摄取（ingest only）」（ADR 0010：项目 Token 只上报遥测，查询/MCP/Grafana 用个人 Token），签发（token 值只在创建响应显示一次，含复制按钮与告警文案，并给出可复制的 DSN `<origin>/p/<slug>@<token>`）、吊销（`ConfirmDialog` 确认）。片段字符串由 `lib/tokens/token-usage.ts` 纯函数生成（`node --test` 覆盖）。
- **设置页个人 Token（`components/settings/user-api-tokens-card.tsx`，`/settings`）**：签发后除 token 值外，给出可复制的 MCP 命令 `lazy-observe mcp --dsn '<origin>/p/<slug>@<lou_token>'`（slug 取当前项目，缺省 `default`）与 Grafana Prometheus 数据源要点（网关根 URL + `Authorization: Bearer …`、`X-LO-Project: <slug>` 两个自定义请求头），口径以 `docs/deployment.md` 为准。
- **用户（`routes/users-page.tsx`，`/settings/users`，仅 admin）**：列表、新建（用户名/密码/角色）、行内改角色、删除；对「自己的账户」禁用改角色/删除。非 admin 直达时渲染「无访问权限」空态（侧栏该项也隐藏）。

## ★ 告警页组（A1 契约，侧栏「告警」分组）

- **事件（`alert-events-page.tsx`，`/alerts`）**：`GET /api/alerts?status=`；分段过滤（全部/触发中/已恢复）、severity 徽章、label chips、起止时间、活跃计数徽章、30s 轮询。
- **规则（`alert-rules-page.tsx`，`/alerts/rules`）**：`/api/alert-rules` CRUD。表单含 name/expr(PromQL Textarea)/for/severity/labels+annotations(`common/kv-editor.tsx`)/渠道多选/enabled 开关，带必填校验；列表行内 enabled 开关即时切换、编辑/删除。
- **静默（`silences-page.tsx`，`/alerts/silences`）**：`/api/silences` CRUD。matcher 编辑器（name/op `=`|`=~`/value 增删）、`datetime-local` 起止时间（本地时区 ↔ ISO）、备注；按 `ends_at` 分「生效中/已过期」两区，剩余/结束时长用 `format.ts#formatDurationShort`。
- **渠道（`channels-page.tsx`，`/alerts/channels`）**：`/api/channels` CRUD + `POST /:id/test`。5 类型（webhook/feishu/dingtalk/wecom/email）各有 `CHANNEL_FIELDS` 配置字段描述符驱动的动态表单；测试发送按钮内联反馈成功/失败（mock：无有效接收地址 → 失败态）。
- **共享**：`components/alerts/severity.tsx`（`SeverityBadge/StatusBadge/LabelChips`，severity 复用 `--level-*` 语义色）。

## ★ 总览 Dashboard（`overview-page.tsx`，`/`）

预置响应式卡片网格（`grid-cols-1 md:2 lg:3`），全部复用既有图表组件与领域 API：请求速率 / P95 时延 / 错误率（各一张 `queryRange` 迷你 `TimeSeriesChart` + KPI 值）、活跃告警数（`listAlerts("firing")` + severity 分解 + 跳 `/alerts`）、日志量直方图（`logsHits` + `LogsHistogramChart`，跨 2 列）、最近 3 条未解决 Issues（`listIssues` 按 `last_seen` 排序，覆盖加载/空/错误态并可跳详情）。整块共享单一时间窗（`resolveRange("1h")`）。

## ★ 问题追踪页组（Issues，Sentry 对标）

错误事件按 fingerprint 聚合为 Issue。列表页 `routes/issues-page.tsx`（`/issues`）、详情页 `routes/issue-detail-page.tsx`（`/issues/$issueId`，`?event=` 可深链某次事件——Replay 跳来时用）。

- **列表**：状态 tab（open/resolved/ignored）+ level 过滤 + 标题搜索（Enter 执行）+ 排序（last_seen/count）+ 分页。行含复选框、level 徽章、标题、首/末次相对时间、迷你趋势（`common/sparkline.tsx`，纯 SVG `currentColor`）、计数/用户数、行内状态操作。**乐观更新**：resolve/ignore/reopen 通过 `useMutation` 先把该行从当前 tab 移除、`onSettled` 再 `invalidateQueries`；支持批量（顶部动作条）。搜索含 `__empty`→空态、`__error`→错误态。
- **详情**：头部（level 徽章 + 标题 + culprit + 状态操作 + 计数卡）；**堆栈查看器**（`components/issues/stacktrace-viewer.tsx`）——函数名高亮、`file:line:col`、`in_app` 区分（系统帧灰显并可整体折叠）、含 `context_line` 的帧可展开；两份堆栈都在时提供 **raw/symbolicated 切换**。最近事件切换器（上一/下一）；关联跳转 `contexts.trace.trace_id → /traces/$traceID`、`session_id → /replays/$sessionId`；上下文卡（browser/os/url/user + tags chips）；事件原始 JSON 抽屉（`components/issues/event-json-drawer.tsx`）。
- **级别徽章**：`components/issues/issue-level-badge.tsx`，error/warning/info 复用 `--level-*` 语义色（`IssueLevelBadge` + `IssueLevelDot`）。

## ★ 会话回放页组（Replay，rrweb）

列表页 `routes/replays-page.tsx`（`/replays`）、播放页 `routes/replay-detail-page.tsx`（`/replays/$sessionId`）。

- **列表**：`has_error` 过滤开关、page_url、开始时间、时长估算（`last_seen_at - started_at`）、分片数、大小、错误徽章；分页。
- **播放器**（`components/replay/replay-player.tsx`）：**直接驱动 rrweb 的 `Replayer` 引擎**并自绘控制条（播放/暂停、进度轨、倍速、时间），而非用 `rrweb-player`——因为 rrweb-player 2.x 的 Svelte 组件在 React 中挂载时其内部 Replayer 无法实例化（空白帧、无报错）。自绘控制条全部走主题 token，**天然适配暗色**、无第三方硬编码白底黑字。录制画面按容器宽度 `transform: scale()` 自适应；`ResizeObserver` 跟随。
- **分片加载**：详情页按 seq **顺序拉取全部分片并拼接**为单一事件流后挂载播放器（`getReplaySegment` 真实模式解 gzip、mock 直接返回事件数组）。
- **错误标记**：进度轨上按 `(markerTime - startedAt)/durationMs` 定位的彩色 tick（`--level-*`）+ 下方标记列表；点击 tick/标题 `seek` 到该时刻，「查看问题」跳 `/issues/$issueId?event=`。标记来自该 session 的 Issue 事件（见 mock 交叉链接）。

## ★ 性能页（`performance-page.tsx`，`/performance`）

两个 tab，均复用既有 `queryRange`：

- **Web Vitals**：LCP/INP/CLS/TTFB/FCP 五卡（值 + 阈值配色 + 迷你趋势）+ 选中项 `TimeSeriesChart` 趋势 + 按页面聚合表。阈值 good/needs-improvement/poor 用新增语义色变量 **`--vital-good/--vital-ni/--vital-poor`**（`globals.css` 定义 + `@theme inline` 映射为 `text-vital-*`，亮/暗各一套）。评级逻辑在 `api/performance.ts#rateVital`。
- **后端接口**：`http.server.request.duration` 直方图派生 p50/p95/p99 + 吞吐 + 错误率，表格**可按列排序**；点击接口用 `endpointLatencyQuery()` 生成 `histogram_quantile` PromQL 跳 `/metrics?q=…&run=1` **预填并自动执行**（`metrics-page.tsx` 读 `useSearch` 挂载时执行一次）。

## ★ 持续剖析页（Profiling，`profiling-page.tsx`，`/profiling`）

对标 Pyroscope / Sentry Profiling 的火焰图页，消费 B9 剖析 API（端点在 `/api`，非 `/api/v1`）。信息架构走观测产品语言，**火焰图组件完全自研，不引入 d3 或任何图表库**。

**结构**
- **顶栏控件**：service 下拉（`/profiles/services`）、type 分段切换（cpu / heap / goroutine）、时间范围（复用 `api/ranges.ts` 的 `RANGE_PRESETS`；从 Trace 跳来时带显式 `start/end` → 呈现为「关联时段」自定义档）。刷新按钮重取当前窗口。
- **搜索栏**：匹配函数名的帧高亮（`ring`）、其余降饱和（`opacity`），并显示匹配计数；搜索对全树计数（`countMatches`），非仅可见节点。
- **剖析记录抽屉**（`components/profiling/profile-list-drawer.tsx`）：当前窗口 `listProfiles` 的记录（时间 / 类型 / 大小 + raw pprof 下载链接；mock 用自包含 `data:` URL，真实模式指向 `/api/profiles/:id/raw`）。
- **截断提示**：树含 `truncated` 节点时显示「已截断」提示条（`hasTruncated`）。

### 火焰图实现思路（自研，渲染策略与性能）

布局模型收敛在 `src/lib/flamegraph.ts`（**纯函数，无渲染**），组件在 `components/profiling/flamegraph.tsx`：

1. **经典 icicle**：根在上、深度向下，每节点一色块。`computeLayout(focusPath, query)` 把树扁平化为绝对定位行——宽度是**当前视图**的 `[0,1]` 比例，聚焦节点铺满 `[0,1]`，子树在其下按 `value` 降序**从左紧密排布**（父节点未被子节点占满的尾部即 self-time 空隙）。
2. **Zoom = 重新聚焦**：点击节点把它设为焦点铺满全宽（`focusPath` 追加），面包屑逐级返回、Home 回根。因坐标始终相对焦点重算，放大后原本极窄的帧自然重新展开。
3. **虚拟化裁剪（性能）**：视图宽度 `< 0.5%`（`MIN_WIDTH`）的兄弟节点折叠为单个「…」占位（`aggregated`），**永不渲染亚像素节点**——千级节点树只落地数百个 DOM 块；放大父节点后这些帧重新可见。
4. **着色走 CSS 变量**：`frameColor()` 把帧的**顶层模块名**（`moduleKey`：Go 路径取前三段，故同 repo 同色系）hash 到 `--chart-N`；runtime / syscall 帧走 `--muted` 灰阶。色块用 `color-mix(in oklab, var(--chart-N) …%, var(--card))` 淡染 + 同色左边框，**保证亮/暗双主题文字可读**，且主题/色板切换即时跟随（纯 CSS 变量，无 JS 重上色）。
5. **交互**：单一轻量 tooltip 跟随光标（函数名全称、样本数、占 total 与占父级百分比），避免给每个节点挂 Radix Tooltip 的开销；节点 `role=button` + Enter 可键盘放大。

### API / mock 扩展（剖析）

- **域文件 `api/profiles.ts`**：`getProfileServices / listProfiles / getFlamegraph / profileRawHref`（`ProfileType = cpu|heap|goroutine`，`FlameNode` 递归树），均 `IS_MOCK` 分支、`base=/api`。
- **mock `api/mock-profiles.ts`**：**纯 `(service, type)` 函数**生成确定性火焰图——根值 10000、五个顶层帧固定占比（46/24/14/11/5%，便于人工核对），深层用 seeded PRNG 展开；保证 **≥4 层、≥60 节点**，注入可搜索特征帧 `github.com/lazy-observe/profiler.collectFlameSamples`，并确定性标记一个深叶 `truncated` 以演示提示条。记录列表按窗口派生，raw 下载为自包含 `data:` URL。

## ★ API / mock 扩展（管理 + 告警）

- **域文件**：`api/projects.ts`、`api/users.ts`、`api/alerts.ts`——均 `IS_MOCK` 分支，走 `apiSend/apiGet`（base `/api`）。
- **mock 存储**：`api/mock-admin.ts`（项目/令牌/用户）与 `api/mock-alerts.ts`（规则/渠道/静默/事件）用模块级可变数组，seed 后在 tab 生命周期内可增删改，令满 tab 内 CRUD 全交互；事件列表由启用规则派生。假会话切换 admin/viewer 便于验收 RBAC。

## ★ API / mock 扩展（Issues + Replay + 性能）

- **域文件**：`api/issues.ts`（列表/详情/`PATCH` 状态/取事件原始 JSON，协议 v1 `ErrorEvent`）、`api/replay.ts`（会话列表/详情/分片，`RRWebEvent = @rrweb/types#eventWithTime`）、`api/performance.ts`（`VITALS` 元数据 + 阈值、`rateVital`、vitals/端点查询，真实模式按 series label 分组派生）。
- **交叉链接（产品灵魂）**：`api/mock-sessions.ts` 是 Issues↔Replay 的**共享 session 身份**（无依赖，避免环）——会话 id/窗口/是否含错误全部由 id 决定（纯函数）。`mock-issues.ts` 只给「最近一条事件」挂 `session_id`，时间落在该会话窗口内（按 issue 序号 0.2–0.8 分散），并挂 `trace_id`（trace mock 纯函数可解析）；`mock-replay.ts` 反向用 `errorEventsForSession()` 取该会话的 Issue 事件作为时间轴标记，位置与 Issue 事件时间严格一致。
- **rrweb mock**：`mock-replay.ts` **手写合法 rrweb 事件**——分片 0 = Meta + FullSnapshot（自包含小 DOM），其余分片 = 增量快照（鼠标移动、文本变更，错误时刻插入点击 + 错误横幅）。数值枚举含义见文件顶注；录制页 CSS 是「回放数据」（非应用 UI，`.ts` 文件、被 tsx 色值自查排除）。
- **性能 mock**：`mock-performance.ts` 按页面把 vitals 值刻意分布到 good/ni/poor 三档（阈值配色可见），端点表 p50<p95<p99 派生。
- **依赖**：新增 `rrweb`（回放引擎，MIT）+ `@rrweb/types`；`vite.config.ts` 把 `rrweb` 单独分包（仅回放页需要），与 uPlot 一致隔离出壳外。

## ★ 基础设施页组（侧栏「基础设施」分组）

主机监控与抓取目标管理，契约见 `api/hosts.ts` / `api/scrape-targets.ts`（均 `IS_MOCK` 分支，端点在 `/api`）。

- **主机页（`hosts-page.tsx`，`/infrastructure/hosts`）**：`GET /api/hosts` → 主机卡片网格。每卡：`hostname` + up/down/unknown 状态点、instance、CPU 核数、load（按 load/核分档着色）、内存/磁盘使用率**条形微图**；后端暂无样本时字段为 `null`，界面显示 `—`。点击 → 主机详情。
- **主机详情（`host-detail-page.tsx`，`/infrastructure/hosts/$instance`）**：主机基本信息 + CPU / 内存 / 磁盘 / 网络 / 负载 5 个指标图表。图表直接走既有 `queryRange` + `TimeSeriesChart`，查询窗口为最近 1 小时、60s step；不再依赖已移除的 dashboard 引擎。
- **抓取目标（`scrape-targets-page.tsx`，`/infrastructure/scrape-targets`）**：`/api/scrape-targets` CRUD + `GET /:id/health`。表格：name/url/interval、健康状态点（up/down/unknown，30s 轮询）带 `last_scrape_at` 相对时间、`enabled` 开关行内即时切换。新建/编辑对话框含 **URL 校验**（`new URL()` + http(s) 协议，非法时红框 + 内联提示）。**RBAC**：`canManageAlerts`（admin|member）才显示新建/编辑/删除与可用开关，viewer 只读并显示提示条。

## ★ 项目告警配置（并入设置页）

`components/settings/alert-config-card.tsx`，渲染于 `settings-page.tsx`，作用域为当前项目（`useProject().current`）。`GET/PUT /api/projects/:id/alert-config`（`api/alert-config.ts`）：`new_issue`/`regression` 开关、`error_rate_threshold_per_min` 数字输入、渠道多选（**复用 F2 `listChannels` 数据源**，chip 切换）。保存走 `useMutation`，`onSuccess` 写回 query 缓存并显示「已保存」反馈（1.5s）。

## ★ 主题系统收尾（§6）

在原「模式 / 色板 / 圆角」三轴上补齐，`theme-switcher.tsx` 由 Popover 升级为完整 **Sheet 面板**（模式三态 + 色板 + 圆角 + 字号 + 重置）：

- **第四轴 · 字号**：新增 `--font-scale` 内联变量（`presets.ts#FONT_SIZES` 小/中/大 = 0.9375/1/1.0625，存储键 `lo-theme-font`）；`globals.css` 用 `html { font-size: calc(100% * var(--font-scale)) }` 缩放根字号，**一处生效、全站 rem 尺寸跟随**。`ThemeProvider` 新增 `font/setFont` 与 `reset()`（四轴归默认），`index.html` 预上色脚本同步套用避免闪烁。
- **色板扩展到 7 套**（≥6）：在原 中性/蓝/紫/翠绿 上新增 **rose / amber / cyan**，全部 **OKLCH 手写**保持感知均匀、亮暗成对（`globals.css` 加 `[data-palette]` 块 + `.dark[data-palette]` 变体，`presets.ts#PALETTES` 加 swatch）——**零组件改动**，符合「自定义主题即覆盖 CSS 变量集」。
- **持久化**：四轴全部 localStorage 持久化，`useTheme()` 单点修改并触发图表重上色。

## ★ API / mock 扩展（基础设施 + 仪表盘 + 告警配置）

- **域文件**：`api/hosts.ts`（+ `usageRating/loadRating` 阈值分档）、`api/scrape-targets.ts`、`api/alert-config.ts`。
- **mock 存储**：`api/mock-infra.ts`（静态 5 主机刻意覆盖 good/ni/poor 三档且含一台 down；抓取目标可变数组 + CRUD + 由 id 派生的 up/down/unknown 健康；按项目 id 存告警配置）。mock 的 `query_range` 对任意 expr 返回正弦数据，故主机详情图表可渲染。

## ★ LLM 应用可观测性（`llm-observability-page.tsx`，`/llm`）

- **真实项目隔离 API**：`api/llm.ts` 通过共享 client 发送 `X-LO-Project` / token 身份，消费 `/api/llm/summary`、`/calls`、`/timeseries`；`llm-contract.ts` 在渲染前严格校验聚合、调用、RFC3339 时间、trace ID、耗时单位与趋势序列。项目切换会重置模型/服务筛选并触发新作用域查询。
- **面板**：调用数、输入/输出 Token、估算 USD 成本与价格覆盖 KPI；按模型/服务聚合；调用/Token/成本趋势；最近 100 条调用支持模型/服务筛选与 trace 跳转。汇总、趋势和列表分别提供真实加载/空/错误/截断状态。
- **Mock 同契约**：`mock-llm.ts` 生成确定性调用与成本，所有响应仍经过同一 normalizer；Mock trace ID 可进入真实 waterfall，并显示 GenAI 属性。
- **隐私边界**：trace drawer 优先识别当前 `gen_ai.provider.name`、结构化 input/output messages，同时兼容旧 `gen_ai.system` / prompt / completion。内容不另行获取且默认折叠，只展示当前已授权 trace 中、由 SDK 显式 opt-in 采集的数据。

## ★ AI 能力页组（Assistant 面板 · Issue AI 分析卡 · AI 设置）

后端 BYO AI 能力的前端交付。AI 默认关闭以保护隐私（BYO Key），全部界面对未启用态与无 Key 态有完整引导，且**绝不显示 API Key**。

### 形态选型：全局 Sheet 面板（而非独立 `/assistant` 页）

需求允许「顶栏入口开 Sheet/对话式面板」或「独立 `/assistant` 页」二选一，本任务选 **顶栏「AI 助手」按钮开右侧全局 Sheet 面板**：

1. **助手是上下文工具而非目的地**：用户通常是在某页（看某条指标/某个 Issue）时顺手发问，独立路由会打断当前视图、丢失上下文；全局 Sheet 可从**任意页**唤起且不改变路由。
2. **多轮会话天然是叠加层**：对话历史是跨页持久的会话态（存 `AssistantProvider` + localStorage），挂在 `AppShell` 内**只挂一次**，比每次进出路由重挂更贴合。
3. **与既有语言一致**：复用 `ui/sheet.tsx`，纯 token 驱动、暗色自适应，与日志/链路详情抽屉同一交互语言。

### 1. AI Assistant 面板（`components/ai/`，全局）

- **入口**：顶栏「AI 助手」按钮（`layout/topbar.tsx`）→ `useAssistant().setOpen(true)`。面板与 Provider 由 `layout/app-shell.tsx` 全局各挂一次。
- **状态分层**：`assistant-provider.tsx` 持有 `open` + 多轮 `turns`（发送、乐观追加 pending、完成/失败回填），最近 20 轮持久化到 localStorage（`lo-assistant-history`，丢弃 in-flight）；`assistant-panel.tsx` 是展示层。
- **一次问答**：自然语言 + 信号（自动/指标/日志）+ 时间范围 → `assistantQuery` → 展示①生成查询（代码块，**恒可复制**）②「在指标/日志页打开」（跳 `/metrics?q=&run=1` 或 `/logs?q=&range=` 预填并自动执行）③解释文字 ④内联结果：metrics 走既有 `TimeSeriesChart`（迷你 180px）、logs 取前 8 行（时间·level 色点·消息）。
- **异常态**：`execution_error` 显示查询 + 解释 + 错误提示（查询仍可复制）；429 显示限额文案；AI 未启用显示引导卡（指向 `/settings/ai`），输入禁用。
- **magic token**：问句含 `__error` → 触发 execution-error 演示态。

### 2. Issue AI 分析卡（`components/ai/issue-ai-analysis-card.tsx`）

- 最小侵入嵌入 `routes/issue-detail-page.tsx`（头部与事件切换器之间一张卡）。
- **未生成**（GET 404）→「AI 分析」按钮（member+，`permissions.ts#canUseAi`）；**生成中** loading；**已生成** → summary + `probable_causes`（confidence 用 `ai/confidence-badge.tsx`，复用 `--vital-{good,ni,poor}` 映射 high/medium/low）+ `suggested_actions`；显示 `分析于 <相对时间>`；**stale** 显示「有新事件」条 + 刷新按钮（POST `?refresh=1`）。
- **禁用/无权**：AI 未启用或 viewer → 按钮禁用 + tooltip 提示（不报错）。

### 3. AI 设置页（`routes/ai-settings-page.tsx`，`/settings/ai`，admin）

- 显示 `provider/model/enabled`（来自 `status`，**无 Key**）；「测试连接」→ POST test 显示延迟与回显；未启用时显示 config.yaml 启用步骤 + 隐私默认关闭说明。
- 非 admin 直达 → 「无访问权限」空态（侧栏该项 `adminOnly` 隐藏）。
- **Mock 开关**：仅 `IS_MOCK` 可见的 `Switch`，切换 `status.enabled` 两态以验收未启用引导（写 `lo-mock-ai-enabled`，切换后 `invalidateQueries(["ai-status"])` 令面板/卡片即时响应）。

### API / mock 扩展（AI）

- **域文件 `api/ai.ts`**（base `/api`）：`getAiStatus / testAi / assistantQuery / assistantExplain / getIssueAiAnalysis / generateIssueAiAnalysis`。assistant 的原始结果「与 query_range/logs 同构」，在**领域层**归一化为 `AssistantResult = {kind:"metrics",series} | {kind:"logs",rows}`，组件不感知线上形状；404 归一化为 `null`。
- **mock `api/mock-ai.ts`**：确定性——status 可切启用/禁用（持久化）；assistant 由问句 hash 派生稳定查询+解释+结果（metrics 复用 `mockQueryRange`、logs 复用 `mockQueryLogs`）；issue 分析按 id 生成一次并存内存，半数 id（`hash%2`）标记为 stale 以演示「刷新」流程，refresh 清除 stale；生成 prose 按当前语言产出（模型**输出**即数据，非 UI chrome）。禁用态下 test/query/generate 抛 409。
- **入口/权限**：`config/nav.ts` 管理组新增 AI 设置（`adminOnly`）；`permissions.ts#canUseAi`（admin|member）。

## 新增页面的自测约定

- **无硬编码色值/文案**：颜色只用语义 token / `--chart-N` / `--level-*` / `--vital-*`；文案全部进 `locales/zh.ts`（事实源）+ `locales/en.ts`（`typeof zh` 约束）。可 `grep -rE '#[0-9a-fA-F]{3,8}|rgba?\(' src --include='*.tsx'` 自查（命中应仅限 `use-chart-theme.ts` 的兜底值与 `presets.ts` 的色板 swatch）。
- 新增 UI 原语：`ui/sheet.tsx`（Radix Dialog 侧滑抽屉）、`ui/tabs.tsx`（Radix Tabs）、`ui/dialog.tsx`（Radix Dialog 居中模态，用于管理/告警表单与确认）、`ui/switch.tsx`（Radix Switch，enabled 开关）、`ui/textarea.tsx`（多行输入）——沿用既有 shadcn 写法，纯 token 驱动。复用组件：`common/confirm-dialog.tsx`、`common/empty-state.tsx`、`common/kv-editor.tsx`。
