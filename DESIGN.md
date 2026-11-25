---
version: alpha
name: lazy-observe Console
description: >-
  高密度可观测性控制台（日志、指标、链路、错误、回放）的设计系统。
  Token 取默认主题（中性色板、亮色、--radius M、--font-scale 1）；
  运行时颜色与圆角以 web/src/styles/globals.css 的 CSS 变量为准。
colors:
  primary: "oklch(0.205 0 0)"
  on-primary: "oklch(0.985 0 0)"
  secondary: "oklch(0.97 0 0)"
  on-secondary: "oklch(0.205 0 0)"
  neutral: "oklch(1 0 0)"
  surface: "oklch(1 0 0)"
  on-surface: "oklch(0.145 0 0)"
  on-surface-variant: "oklch(0.556 0 0)"
  surface-muted: "oklch(0.97 0 0)"
  sidebar: "oklch(0.985 0 0)"
  outline: "oklch(0.922 0 0)"
  ring: "oklch(0.708 0 0)"
  error: "oklch(0.577 0.245 27.325)"
  on-error: "oklch(0.985 0 0)"
typography:
  headline-md:
    fontFamily: ui-sans-serif, system-ui, sans-serif
    fontSize: 1.125rem
    fontWeight: 600
    lineHeight: 1.75rem
  title-sm:
    fontFamily: ui-sans-serif, system-ui, sans-serif
    fontSize: 0.875rem
    fontWeight: 600
    lineHeight: 1.25rem
  body-md:
    fontFamily: ui-sans-serif, system-ui, sans-serif
    fontSize: 0.875rem
    fontWeight: 400
    lineHeight: 1.25rem
  body-sm:
    fontFamily: ui-sans-serif, system-ui, sans-serif
    fontSize: 0.75rem
    fontWeight: 400
    lineHeight: 1rem
  label-md:
    fontFamily: ui-sans-serif, system-ui, sans-serif
    fontSize: 0.75rem
    fontWeight: 500
    lineHeight: 1rem
  label-sm:
    fontFamily: ui-sans-serif, system-ui, sans-serif
    fontSize: 0.625rem
    fontWeight: 500
    lineHeight: 0.75rem
  code-md:
    fontFamily: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace
    fontSize: 0.8125rem
    fontWeight: 400
    lineHeight: 1.25rem
  code-sm:
    fontFamily: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace
    fontSize: 0.75rem
    fontWeight: 400
    lineHeight: 1rem
rounded:
  none: 0rem
  sm: 0.25rem
  md: 0.375rem
  lg: 0.5rem
  xl: 0.75rem
  full: 9999px
spacing:
  xs: 0.25rem
  sm: 0.5rem
  md: 0.75rem
  lg: 1rem
  xl: 1.5rem
  control: 2rem
  control-compact: 1.75rem
  control-relaxed: 2.25rem
  control-padding-x: 0.75rem
  icon: 0.875rem
  topbar: 3.5rem
