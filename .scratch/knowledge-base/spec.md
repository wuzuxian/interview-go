# Spec: 知识库管理复刻

Status: ready-for-agent
Created: 2026-10-29
Origin: /grill-with-docs 设计访谈（共识 + ADR 0004 / ADR 0005；GLOSSARY「知识库与 RAG」节）
Tracker: 本地 Markdown（约定见 docs/agents/issue-tracker.md）

## Problem Statement

Java 版 interview-guide 已停用，Go 版是唯一后端，但知识库产品线整体缺失：求职者无法上传文档构建知识库，无法基于自己的资料做 RAG 问答与多轮会话，无法从文档离线生成题库、再以题库开一场知识库面试。React 前端的知识库页、RAG 会话页与知识库面试入口已按 Java 契约实现且不再改动；文字面试模块里 `knowledgeBaseId` / `sourceType=KNOWLEDGE_BASE` 的占位字段也在等真实值。后端缺位意味着「上传文档 → 检索问答 → 题库 → 面试」这条产品线不可用。

## Solution

在 Go 后端完整复刻知识库管理后端，共 32 个接口：知识库管理 + RAG 查询（13）、RAG 多轮会话（8）、知识库题库 + 知识库面试（11）。链路为：上传（S3 存储 + SHA-256 去重 + 并发闸门 + 限流）→ Redis Stream 异步向量化（文档解析 → TokenTextSplitter 忠实移植切分 → eino Embedder 分批 → pgvector 两阶段写入 → 心跳 fencing + 恢复调度）→ RAG 查询（问题改写 → 动态分档检索 → [S#] 引用 → 拒答探测 → 参考片段 footer）→ SSE 流式。协议按与 Java 共存设计（同库、同 Redis Stream、同向量 metadata 协议，ADR 0005），DTO 与线协议逐字对齐，前端零改动切换。

## User Stories

1. 作为求职者，我想上传一份文档（PDF/DOC/DOCX/TXT/MD/RTF，≤50MB）创建知识库，以便把参考资料变成可检索的语料。
2. 作为求职者，重复上传内容相同的文件时，我想被告知重复且不重复向量化，以便避免同一份资料占多份空间。
3. 作为求职者，上传被并发闸门或限流拒绝时，我想得到明确提示稍后再试，而不是无限等待。
4. 作为求职者，我想在知识库列表看到名称、分类、原始文件名、大小、向量状态、切片数、提问次数与最近访问，以便一眼掌握每个知识库的状态。
5. 作为求职者，我想按向量状态过滤列表、按时间或其他字段排序，以便快速找到处理中或失败的条目。
6. 作为求职者，我想用关键词搜索知识库，以便在条目多时定位文件。
7. 作为求职者，我想查看全部分类、按分类浏览、查看未分类条目，并给知识库设置分类，以便按主题组织资料。
8. 作为求职者，我想看知识库统计（总数、总提问数、总访问数、完成数、处理中数），以便了解整体使用情况。
9. 作为求职者，我想下载知识库的原始文件，以便本地留档核对。
10. 作为求职者，我想删除知识库，并确保其向量、存储文件与 RAG 会话关联被一并清理，以便不留垃圾数据。
11. 作为求职者，当向量化失败或卡住时，我想看到失败原因（vectorError）并手动触发重新向量化，以便自愈而不是干等。
12. 作为求职者，我想对一个或多个知识库提问，得到带 [S#] 来源编号引用的回答，以便知道答案出自哪份材料的哪个片段。
13. 作为求职者，无论问题长短，我想系统自动选择合适的检索档位（数量与相似度阈值），以便短关键词和长句子都有好的召回。
14. 作为求职者，在多轮追问时，我想让系统结合历史改写我的问题再检索，以便代词与省略不至于检索落空。
15. 作为求职者，当知识库中没有相关内容时，我想得到明确的固定拒答文案，而不是模型编造答案。
16. 作为求职者，我想在回答结束后看到服务端生成的参考片段列表（含知识库名与原文件名），以便溯源核验。
17. 作为求职者，我想流式地看到回答逐字出现，中途取消时已生成部分被保存，以便快速得到答案且不丢内容。
18. 作为求职者，我想创建 RAG 会话（选定知识库、可起标题），以便围绕一组资料持续提问。
19. 作为求职者，我想查看 RAG 会话列表（置顶优先、含消息数与知识库名）、会话详情（全部消息），以便找回历史对话。
20. 作为求职者，我想修改会话标题、置顶/取消置顶、更换会话关联的知识库、删除会话，以便管理对话。
21. 作为求职者，我想对知识库发起异步出题（难度、题数、追问数、分类数上限、可选 LLM 提供方），以便后台生成题库。
22. 作为求职者，我想轮询出题状态（状态、已保存/跳过数、错误信息），以便知道生成进度。
23. 作为求职者，我想按难度、分类、状态、关键词筛选浏览题库，以便定位题目。
24. 作为求职者，我想手动新增、编辑、改状态（草稿/激活/归档）与删除题目，以便人工修订题库。
25. 作为求职者，我想查看题库的分类统计（各分类题数），以便判断题量是否够开一场面试。
26. 作为求职者，我想用题库创建知识库面试（分类、难度、主问题数、追问数），题目不足时得到含明细的明确报错，以便按需练习。
27. 作为求职者，我想查看面试容量（各分类可用题数、各追问档位可用量与可选性），以便创建前预判能否成场。
28. 作为求职者，知识库面试走统一的面试场次（sourceType=KNOWLEDGE_BASE），评估以题目自带的参考答案与评分要点为基线，我想在报告里看到参考答案，以便对照改进。

## Implementation Decisions

总体：新增知识库业务模块（internal/knowledgebase，controller/service/repo/model/dto 内聚），复用既有 infra（eino ChatModel/Embedder、S3 存储、docparse、pgvector、Redis Stream 模式、限流、result/apperr）；文字面试模块新增「从题目创建场次」的服务入口并让占位字段接真实值。四阶段交付，票间依赖按阶段排序。

### 阶段一：知识库核心（上传 / 向量化 / 管理接口）

- **错误码**：新增 6xxx 段对齐 Java ErrorCode（6001 不存在、6002 解析失败、6004 查询失败、6005 删除失败、6006 向量化失败；6003 在 Java 侧即缺号，保持缺口）。
- **上传**：multipart ≤50MB；MIME 白名单对齐 Java（pdf/msword/wordprocessingml/text/plain/markdown/rtf，MD 扩展名兜底）；SHA-256 内容去重（file_hash 唯一索引，命中返回 duplicate=true 与既有条目信息）；S3 键前缀 `knowledgebases/yyyy/MM/dd/`；Semaphore(4) 并发闸门不排队直接拒绝；限流 GLOBAL=3/IP=3；落库失败补偿删除 S3 孤儿对象；向量任务投递失败如实返回 FAILED 并提示可手动重试。
- **解析**：复用 docparse（PDF 双路已有）；新增基础 RTF→纯文本剥离器；解析在独立 goroutine + 超时（对齐 Java 2 分钟超时语义）；提取文本上限对齐 Java（5MB 文本）；解析后文本清洗。
- **切分**：忠实移植 Spring AI `TokenTextSplitter`（ADR 0004）：chunkSize=800 token、minChunkSizeChars=350、minChunkLengthToEmbed=5、maxNumChunks=10000、keepSeparator、中文句末标点集；tiktoken cl100k_base 词表 go:embed；strategyVersion=`token-v1` 写入每条向量 metadata 与知识库 vector_config 快照；对外包装为 eino Transformer 接口。
- **向量化协议**（ADR 0005，完整复刻 Java fencing）：`PENDING→PROCESSING` 条件更新原子领取并生成 attempt_id；下载解析后心跳、embedding 每批后节流心跳（30s）；终态全部条件写入（防旧代次覆盖新任务）；embedding 分批 batch=10（维度 1024，默认 DashScope text-embedding-v3）；两阶段写入：临时 kb_id=`pending:<kbId>:<jobId>` → 成功后在数据库锁内先删旧正式向量再 promote 临时向量并清理临时字段，同时短事务写 chunk_count 与 config 快照；失败按 jobId 清理临时向量；检索永远只见正式向量。
- **恢复调度器**：60s 巡检；PENDING 超 10 分钟补投；PROCESSING 超 15 分钟无心跳先条件重置回 PENDING 再补投；recovery_count≥3 转 FAILED；手动 revectorize 清零恢复计数。
- **Redis Stream**：key/group/consumer 前缀/消息字段与 Java 逐字一致（`knowledgebase:vectorize:stream` / `vectorize-group`，消息只传 kbId；重试 3 次、batch 10、pending 认领 5 分钟空闲、stream maxlen 1000）。
- **向量存储**：同一张 vector_store 表；metadata 键对齐 Java（kb_id、source_kb_id、source_name、source_filename、chunk_strategy、chunk_index/total_chunks、source_id=md5(fileHash+configJson+i)）；internal/vector 泛化承载 resume（source='resume' 行为不变）与 kb 两类来源；检索 kb_id IN 过滤优先下推 SQL，前置过滤失败回退本地过滤（放大 3 倍 topK 内存过滤）。
- **管理 REST（13 接口）**：list（vectorStatus 过滤 + sortBy，null/time 为时间序）、详情、删除（事务内摘 RAG 会话关联 → 删记录 → 事务外删向量〔失败不阻断〕→ 删 S3）、query、query/stream、categories、category/{category}、uncategorized、set-category、download（Content-Disposition UTF-8 文件名）、search（关键词）、stats、revectorize（限流 GLOBAL=2/IP=2）。全部走统一 Result 封装；限流数值对齐 Java 注解（窗口语义按 Java 切面在实现票核对）。

### 阶段二：RAG 查询 + RAG 会话

- **查询管线**：LLM 问题改写（携带最近 10 条完成历史、助手回复截 200 字符；失败或关闭时降级原问题）→ 动态分档检索（短问题 topK=20/minScore=0.25、中 12/0.28、长 8/0.28；具体默认值以 Java application.yml 为准在实现票核对）→ 可选双路召回融合（改写路+原始路按文档 id 去重保留高分，默认关）→ prompt 组装：片段编号 [S1] 来源（知识库名、原文件、第 n/m 片段）、反注入指令、用户内容数据边界包裹；三段模板（system/user/rewrite）从 Java .st 逐字移植。
- **拒答治理**：流式输出先缓冲 120 字符探测「无信息」类模板，命中即截断为固定拒答文案，否则透传。
- **收尾**：流结束后追加服务端生成的参考片段 footer；提问计数批量 +1（单条 SQL）。
- **SSE 线协议**：查询流与 RAG 会话流均为 data-only 事件；会话流换行转义为字面 `\n`；先同步落库用户消息 + AI 占位，流完成回写全文，取消/错误保存已生成部分（空则写固定中断/错误文案）。
- **RAG 会话 CRUD（8 接口）**：创建（kbIds 必填、标题可选）、列表（置顶优先、含消息数与知识库名）、详情（含消息）、改标题、置顶切换、换绑知识库、删除、发消息（流式）；多轮历史取最近 10 条完成消息。

### 阶段三：知识库题库

- **异步出题**：POST generate 校验参数（难度 junior/mid/senior、题数 1-30、追问 0-5、分类上限 1-5、可选 provider）→ 创建 taskId（幂等）→ Redis Stream（key/group 对齐 `knowledgebase:question-gen:stream`）→ RAG 检索构造 sourceContext：4 组固定查询各取 top4、按文档去重、总量 12 片段、截断 5000 字符 → 结构化输出（复用 ai/structured，失败带修复提示重试）→ Prompt 含已有分类/已有题目防重复、题干 NFC 归一化去重 → 小事务 replace：校验 taskId 仍匹配 → 删旧题 → 存新题（DRAFT）→ COMPLETED；旧任务被新任务替换则丢弃结果。
- **状态机**：question_gen_status NONE/QUEUED/PROCESSING/COMPLETED/FAILED + 配置快照 + saved/skipped 计数 + generation-status 轮询接口。
- **题库接口**：列表筛选（status/category/difficulty/keyword）、分类统计、手动建题、更新、改状态（DRAFT/ACTIVE/ARCHIVED/STALE）、删除；表结构复用 Java schema（skill_id 固定 'knowledge-base'），应用不做迁移（ADR 0001）。

### 阶段四：知识库面试

- **创建**：按分类/难度取 ACTIVE 题目，过滤追问池 ≥ 追问数的题，不足报错（消息含缺口明细）；主问题整体洗牌抽取、追问 Fisher-Yates 局部洗牌；题目携带 referenceAnswer/keyPoints/scoringRubric/sourceContext 进入场次。
- **统一场次**：经文字面试模块新暴露的「从题目集合创建场次」入口落 interview_sessions（sourceType=KNOWLEDGE_BASE、knowledge_base_id、interview_category），复用 Redis 缓存 + DB 双写；容量接口返回各分类可用题数与追问档位可用量/可选性。
- **评估**：评估输入扩展 per-question 参考基线（对应 Java 的题目参考上下文注入），评分提示词注入参考答案/评分要点/评分规则作为基线；报告展示每题参考答案；interview 模块 DTO/缓存/报告中占位字段接真实值。

## Testing Decisions

好测试的标准（沿 voice spec）：只测外部行为，不 mock 内部步骤；契约测试锁 JSON 字段名与错误码；纯函数表驱动单测、中文用例名；封闭式管道测试（httptest 真服务端 + 假上游）；真实冒烟不进 CI。

测试缝（已与维护者确认的布局，主缝是 HTTP 契约缝）：

1. **HTTP 契约缝（主缝）**：httptest 真路由 + 依赖全 fake，锁 32 个接口的请求/响应 JSON 字段、6xxx 错误码、SSE 线协议（data-only、换行转义、取消/错误回写部分内容）。先例：internal/interview/contract_test.go。
2. **假上游服务缝**：httptest OpenAI 兼容假 ChatModel/Embedding 服务端，锁 RAG prompt 组装、流式行为、结构化出题与重试。先例：voiceinterview 假 DashScope 服务端。
3. **Redis Stream/恢复缝**：fakeStream/fakeCache/fakeStore 接口 fake，测向量化/出题 worker 编排与恢复调度决策；DB 条件更新 SQL 语义（领取/心跳/promote）单测不覆盖，由真库缝兜底。先例：evaluation_recovery_test.go。
4. **纯函数缝**：TokenTextSplitter 黄金用例（中英混排、跨句边界、超长段、纯换行文本）、动态分档参数、拒答探测、RTF 剥离。
5. **真库缝**：cmd/smoke 增知识库模式（真 PG/pgvector/Redis/S3/模型服务），上传 → 向量化 → RAG 查询 → 拒答 → 出题 → 知识库面试全链路，不进 CI。

## Out of Scope

- 语音面试与知识库/RAG 的结合（语音 spec 已明确排除，维持）。
- Micrometer 全链路指标体系与 ragEvaluation 评测任务（偏离：以 slog 阶段日志替代）。
- 复杂 RTF 嵌入对象（图片/OLE）解析——基础剥离器，偏离留痕。
- 前端任何改动。
- 数据库迁移：knowledge_bases、knowledge_base_questions、rag_chat_sessions/messages/关联表、vector_store 均沿用 Java 既有 schema（ADR 0001）。
- eino Agent/ADK/Graph 编排（ADR 0002：ChatModel/Embedder/Transformer 直调）。

## Further Notes

- **偏离清单**：实现完成后在本文件末尾补「与 Java 行为的有意偏离」完整清单（先例：mock-interview 11 条、voice-interview 11 条）。已知预期偏离：指标/评测跳过、RTF 基础解析、PDF 解析引擎差异（Java Tika vs Go pdftotext 双路——提取文本的空白/排版细节不完全一致，切片随之有差，检索质量可接受）。
- **共存是协议要求，不是部署要求**：当前只有 Go 在跑，但状态机、Redis Stream key/group、向量 metadata 协议按两实例共存逐字对齐（ADR 0005）；将来 Java 复用或数据混跑（含互相补投恢复）无虞。
- **术语**：文档用语遵循 GLOSSARY「知识库与 RAG」节——知识库=单文档语料单位；题库=模型离线生成（与方向静态「参考题库」相对）。
- **验收**：每票 TDD 自验；全阶段完成后 code-review（沿语音面试流程）；最终以 smoke 知识库模式真链路通过收口。

## 与 Java 行为的有意偏离（收口清单，2026-10-29）

五票完成后按票 Comments 归并收口（先例：mock-interview 11 条、voice-interview 11 条）。前 3 条为 spec 预案内偏离。

**预案内**

1. Micrometer 全链路指标体系与 ragEvaluation 评测任务未移植：以 slog 中文阶段日志（改写/检索/融合/流式起止等）替代。（票 03）
2. RTF 采用基础剥离器：支持 `\uN`/UTF-8 序列/cp1252；复杂嵌入对象（图片/OLE）不解析；`ansicpg936` 原生 GBK 字节流可能乱码（需 x/text 依赖，超票面授权未引入）。（票 01）
3. PDF/DOC 解析引擎差异：Java Tika AutoDetectParser vs Go pdftotext 外部工具 + 纯 Go 兜底双路，提取文本的空白/排版细节不完全一致，切片随之有差，检索质量可接受。（票 01）

**阶段一（上传/向量化/管理）**

4. 5MB 文本上限实现点：Java 在 Tika 写入流中中断，Go 解析完成后整体校验拒绝——外部契约相同（解析失败），失败点略后。（票 01）
5. tokenizer 特殊 token：jtokkit 对 `<|endoftext|>` 等抛异常，Go 对齐为返回错误走向量化失败路径。（票 01）
6. fileKey 文件名净化：Java 转拼音大驼峰，Go 沿 resumes 先例替换为下划线（仅影响存储键美观）。（票 01）
7. cleanText 首尾修剪用 Go TrimSpace（Unicode 空白集合比 Java strip 略宽）。（票 01）
8. Stream maxlen 近似裁剪、心跳 monotonic clock、promote+配置快照合并为单事务。（票 01）
9. 限流窗口语义：Java 切面为 Redis ZSET 滑动窗口，Go ratelimit 为共享固定窗口（INCR+PEXPIRE）——既有 infra（resume 同用），数值（3/3、2/2、10/10、5/5）已逐一对齐，窗口形态未改。（票 02，票 03/04 沿用）
10. sortEntities 的 null 拆箱：Java 对 null fileSize/accessCount/questionCount 比较时 NPE，Go 按 0 参与排序（正常数据列值非空，行为不可区分）。（票 02）
11. 非法请求体 JSON：Go 返回 400「请求参数错误/请求体格式不正确」；Java 走全局兜底 500「系统繁忙」（前端恒发合法 JSON，不可达路径）。（票 02，票 04/05 同口径）
12. 下载文件名编码自实现 javaURLEncode（URLEncoder.encode + replace("+","%20") 逐字节等价，表驱动锁定）；上传/下载时间字段为 Go RFC3339（带时区后缀）vs Java LocalDateTime ISO（无时区），前端按字符串透传展示。（票 02）
13. search 缺 keyword 参数：Java @RequestParam 必填返回参数错误，Go 空参按空白关键词返回全量列表（前端恒带参）。（票 02）

**阶段二（RAG 查询/会话）**

14. 会话列表/详情的知识库数组顺序：Java HashSet 迭代序不稳定，Go 按知识库 id 升序稳定输出；会话 DTO 的 knowledgeBaseIds 按请求顺序输出。（票 03）
15. 字符长度口径：分档计长（`\s` 剥离后）、拒答探测窗口 120 字符、助手回复截 200 字符、出题上下文 5000 截断、llmProvider 64 上限按 Go rune 计（Java 为 UTF-16 code unit），差异仅在 emoji/增补平面字符出现，中文场景不可区分。（票 03，票 04 同口径）
16. 会话流历史加载 DB 失败：Java getStreamAnswer 直接抛出（500 Result），Go 记 warn 后以空历史继续（多轮场景更稳，不阻断回答）。（票 03）
17. SSE 响应头：Go 设 `Content-Type: text/event-stream` + `Cache-Control: no-cache`（Java WebFlux 另带 charset 与 connection 头，前端不读取）。（票 03）
18. 检索回退路径前置过滤失败后重 embed 再查（与 Java 二次调用 similaritySearch 的重嵌入一致）；DB 条件更新 SQL 执行语义由 smoke 真库兜底（测试决策 3）。（票 03）

**阶段三（题库）**

19. 结构化输出 system prompt 组装顺序与重试提示位置：Java「模板 + BeanOutputConverter.getFormat() + 防注入段」、重试提示追加 system；Go 经 ai.InvokeStructured 接缝为「模板 + 防注入段 + 格式指令」、解析错误追加 user prompt。格式指令为 interview 模块既有通用 JSON 格式说明（非 BeanOutputConverter 完整 Schema 文本），字段约束由 prompt 话术与解析兜底。（票 04）
20. 结构化输出整体失败消息：Java「知识库题库生成失败：请稍后重试」，Go 沿 ai.InvokeStructured 语义带最后解析错误（码同 3006；对外 questionGenError 恒为安全文案，差异不可达）。（票 04）
21. LLM 调用走 JSON 响应模式模型（aibridge.JSONChatModelFor）而非 Java 普通 ChatClient + BeanOutputConverter 文本解析——沿 interview 模块既有接缝先例。（票 04）
22. 枚举解析错误：题目列表 status 查询参数与请求体 status 非法值 → 400 Result「无效的题目状态: 原值」（Java 为 Spring 类型转换 400 / HttpMessageNotReadable，非 Result 形状，前端不发非法值，不可观察）。（票 04）
23. 恢复扫描每轮上限 10（Java findStaleQuestionGenerationTasks 无上限全扫；超限下一轮巡检续投）；恢复窗口（QUEUED 2min、PROCESSING 20min、巡检 60s）按 Java 常量对齐。（票 04）
24. golang.org/x/text（unicode/norm，实现 Normalizer.NFC）由 indirect 提升为直接依赖——无新增第三方模块。（票 04）
25. 防注入清洗复用 interview.PromptSanitizer（跨模块复用先例 voiceinterview），四个清洗正则与随机边界标签逐字一致。（票 04）

**阶段四（知识库面试）**

26. 「从题目集合创建场次」的缓存双写失败按尽力而为处理（warn 不阻断），Java sessionCache.saveSession 异常向上传播——沿文字面试创建的仓库既有约定（读路径 DB 兜底可重建）。（票 05）
27. 追问池/题目要点解析失败静默降级空列表（Java log.warn 后降级）——沿题库模块既有 readStringList/readFollowUps 口径，行为（空池）一致，仅日志可见性差异。（票 05）
28. 随机源 Java ThreadLocalRandom → Go math/rand/v2；洗牌算法同构（主问题 Collections.shuffle 末端 Fisher-Yates、追问前端局部 Fisher-Yates 且不改原列表），随机源注入为测试缝，生产行为等价。（票 05）

29. 路径 ID 参数解析：非数字 / int64 溢出 / 负数 → 400 Result「无效的ID参数」（Java @PathVariable Long 类型不匹配走全局兜底 500「系统繁忙，请稍后重试」；负数在 Java parseLong 下合法、走「不存在」路径）——评审统一要求 400 快速失败。（code-review 修复）

共 29 条。验收留痕：真链路 smoke（-mode knowledgebase）由维护者在真实环境执行收口。
