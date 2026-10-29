# 05: 阶段四 · 知识库面试 + 全链路收口

**What to build:**
求职者用题库开一场知识库面试：选分类/难度/主问题数/追问数，系统校验题量是否足够（不足时给出含缺口明细的报错），洗牌抽题并让每题携带参考答案、评分要点、评分规则与来源上下文；面试落统一的面试场次（sourceType=KNOWLEDGE_BASE），评估以题目自带参考基线评分，报告展示参考答案；文字面试模块的占位字段全部接上真实值。最后以真实环境全链路冒烟收口，并完成架构文档与偏离清单登记。

实现细节以 `.scratch/knowledge-base/spec.md` 的「阶段四：知识库面试」与「Further Notes」为准。

**Blocked by:** 02, 04（管理接口与题库就绪，面试才有完整可验收路径）

**Status:** ready-for-agent

- [ ] 创建场次：按分类/难度取 ACTIVE 题目并过滤追问池 ≥ followUpCount 的题，不足报错（消息含缺口明细）；主问题整体洗牌抽取、追问 Fisher-Yates 局部洗牌；题目组装携带 referenceAnswer/keyPoints/scoringRubric/sourceContext
- [ ] interview 模块新增「从题目集合创建场次」入口：interview_sessions 落 sourceType=KNOWLEDGE_BASE + knowledge_base_id + interview_category，Redis 缓存 + DB 双写；既有 DTO/缓存/报告中占位字段接真实值
- [ ] 容量接口：各分类可用题数、各追问档位可用量与可选性（difficulty 默认 mid、mainQuestionCount 默认 5 的 query 语义对齐 Java）
- [ ] 评估：评估输入扩展 per-question 参考基线（对应 Java 题目参考上下文注入评分提示词），评分基线是题目自带参考答案而非方向参考题库；报告展示每题参考答案
- [ ] cmd/smoke 知识库模式：真 PG/pgvector/Redis/S3/模型服务，上传 → 向量化 → RAG 查询 → 拒答 → 出题 → 知识库面试全链路（不进 CI）；冒烟发现的原域缺陷回原域修复后再收口
- [ ] 架构依赖文档（面试助手-架构依赖.md）登记新模块的三层清单与代码范围；`.scratch/knowledge-base/spec.md` 偏离清单收口（已知预期：指标/评测跳过、RTF 基础解析、Tika vs pdftotext 文本差异）
