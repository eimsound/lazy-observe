# 设计工具链（DESIGN.md、impeccable、Playwright MCP）

给增删、升级或配置前端设计工具的 agent 看。日常改 UI 只需要 `AGENTS.md` 那一行。

## DESIGN.md lint

- 命令：`mise run design-lint`（`designmd lint DESIGN.md`，只有 error 才失败；warning 与 info 照常输出）。
- CLI：`mise.toml` 的 `npm:@google/design.md`（Apache-2.0，lazy 工具）。升级时改这一处版本，发布满 7 天再升（`scripts/release-age.env`）。
- CI：`scripts/ci-changes.sh` 只在根目录 `DESIGN.md` 变化（或全量）时打开 `design` 标志，`ci.yml` 的 `design` job 跑同一条 mise task；其它 `*.md` 不触发。

## impeccable 技能

- 位置：`.agents/skills/impeccable/`，Claude Code 经 `.claude/skills` 软链读到，agy 直接读 `.agents/skills/`。
- impeccable 命令一律在仓库根目录运行：关闭遥测的 `.impeccable/config.json` 只在 cwd 是仓库根目录时被读到。
- 产品上下文：`PRODUCT.md` 由 `/impeccable init` 生成；`DESIGN.md` 已手写，不要用 `/impeccable document` 覆盖，改它按 `DESIGN.md` 自身约定手改。

## Playwright MCP

- 两份配置参数保持一致：Claude Code 读根目录 `.mcp.json`，agy 读仓库内插件 `.agents/plugins/playwright/`（`plugin.json` + `mcp_config.json`，会话里工具名前缀为 `playwright_playwright`）。升级时两处版本一起改，版本发布满 7 天。
- 参数：`--browser chromium --headless --isolated --snapshot-boxes`。chromium 用 Playwright 自带浏览器（缓存在 `~/.cache/ms-playwright`，本机没有时先 `npx playwright install chromium`）；`--snapshot-boxes` 让快照带每个元素的 `[box=x,y,宽,高]`，可以直接比较同一栏控件高度。快照输出在 `.playwright-mcp/`（已 git 忽略）。
- Claude Code 对 `.mcp.json` 的批准来自 `.claude/settings.json` 的 `enabledMcpjsonServers`，只在受信任的仓库里生效。信任记录按主仓库路径存放，所有 git worktree 共用：在主仓库目录运行一次 `claude` 接受信任即可，不用每个 worktree 各接受一次；只信任 worktree 的上级目录不算数。
- 被测页面：`mise run dev-web`（或 `VITE_MOCK=1` 离线 mock）起本地前端，再让 MCP 打开对应地址。
