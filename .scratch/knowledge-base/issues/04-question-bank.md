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

## Comments

### 实现留痕（2026-10-29，分支 kb-04-question-bank，基于集成分支 tip d55879a）

验收全过，`go build/vet/test` 全绿（build ✓ vet ✓ 17 包 ok 无 FAIL；本包累计 146 例，其中票 04 新增 35 例）。本票交付：出题异步全链路（generate 接口校验/限流 2/2 + taskId 幂等 createTask + `knowledgebase:question-gen:stream`/`question-gen-group` Stream（消息字段 kbId/taskId/retryCount）+ 4 组固定查询构造 sourceContext（各 top4、按文本去重、12 片段封顶后短路后续查询、截 5000 字符加固定后缀）+ ai/structured 结构化输出（失败带修复提示重试）+ Prompt 已有分类/已有题目防重复段 + 题干 NFC 归一化去重 + replace 小事务（校验 taskId → 删旧题 → 存新题 DRAFT → COMPLETED；被替换丢弃））+ 出题消费者/恢复调度（QUEUED 超 2 分钟补投、PROCESSING 超 20 分钟先条件重置再补投）+ generation-status 轮询 + 题库 CRUD 6 接口（列表筛选/方向统计 CategoryCount/手动建题/更新/改状态/删除）。测试布局按 spec：HTTP 契约缝（httptest 真路由锁 Result 键集黄金清单、generate 校验消息逐字、幂等/前置状态/投递失败回退）、假上游缝（fakeChatModel 脚本化结构化输出与重试 + prompt 锚点）、Redis/服务缝（fakeStream/fakeStore 锁条件领取/重试重投/耗尽转失败/恢复补投）、纯函数表驱动（题干 NFC 归一化键/难度与方向归一化）。

按 Java 原文核实的契约事实（与票面文字的差异处，逐字照抄 Java 实际行为）：
1. Stream 消息只传 **kbId/taskId/retryCount**（Java QuestionGenStreamProducer.buildMessage）；出题参数（difficulty/questionCount/followUpCount/categoryLimit/llmProvider）经 question_gen_config 快照在 DB 传递，消费侧 getConfig 读取（taskId 不匹配 → 400「题目生成任务已失效」，快照缺失 → 500「题目生成配置不存在」）。AsyncTaskStreamConstants 里的 FIELD_DIFFICULTY 等常量在本链路未被 producer 使用。
2. generate 校验：difficulty 无 @NotNull（缺省合法，服务端归一为 "mid"）；questionCount 为原生 int（缺省即 0 → 「题目数量最少1题」）；多违规按字段声明序以 ", " 连接（400）。
3. 创建任务前置：vectorStatus != COMPLETED → 400「知识库尚未完成向量化」；QUEUED/PROCESSING 中重复提交 → 400「知识库问题正在生成中，请勿重复提交」；COMPLETED/FAILED 可再次提交（幂等重入）。投递失败如实回退 getStatus（FAILED + 安全文案「题目生成失败，请稍后重试」，Java sent=false 分支）。
4. 生成结果校验错误码走 **3006**（INTERVIEW_QUESTION_GENERATION_FAILED）：空列表「知识库题库生成结果为空」、全空白题干「知识库题库生成结果无有效题干」；检索为空走 6004「知识库未检索到可用于生成题目的内容」。终态 questionGenError 恒为安全文案（Java markFailed 忽略原始错误）。
5. 完成消息：`已生成 N 道题` / `已生成 N 道题，跳过 M 道重复题`；savedCount/skippedCount 为原生 int；可空键（questionGenTaskId/questionGenConfig/message/error/updatedAt）输出 JSON null。
6. taskId 双重校验：executeGeneration 入口再确认（不匹配静默放弃、状态不动）+ replace 小事务锁内再确认（被新任务替换返回 false 丢弃旧结果）。
7. 生成上下文：4 组固定查询 minScore=0、每组 top4；**达到 12 片段即短路后续查询**（Java 外层 break）；超 5000 字符截断加「\n...(知识库片段过长，已截断)」。
8. provider 解析：llmProvider 空/空白/"default"（忽略大小写）→ 默认 Provider（Java resolveProviderId），经 aibridge 接缝（先例 interview.DefaultChatProviderID）。
9. 题库接口错误码：题目不存在 **3003**「面试问题不存在」；手动建题 category/question 空白 400「面试方向不能为空」/「题干不能为空」（更新同）；status 缺省建题默认 DRAFT、显式指定生效；keyword 命中题干/参考答案/评分规则/摘要/方向（大小写不敏感）。

与 Java 的偏离（收口时记入 spec 偏离清单）：
1. 结构化输出 system prompt 组装顺序：Java 为「模板 + "\n\n" + BeanOutputConverter.getFormat() + 防注入段」，Go 经 ai.InvokeStructured 接缝为「模板 + 防注入段 + "\n\n" + 格式指令」——内容一致、顺序不同；重试提示措辞同样沿 ai/structured 既有语义（Go 把上次解析错误追加到 user prompt，Java 追加到 system）。格式指令为 interview 模块既有的通用 JSON 格式说明（非 BeanOutputConverter 按 QuestionListDTO 生成的完整 JSON Schema 文本），字段约束由 prompt 话术与解析兜底。
2. 结构化输出整体失败消息：Java「知识库题库生成失败：请稍后重试」，Go 沿 ai.InvokeStructured 语义为「知识库题库生成失败：<最后解析错误>」（码同为 3006；对外 questionGenError 恒为安全文案，该差异不可达）。
3. LLM 调用走 JSON 响应模式模型（aibridge.JSONChatModelFor）而非 Java 普通 ChatClient + BeanOutputConverter 文本解析——沿 interview 模块既有接缝先例。
4. 字符长度口径：上下文 5000 截断按 Go rune 计（Java 为 UTF-16 code unit），差异仅在增补平面字符出现（沿票 03 偏离 3）；llmProvider 64 上限同口径。
5. 枚举解析错误：题目列表 status 查询参数与请求体 status 非法值 → 400 Result「无效的题目状态: 原值」（Java 为 Spring 类型转换 400 / HttpMessageNotReadable，非 Result 形状，前端不发非法值，不可观察）。
6. 恢复扫描每轮上限 10（Java findStaleQuestionGenerationTasks 无上限逐条全扫；超限下一轮巡检续投）；出题限流与恢复窗口语义沿票 02 先例（数值对齐注解/常量 generate 2/2、QUEUED 2min、PROCESSING 20min、巡检 60s，Go ratelimit 固定窗口 vs Java 滑动窗口）。
7. golang.org/x/text（unicode/norm，实现 Java Normalizer.NFC）由 indirect 提升为直接依赖——无新增第三方模块（版本 v0.34.0 已在 go.sum 依赖图内）。
8. 防注入清洗复用 interview.PromptSanitizer（跨模块复用先例 voiceinterview），四个清洗正则与随机边界标签逐字一致。