components:
  button-primary:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.on-primary}"
    typography: "{typography.label-md}"
    rounded: "{rounded.md}"
    height: "{spacing.control}"
    padding: "{spacing.control-padding-x}"
  button-secondary:
    backgroundColor: "{colors.secondary}"
    textColor: "{colors.on-secondary}"
    typography: "{typography.label-md}"
    rounded: "{rounded.md}"
    height: "{spacing.control}"
    padding: "{spacing.control-padding-x}"
  button-outline:
    backgroundColor: "{colors.neutral}"
    textColor: "{colors.on-surface}"
    typography: "{typography.label-md}"
    rounded: "{rounded.md}"
    height: "{spacing.control}"
    padding: "{spacing.control-padding-x}"
  button-destructive:
    backgroundColor: "{colors.error}"
    textColor: "{colors.on-error}"
    typography: "{typography.label-md}"
    rounded: "{rounded.md}"
    height: "{spacing.control}"
    padding: "{spacing.control-padding-x}"
  button-icon:
    textColor: "{colors.on-surface}"
    rounded: "{rounded.md}"
    size: "{spacing.control}"
  button-compact:
    typography: "{typography.label-md}"
    rounded: "{rounded.md}"
    height: "{spacing.control-compact}"
    padding: "{spacing.sm}"
  input-field:
    backgroundColor: "{colors.neutral}"
    textColor: "{colors.on-surface}"
    typography: "{typography.body-sm}"
    rounded: "{rounded.md}"
    height: "{spacing.control}"
    padding: "{spacing.control-padding-x}"
  select-trigger:
    backgroundColor: "{colors.neutral}"
    textColor: "{colors.on-surface}"
    typography: "{typography.label-md}"
    rounded: "{rounded.md}"
    height: "{spacing.control}"
    padding: "{spacing.control-padding-x}"
  tabs-list:
    backgroundColor: "{colors.surface-muted}"
    textColor: "{colors.on-surface-variant}"
    rounded: "{rounded.lg}"
    height: "{spacing.control}"
    padding: 0.125rem
  tabs-trigger-active:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.on-surface}"
    typography: "{typography.label-md}"
    rounded: "{rounded.md}"
  query-editor-single-line:
    backgroundColor: "{colors.neutral}"
    textColor: "{colors.on-surface}"
    typography: "{typography.code-md}"
    rounded: "{rounded.md}"
    height: "{spacing.control}"
  card:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.on-surface}"
    rounded: "{rounded.xl}"
    padding: "{spacing.lg}"
  badge:
    backgroundColor: "{colors.secondary}"
    textColor: "{colors.on-secondary}"
    typography: "{typography.label-md}"
    rounded: "{rounded.md}"
  sidebar:
    backgroundColor: "{colors.sidebar}"
    textColor: "{colors.on-surface}"
  focus-ring:
    backgroundColor: "{colors.ring}"
  divider:
    backgroundColor: "{colors.outline}"
---

# lazy-observe Console

