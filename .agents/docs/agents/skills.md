# 上游 skill 的维护

`.agents/skills/` 下除 `do`、`main`、`spec-to-plan` 外，都是 `mattpocock/skills`（MIT，见
`.agents/skills/LICENSE-mattpocock-skills`）的副本，版本固定在根目录的 `skills-lock.json`。

## 规则

- 不要就地修改上游副本：`npx skills@latest update` 会覆盖它们。
- 仓库自己的规则写进 `do` 或根 `AGENTS.md`。
- 本仓库已经配置完（`.agents/docs/agents/`），忽略任何「运行 `/setup-matt-pocock-skills`」的提示。

## 唯一的本地 patch

上游把术语表和 ADR 放在 `GLOSSARY.md`、`docs/adr/`；本仓库放在 `.agents/docs/GLOSSARY.md`、`.agents/docs/adr/`，
所以改写了这两个路径。

## 更新上游 skill 之后

1. 重新应用上面的路径改写。
2. 用下面的命令确认没有遗漏：

   ```bash
   rg -nP '(?<![/\w.])(GLOSSARY\.md|docs/adr/)' .agents/skills
   ```

   只允许剩下通用的按上下文示例和标题，不允许有指向仓库根的 `GLOSSARY.md`、`docs/adr/` 路径。
