# 03: 阶段二 · RAG 查询与 RAG 会话

**What to build:**
求职者选中一个或多个知识库提问，得到带 [S#] 来源编号引用的流式回答：问题先经模型改写（结合多轮历史）、按长短动态分档检索；知识库没有相关内容时给出固定拒答而非编造；回答结束后附服务端生成的参考片段列表；每次提问计数累加。并能创建 RAG 会话围绕一组知识库多轮追问：会话可置顶、改名、换绑知识库、删除；流式中断不丢已生成内容。

实现细节以 `.scratch/knowledge-base/spec.md` 的「阶段二：RAG 查询 + RAG 会话」为准。

**Blocked by:** 01（检索依赖已入库的向量）

**Status:** resolved

- [x] 检索层：kb_id IN 过滤优先下推 SQL，前置过滤失败回退本地过滤（放大 3 倍 topK 内存过滤）；动态分档：短问题（≤4 字）topK=20/minScore=0.25、中（≤12 字）12/0.28、长 8/0.28，具体默认值对照 Java application.yml 核对；可选双路召回融合（改写路+原始路按文档 id 去重保留高分，默认关）
- [x] 问题改写：带最近 10 条完成历史（助手回复截 200 字符），失败或关闭时降级原问题
- [x] prompt 组装：片段编号 [S1] 来源（知识库名、原文件名、第 n/m 片段、控制字符转义）、反注入指令、用户内容数据边界包裹；三段模板（system/user/rewrite）从 Java .st 文件逐字移植
- [x] 拒答治理：流式输出先缓冲 120 字符探测「无信息」类模板，命中立即截断为固定拒答文案，否则透传
- [x] 收尾：流结束后追加服务端参考片段 footer；提问计数单条 SQL 批量 +1
- [x] SSE：query 与 query/stream 均为 data-only 事件（text/event-stream），行为对齐 Java Flux 语义
- [x] RAG 会话 7 接口：创建（kbIds 必填、标题可选）、列表（置顶优先、含消息数与知识库名）、详情（含消息）、改标题、置顶切换、换绑知识库、删除
- [x] 会话发消息流式：先同步落库用户消息 + AI 占位；data 事件换行转义为字面 \n；完成回写全文，取消保存已生成部分（空则写固定中断文案），错误同（空则写固定错误文案）；多轮历史取最近 10 条完成消息
- [x] 测试：HTTP 契约缝锁字段与 SSE 线协议；假上游服务缝（httptest OpenAI 兼容假 ChatModel/Embedding）锁 prompt 组装、流式行为、降级链路；动态分档与拒答探测走纯函数缝

## Comments

### 实现留痕（2026-10-29，分支 kb-03-rag-query-sessions，基于集成分支 tip 8121850）

验收全过，`go build/vet/test` 全绿（build ✓ vet ✓ 15 包 ok 无 FAIL；本包累计 109 例含票 01/02 先例）。本票交付：RAG 查询管线（query_service.go：改写/动态分档/检索编排/双路融合/prompt/拒答探测/footer/计数）、query 与 query/stream 两接口（SSE data-only）、internal/vector.SimilaritySearch（kb_id IN 下推 + topK×3 回退）、RAG 会话 7 接口 + 会话发消息流式（rag_chat_repo/service/controller）。测试布局按 spec：纯函数缝表驱动（分档/拒答五标记/探测窗口状态机/rag_sources 转义与 footer/prompt 锚点）、HTTP 契约缝（httptest.NewServer 真连接读 SSE 流）、假上游缝（fakeChatModel 脚本化 Generate/Stream + schema.Pipe 造流，锁 system+反注入逐字锚点、user 数据边界、rewrite 插值、流式透传/错误/降级、取消落库轮询）。

按 Java 原文核实的契约事实（与票面文字的差异处，逐字照抄 Java 实际行为）：
1. 短问题 minScore：票面写 0.25（Properties 代码默认），Java application.yml 运行值 0.18——按票面「以 yml 核对为准」取 0.18，配置键 `min_score_short` 可调。
2. POST /query 对缺失知识库：counts 校验 NOT_FOUND 被 answerQuestion 外层 catch 吞掉转 **6004「知识库查询失败，请稍后重试」**（非 404）；stream 端点同场景输出流内固定错误文案「【错误】知识库查询失败：AI服务暂时不可用，请稍后重试。」。
3. 流内固定错误文案经 doOnNext 累进 fullContent，doOnComplete 把它回写为 AI 消息全文（RagChatController.doOnError 实际不可达，属防御代码，Go 同样保留该分支不测）。
4. 同步答案 normalizeAnswer：模型复述「无信息」类模板同样归一为固定拒答且不带 footer；流式探测窗口在**流末**对不足 120 字符的短回答整段归一化透出（Java 完成回调语义，短回答表现为单个合并事件）。
5. 换绑知识库 updateSessionKnowledgeBases 不校验存在性（findAllById 静默丢弃缺失项）；创建会话才校验（数量不符 → 404「部分知识库不存在」）。
6. 历史加载：取 maxMessages+1=11 条完成消息 DESC，剔除首条（当前轮 user）后反转为正序；仅 1 条时历史为空。
7. query 响应：knowledgeBaseId=首个知识库 ID，knowledgeBaseName=多库以「、」连接，缺失位置补「未知知识库」。

与 Java 的偏离（收口时记入 spec 偏离清单）：
1. RagMetrics（Micrometer 指标）与 RagQueryExecution/emitTrace/retrieveOnly 评测轨迹未移植——spec Out of Scope 已明确，Go 以 slog 中文阶段日志（改写/检索/融合/流式起止）替代。
2. 会话列表/详情的知识库数组顺序：Java HashSet 迭代序不稳定，Go 按知识库 id 升序稳定输出（前端按数组渲染，顺序差异不可观察）；会话 DTO 的 knowledgeBaseIds 按请求顺序输出。
3. 字符长度口径：分档计长（\s 剥离后）、探测窗口 120 字符、助手回复截 200 字符按 Go rune 计（Java 为 UTF-16 code unit），差异仅在 emoji/增补平面字符出现，中文场景不可区分。
4. 会话流历史加载 DB 失败：Java getStreamAnswer 直接抛出（500 Result），Go 记 warn 后以空历史继续（多轮场景更稳，不阻断回答）。
5. 限流窗口语义沿票 02 先例：数值对齐注解（query 10/10、query/stream 5/5），Go ratelimit 固定窗口 vs Java 滑动窗口。
6. SSE 响应头：Go 设 `Content-Type: text/event-stream` + `Cache-Control: no-cache`（Java WebFlux 另带 charset 与 connection 头，前端不读取）。
7. 检索回退路径前置过滤失败后重 embed 再查（与 Java 二次调用 vectorStore.similaritySearch 的重嵌入一致）；SQL 执行语义由票 05 smoke 真库兜底（spec 测试决策 3）。
