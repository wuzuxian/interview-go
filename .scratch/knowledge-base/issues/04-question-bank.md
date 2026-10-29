# 04: 阶段三 · 知识库题库

**What to build:**
求职者对知识库发起异步出题（难度、题数、追问数、分类数上限、可选 LLM 提供方），轮询生成进度；完成后在题库浏览、按难度/分类/状态/关键词筛选，并人工修订（手动建题、编辑、改状态、删除）。生成基于知识库检索上下文，taskId 幂等可重入，旧任务被新任务安全替换。

实现细节以 `.scratch/knowledge-base/spec.md` 的「阶段三：知识库题库」为准。

**Blocked by:** 03（复用 RAG 检索层构造出题上下文）

**Status:** ready-for-agent

- [ ] generate 接口：参数校验（difficulty junior/mid/senior、questionCount 1-30、followUpCount 0-5、categoryLimit 1-5、可选 llmProvider）+ 限流 GLOBAL=2/IP=2；创建 taskId（幂等）投递 Redis Stream（knowledgebase:question-gen:stream / question-gen-group，消息字段对齐 Java 常量）
- [ ] sourceContext 构造：4 组固定查询各取 top4、按文档去重、总量 12 片段、截断 5000 字符
- [ ] 结构化输出：复用 ai/structured（失败带修复提示重试，对应 Java BeanOutputConverter + StructuredOutputInvoker 语义）；Prompt 含已有分类/已有题目防重复；题干 NFC 归一化去重
- [ ] replace 小事务：校验 taskId 仍匹配 → 删旧题 → 存新题（DRAFT）→ COMPLETED；被新任务替换的旧任务丢弃结果
- [ ] 状态机：question_gen_status NONE/QUEUED/PROCESSING/COMPLETED/FAILED + question_gen_config 快照 + saved/skipped 计数 + generation-status 轮询接口
- [ ] 题库接口：列表筛选（status/category/difficulty/keyword）、分类统计（CategoryCount）、手动建题、更新、改状态（DRAFT/ACTIVE/ARCHIVED/STALE）、删除；表沿用 Java schema（skill_id 固定 'knowledge-base'），不做迁移（ADR 0001）
- [ ] 测试：HTTP 契约缝锁字段；出题链路走假上游服务缝（结构化输出与重试）；幂等/替换语义走 Redis/服务缝
