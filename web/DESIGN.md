# lazy-observe — Web UI 设计规范

本文件是 `web/` 控制台的 UI 密度、控件尺寸、视觉对齐与组件使用契约，覆盖所有页面、查询栏、工具栏与抽屉/弹窗。新建或修改界面前先读本文件。

## 1. 事实源分工

| 范围 | 单一事实源 |
| --- | --- |
| 颜色、暗色模式、色板、圆角（`--radius`）、根字号缩放（`--font-scale`）、图表/日志级别/Vitals 语义色 | `web/src/styles/globals.css`（用法见 `web/README.md`） |
| 控制台密度、控件高度阶梯、字号/图标配比、单/多行编辑器对齐与组件选型 | 本文（`web/DESIGN.md`） |
| 通用 Web 交互与可访问性审计（A11y、焦点态、表单、键盘、动画） | `.agents/skills/web-design-guidelines/SKILL.md` |
| Web 工程安全、状态码、隐私脱敏与跨项目隔离 | `.agents/skills/pitfalls/web.md`、`web/src/lib/project/architecture-guards.test.ts` |

不在本文重复维护色值或 CSS 变量清单；禁止在业务组件中硬编码颜色、圆角或像素级固定高度（使用 `rem` 工具类，确保 `--font-scale` 主题轴全站联动）。

## 2. 控件高度阶梯

lazy-observe 是高密度可观测性控制台，全站单行交互控件以 **紧凑基准档 `h-8`（`2rem` / `32px`）** 为默认高度：

| 档位 | 高度 | 适用范围 |
| --- | --- | --- |
| **基准档（默认）** | `h-8`（`2rem`） | 全站常规按钮（含主/次/轮廓/图标按钮 `size-8`）、单行 `Input`、`SelectTrigger`、`TabsList`、分段切换容器、组合按钮组（时间范围/刷新等）、单行态 CodeMirror 查询编辑器 |
| **微控件档** | `h-7`（`1.75rem`）或 `h-6`（`1.5rem`） | 表格行内操作、过滤条件 chip（如 `ExtraFiltersBar`）、弹窗/弹层局部微操作、嵌入 `h-8` 控件内部的清除/下拉触发图标 |
| **宽松档（例外）** | `h-9`（`2.25rem`）或 `h-10`（`2.5rem`） | 仅限独立认证页（登录、强制改密）的主表单输入与主提交按钮 |

硬约束：
- **同栏同高**：同一水平行内的全部独立控件（顶栏操作区、查询栏、筛选栏、结果卡头部的「分组/表格/JSON/实时」Tabs 与右侧操作按钮）必须处于同一高度档位。
- **原语收口**：尺寸阶梯集中在 `web/src/components/ui/` 定义；业务页禁止用 `className="h-9"`、`className="h-10"` 等临时覆写打破同栏高度一致。

## 3. 字号、图标与垂直居中

- **配比统一**：
  - `h-8` 基准控件统一采用 `text-xs`（`0.75rem`）配 `size-3.5`（`0.875rem`）图标、`gap-1.5`。
  - `h-7` 微控件统一采用 `text-xs` 配 `size-3.5`（或 `size-3`）图标。
  - 禁止同一工具栏混用 `text-sm` 与 `text-xs`，禁止大高度配小字与大图标（如 `h-9` + `text-xs` + `size-4`）。
- **垂直居中与边框盒一致**：
  - 按钮、`TabsTrigger`、分段项、`SelectTrigger` 使用 `inline-flex items-center justify-center`，以定高 + 对称内边距控制居中，不用会与边框挤压的过大垂直 `py-2`。
  - 所有单行控件采用 `box-border` 控制外高为精确 `2rem`；无边框按钮（`default`/`secondary`/`ghost`）通过 `border border-transparent` 与相邻 `outline` / `Input` / `SelectTrigger` 保持相同的内框高度和文字水平中线。
  - `TabsList` 与分段切换容器外高 `h-8`、内边距 `p-0.5`，内部 `TabsTrigger` / 选项按钮使用 `h-full` 撑满内高，保证文字与同栏外部按钮垂直对齐。
  - 中英文与数字混排（如 `当前时间`、`1h`、`50`）以及 `Switch` + `Label` 组合行，父容器统一 `items-center`，避免基线漂移导致文字视觉偏上或偏下。
- **组合按钮组**：紧邻的复合按钮组（如 `< 当前时间 >`、`- 1h +`、`刷新 | 关`）使用 `-ml-px` 叠合中间边框，并在聚焦态加 `focus-visible:z-10` 防止焦点环被相邻控件裁剪。

## 4. 代码与查询编辑器（CodeMirror）

遵守根 `AGENTS.md`：代码或查询语言输入（如 `LogsQLEditor`、`PromQLEditor`）优先使用 CodeMirror 6。

- **单行态对齐**：单行查询框外框最小高度严格等于基准控件高度（`2rem` / `32px`，`box-sizing: border-box`），禁止为单行查询框预设多行 `min-height`（如 `3.75rem`）。
- **首行中线对齐**：单行态下 `.cm-scroller` 行高与 `.cm-content` 上下内边距之和严格等于内框高度（如 `1px` 边框内高 `30px` = `line-height: 1.25rem` + 上下各 `0.3125rem` 内边距）；左侧前缀图标（`>_`）与右侧内嵌操作按钮定位在首行中线（`top-4 -translate-y-1/2`），使编辑器首行文本、图标与右侧执行按钮文字位于同一条水平中线。
- **多行态伸展**：用户输入换行（`Shift+Enter`）或语句自动折行时，编辑器高度随内容自然向下撑开（达到 `max-height` 后内部滚动）；编辑器所在行使用 `items-start`，右侧按钮保持与首行顶部对齐、不随编辑器拉伸变形。

## 5. 组件复用与页面结构

- **统一走 `web/src/components/ui/` 原语**：视图切换与分段切换统一使用 `Tabs` 或共享分段组件，禁止在路由页面手写裸 `<button>` 拼接自定义高度切换条。
- **布局与容器**：认证页主体统一用 `PageContainer` / `PageActions`（`web/src/components/layout/page.tsx`）；卡片内容区常规间距 `p-4`、`gap-3`/`gap-4`，避免多层卡片嵌套边框与阴影。
- **五态与工程约束**：数据路由页面统一走 `queryViewState` 五态渲染，表单与实体弹窗遵守 `web/src/lib/project/architecture-guards.test.ts` 与 `.agents/skills/pitfalls/web.md`。

## 6. 反模式与验收检查

避免以下回归：
- 同一行混用 `h-10` / `h-9` / `h-8` 或混用 `text-sm` / `text-xs`。
- 单行查询编辑器默认撑成多行高度，或前缀图标与首行文字中线错位。
- 业务页跳过 `components/ui/` 手写分段按钮、覆写控件高度或硬编码颜色/圆角。

> **现状说明**：本规范作为 `web/` 后续新建与存量治理的目标基线；既有页面中尚未收口的高度与对齐差异按后续专项 PR 逐步对齐，新增及修改过的页面须直接满足本规范。

改动 `web/` 界面后：
1. 对照本文检查控件高度、字号/图标配比、单/多行编辑器与同栏对齐；
2. 结合 `.agents/skills/web-design-guidelines/SKILL.md` 复核可访问性、焦点态、表单属性与键盘交互；
3. 按 `.agents/skills/do/SKILL.md` 验证矩阵运行 `mise run web`（纯文档变更无需跑构建）。
