# Domain Docs

工程类技能在探索代码库前应如何消费本仓库的领域文档。

## Before exploring, read these

- **`GLOSSARY.md`**（仓库根目录）：领域术语的唯一权威来源（面试场次、面试方向、分类、追问、批次、交卷、评估等）。
- **`docs/adr/`**：动手前先读涉及该区域的 ADR。当前已有：
  - ADR 0001：模拟面试表沿用 Java 版表结构，应用不做 schema 迁移
  - ADR 0002：文字面试 LLM 编排用 eino ChatModel 直调，不引入 Agent/Graph
- 参考实现（Java 版）在仓库外的 `/home/wuzuxian/interview-guide`，行为对齐以其为准。

## File structure

单上下文仓库：根目录一个 `GLOSSARY.md` + `docs/adr/`。若术语或决策在会话中结晶，由 `/domain-modeling` 技能（经 `/grill-with-docs`）懒创建，不要预先创建空文件。
