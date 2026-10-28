# Issue Tracker: Local Markdown

本仓库的 issue 与 spec 以 Markdown 文件形式存放在 `.scratch/` 目录（不使用 GitHub Issues）。

## Conventions

- 一个特性一个目录：`.scratch/<feature-slug>/`
- Spec 固定为 `.scratch/<feature-slug>/spec.md`
- 实现 issue 一票一文件：`.scratch/<feature-slug>/issues/<NN>-<slug>.md`，从 `01` 起编号，不合并成单个 tickets 文件
- Triage 状态记录在每个 issue 文件顶部的 `Status:` 行（取值见 `docs/agents/triage-labels.md` 的角色字符串）
- 讨论与历史追加到文件底部 `## Comments` 标题下

## Skill 对接

- 技能说 "publish to the issue tracker"：在 `.scratch/<feature-slug>/` 下新建文件（目录不存在则创建）。
- 技能说 "fetch the relevant ticket"：读取指定路径的文件（用户通常会直接给路径或编号）。

## Wayfinder 操作（供 /wayfinder 使用）

- **Map**: `.scratch/<effort>/map.md`（Notes / Decisions-so-far / Fog 正文）。
- **Child ticket**: `.scratch/<effort>/issues/NN-<slug>.md`，从 `01` 起编号，正文写问题。`Type:` 行记录类型（`research`/`prototype`/`grilling`/`task`）；`Status:` 行记录 `claimed`/`resolved`。
- **Blocking**: 文件顶部 `Blocked by: NN, NN` 行；所列文件全部 `resolved` 后该票解锁。
- **Frontier**: 扫描 `.scratch/<effort>/issues/` 中 open、unblocked、unclaimed 的票，编号最小者优先。
- **Claim**: 动手前设置 `Status: claimed` 并保存。
- **Resolve**: 答案追加到 `## Answer` 标题下，设 `Status: resolved`，并在 map 的 Decisions-so-far 追加上下文指针（要点 + 链接）。