本文件按 [Google `DESIGN.md` 格式](https://github.com/google-labs-code/design.md)（`version: alpha`）编写：
front matter 是给工具和 agent 读的 token，正文说明这些值为什么存在、怎么用。
改 `web/` 下的页面、UI 组件或样式前先读本文件；改本文件后运行 `mise run design-lint`（CI 的 design job 同一条命令），
要求 0 error。`contrast-ratio` 警告只作可读性参考，不要求清零。

事实源分工：

- **运行时颜色、圆角、字号缩放**：`web/src/styles/globals.css` 的 CSS 变量（用法见 `web/README.md` 主题一节）。
  front matter 里的颜色只是默认主题（中性、亮色）的快照，两者冲突时以 `globals.css` 为准，并同步改回本文件。
- **控件尺寸、密度、对齐与组件选型**：本文件。
- **通用交互审查**（表单属性、动画）：`.agents/skills/web-design-guidelines/`。
- **Web 工程约束**（五态渲染、隐私脱敏、跨项目隔离）：`.agents/skills/pitfalls/web.md`、
  `web/src/lib/project/architecture-guards.test.ts`。

## Overview

lazy-observe 是给开发者和运维看的自托管可观测性控制台：日志、指标、链路、错误收件箱、会话回放和告警。
用户长时间盯着它排查线上问题，所以界面要**密、稳、安静**：

- **信息密度优先**。一屏放下查询栏、筛选、图表和结果表；留白服务于分组，不服务于氛围。
- **工具感，不是营销感**。中性灰底、单一强调色、细边框；没有渐变、插画、大圆角卡片堆叠。
- **对齐即可信度**。同一行控件同高、文字与图标在同一条水平中线；1–2px 的错位在密集工具栏里非常显眼。
- **主题可换，结构不变**。用户可在设置里切换明暗、7 套色板、5 档圆角、3 档字号，任何组合下布局都不能塌。

## Colors

默认主题是**中性灰阶 + 近黑主色**，所有颜色都以 OKLCH 表达，保证不同色板在感知亮度上一致。

- **Primary（近黑 `oklch(0.205 0 0)`）**：主操作按钮、选中态。切换色板（蓝、紫、翠绿、玫红、琥珀、青）时只替换
  `--primary`、`--ring` 和 `--chart-*`，其余中性色不动。
- **Secondary / Surface Muted（浅灰 `oklch(0.97 0 0)`）**：次级按钮、Tabs 底槽、hover 背景、Badge。
- **Neutral / Surface（纯白）**：页面主体和卡片。侧栏与顶栏用略灰的 `sidebar`，内容区以圆角白板嵌在其中。
- **On Surface（`oklch(0.145 0 0)`）/ On Surface Variant（`oklch(0.556 0 0)`）**：正文与次要文字（说明、时间戳、占位符）。
- **Outline（`oklch(0.922 0 0)`）**：边框、分隔线、输入框描边。
- **Error（`oklch(0.577 0.245 27.325)`）**：破坏性操作与错误提示。

另外三组**语义色有固定含义、不随色板变化**，只在 `globals.css` 维护，不进本文件 token：

- 日志级别 `--level-error/warn/info/debug/trace`；
- Web Vitals 评级 `--vital-good/ni/poor`；
- 图表序列 `--chart-1…5`，uPlot 通过 `useChartTheme()` 运行时读取。

暗色模式由 `<html class="dark">` 整组覆盖，不在组件里写 `dark:` 颜色分支。业务组件只用语义工具类
（`bg-background`、`text-muted-foreground`、`border-border`…）或 `var(--token)`，禁止硬编码色值。

## Typography

不加载 Web 字体：界面用系统无衬线栈（`ui-sans-serif, system-ui`），查询、日志、指标表达式用系统等宽栈
（`ui-monospace, SFMono-Regular, Menlo…`）。中英文与数字混排靠系统字体回落，不单独指定 CJK 字体。

- **Headline MD（1.125rem / 600）**：少量区块标题。页面本身不放大标题，路由名已在面包屑里。
- **Title SM（0.875rem / 600）**：卡片标题、抽屉标题。
- **Body MD（0.875rem / 400）**：正文、表格单元格、描述。
- **Body SM（0.75rem / 400）**：辅助说明、元数据、输入框文字。
- **Label MD（0.75rem / 500）**：所有基准档控件上的文字（按钮、Tabs、Select、Badge）。
- **Label SM（0.625rem / 500）**：编辑器内嵌标识等极小标签，慎用。
- **Code MD（0.8125rem 等宽）**：查询编辑器正文。
- **Code SM（0.75rem 等宽）**：日志行、JSON 视图、表格内等宽值。

全部尺寸用 `rem`。根字号由 `--font-scale`（0.9375 / 1 / 1.0625）缩放，所以任何写死 `px` 的字号或高度都会在字号切换时
与周围脱节。同一工具栏内不混用 `text-sm` 与 `text-xs`。

## Layout

整体是**固定侧栏 + 56px（`h-14`）顶栏 + 可滚动内容区**（`web/src/components/layout/app-shell.tsx`）。
内容区用 `PageContainer`（`p-6`），页面操作放 `PageActions`（右对齐、`gap-2`），页面不再重复标题。

间距以 Tailwind 的 4px 步进为准，常用档：

| Token | 值 | 用途 |
| --- | --- | --- |
| `xs` | 0.25rem | 图标与文字、组合控件内部 |
| `sm` | 0.5rem | 同一工具栏内控件之间 |
| `md` | 0.75rem | 卡片内元素之间、表单行之间 |
| `lg` | 1rem | 卡片内边距 |
| `xl` | 1.5rem | 页面内边距、页面区块之间 |

**控件高度阶梯**是本设计系统最重要的约定：

| 档位 | Token | 高度 | 用途 |
| --- | --- | --- | --- |
| 基准档（默认） | `control` | 2rem（`h-8`） | 所有常规单行控件：按钮、图标按钮（`size-8`）、`Input`、`SelectTrigger`、`TabsList`、分段切换、组合按钮组、单行态查询编辑器 |
| 紧凑档 | `control-compact` | 1.75rem（`h-7`） | 表格行内操作、筛选条件 chip、弹层内的局部微操作 |
| 宽松档 | `control-relaxed` | 2.25rem（`h-9`） | 仅登录、强制改密等独立认证页的主表单 |

- **同栏同高**：一条水平工具栏里所有独立控件处于同一档位。查询栏、筛选栏、结果卡头部的 Tabs 与右侧按钮都算同一栏。
- **对齐方式**：工具栏容器 `flex items-center gap-2`，允许 `flex-wrap`；窄屏换行后每行仍同高。
- **多行编辑器所在行**改用 `items-start`，右侧按钮对齐编辑器首行，不随编辑器拉伸。

## Elevation & Depth

层级主要靠**色阶与细边框**表达，阴影只做轻微提示：

- 侧栏与顶栏是 `sidebar` 灰底；内容区是带左上圆角（`rounded-tl-xl`）的白板，像一张嵌进外框的纸。
- 卡片：1px `outline` 边框 + `shadow-sm`。不要卡片套卡片；需要二级分组时用分隔线或 `surface-muted` 底色块。
- 浮层（Popover、Select 下拉、Dropdown）：`shadow-md`；模态框和抽屉：`shadow-lg`，配半透明遮罩。
- 焦点：`ring-2 ring-ring`，按钮额外 `ring-offset-2`。组合按钮组相邻边框用 `-ml-px` 叠合，聚焦项 `focus-visible:z-10`，
  防止焦点环被邻居遮住。

## Shapes

圆角由 `--radius` 一个变量驱动（用户可选 0 / S 0.3rem / M 0.5rem / L 0.75rem / XL 1rem），token 取默认 M：

- `rounded-md`（`--radius` − 2px）：按钮、输入框、Select、Tabs 选项、Badge——所有基准档控件。
- `rounded-lg`（`--radius`）：Tabs 底槽、分段切换容器。
- `rounded-xl`（`--radius` + 4px）：卡片、对话框、内容区白板。
- `rounded-sm`：对话框关闭按钮等极小元素。

只用这四个工具类，不写 `rounded-[6px]` 之类的固定值，否则圆角主题轴对它不生效。

## Components

组件原语统一在 `web/src/components/ui/`（shadcn 风格，Radix + Tailwind + `cva`）。尺寸阶梯在原语里定义，
业务页通过 `size` / `variant` 选择，不用 `className` 覆写高度。

### Buttons

- 变体：`default`（Primary）、`secondary`、`outline`、`ghost`、`destructive`、`link`。
- 基准档：`h-8`、`px-3`、Label MD、图标 `size-3.5`、`gap-1.5`。图标按钮 `size-8`。
- 紧凑档：`h-7`、`px-2`，文字与图标同基准档。
- 无边框变体（`default`/`secondary`/`ghost`）也带 `border border-transparent`，与相邻 `outline` 按钮内框等高、文字中线一致。
- 一个视图里最多一个 Primary 按钮（通常是「查询」「保存」）。

### Inputs & Selects

- `Input`、`SelectTrigger` 与基准档按钮同高（`h-8`），`px-3`，`box-border`，1px `outline` 描边。
- Select 下拉箭头 `size-3.5`、`opacity-50`；输入框内嵌清除按钮用紧凑尺寸并垂直居中。

### Tabs & Segmented Controls

- `TabsList` 外高 `h-8`、`p-0.5`、`rounded-lg`、`surface-muted` 底；`TabsTrigger` 用 `h-full` 撑满，选中项白底 + `shadow-sm`。
- 视图切换（分组/表格/JSON、构建器/代码）一律用 `Tabs` 或共享分段组件，不在路由页手写裸 `<button>` 拼切换条。

### Query Editors (CodeMirror)

代码和查询语言输入（`LogsQLEditor`、`PromQLEditor`）用 CodeMirror 6。

- **单行态**：外框高度等于基准档（`2rem`，`box-sizing: border-box`）。1px 边框内高 30px = 行高 1.25rem + 上下各 0.3125rem 内边距。
  不给 `.cm-content` 预设多行 `min-height`。
- **首行中线**：前缀图标和内嵌操作按钮定位在首行中线（`top-4 -translate-y-1/2`），与同栏按钮文字在同一水平线。
- **多行态**：换行（`Shift+Enter`）或自动折行时向下自然撑开，到 `max-height` 后内部滚动。

### Cards, Drawers & Dialogs

- 卡片 `rounded-xl`、1px 边框、`shadow-sm`；头部 `p-4` 带下分隔线，内容 `p-4`。
- 详情类内容（日志行、链路 span、错误事件）用右侧抽屉（`Sheet`），确认与表单用居中 `Dialog`（`p-6`、`max-w-lg`）。

### Badges & Chips

- Badge：`rounded-md`、`px-2 py-0.5`、Label MD；日志级别和 Vitals 评级用对应语义色。
- 筛选 chip 用紧凑档（`h-7`），可移除项右侧带 `size-3` 关闭图标。

### Data Tables & Charts

- 表格行高跟随紧凑密度，单元格 Body MD，等宽值用 Code SM；行内操作用紧凑档图标按钮。
- 图表（uPlot）颜色只取 `--chart-N`，主题变化后通过 `useChartTheme()` 重上色。

## Do's and Don'ts

- Do：同一工具栏只用一个高度档，默认 `h-8`。
- Do：控件文字用 `text-xs`，图标用 `size-3.5`，容器用 `inline-flex items-center`。
- Do：所有尺寸用 `rem` 工具类，让字号主题轴整体生效。
- Do：颜色、圆角只走语义工具类或 `var(--token)`。
- Do：单行查询编辑器与相邻按钮同高、首行文字与按钮文字同一中线。
- Do：改完 UI 用 `web-design-guidelines` 复查交互细节。
- Don't：在业务页写 `className="h-9"`、`h-10` 覆写原语高度。
- Don't：在同一行混用 `text-sm` 与 `text-xs`，或用大高度配小字大图标（`h-9` + `text-xs` + `size-4`）。
- Don't：为单行编辑器预设多行高度（如 `min-height: 3.75rem`）。
- Don't：硬编码 hex/oklch、`px` 字号或 `rounded-[Npx]`。
- Don't：卡片套卡片，或一个视图放多个 Primary 按钮。
- Don't：手写裸 `<button>` 分段条代替 `Tabs`。
- Do：尽量仅呈现基础视觉层与核心交互组件。若脱离辅助文本后视觉层级无法自洽，重新设计布局，而不是补一段说明。
- Don't：放常驻的辅助文本，即不属于数据、标签或操作名的解释性文字（卡片副标题、字段下方说明、引导段落）。
  确实需要的解释放进悬停提示，或链接到文档。报错信息不算辅助文本。只有三类例外：
  空状态（一句事实加一个动作）、不可逆操作的后果说明、一次性凭据提示。

## Known Gaps

本文件是目标基线，存量代码尚未全部收口（2026-11 盘点）：

- `Button` 默认 `h-9 text-sm`、`icon` 为 `size-9`；`Input`、`SelectTrigger`、`TabsList` 只有 `h-9` 一档，需先改原语。
- `/logs` 查询栏混用 `h-10`、`h-9`、`h-8`；`LogsQLEditor` 单行时 `min-height: 3.75rem`；`PromQLEditor` 为 `2.25rem`。
- `/metrics` 存在 `size="sm" className="h-9"` 覆写；`/issues` 等页面手写 `py-1.5` 分段按钮。

新增或修改过的页面直接满足本文件；存量页面按专项 PR 逐步对齐，届时同步删去本节对应条目。
