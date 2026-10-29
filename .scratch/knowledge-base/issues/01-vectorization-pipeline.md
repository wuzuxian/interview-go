# 01: 阶段一 · 向量化管线（上传 → 切分 → 向量入库 → 恢复）

**What to build:**
求职者上传一份文档后，系统能可靠地把它变成可检索的知识库：上传经并发闸门与限流把守；内容相同的重复文件直接告知而不重复处理；原始文件存入对象存储；知识库落库为 PENDING；后台异步完成「下载 → 解析（含 RTF）→ TokenTextSplitter 切分 → 分批向量化 → 两阶段写入 pgvector」，全程心跳 fencing 与恢复调度兜底，失败可手动重试。检索永远只见完整向量，半成品不可见。

实现细节以 `.scratch/knowledge-base/spec.md` 的「阶段一：知识库核心」为准；切分算法遵循 ADR 0004，向量 metadata 与 Redis Stream 协议遵循 ADR 0005。

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] 上传：≤50MB、MIME 白名单（pdf/msword/wordprocessingml/text/plain/markdown/rtf + md 扩展名兜底）、限流 GLOBAL=3/IP=3、并发闸门 4 不排队直接拒绝；成功返回含 vectorStatus=PENDING 的 DTO；重复上传返回 duplicate=true 且不投递向量化任务；落库失败补偿删除 S3 孤儿对象；任务投递失败如实返回 FAILED 并提示可重试
- [ ] TokenTextSplitter 忠实移植：chunkSize=800、minChunkSizeChars=350、minChunkLengthToEmbed=5、maxNumChunks=10000、keepSeparator、中文句末标点集；cl100k_base 词表 go:embed；strategyVersion=token-v1 写入向量 metadata 与 vector_config 快照；黄金用例表驱动（中英混排、跨句边界、超长段、纯换行文本）
- [ ] RTF 基础剥离器产出可读纯文本；解析隔离 2 分钟超时；提取文本上限 5MB；解析后文本清洗
- [ ] worker：PENDING→PROCESSING 条件更新原子领取 + attempt_id 代次；下载解析后心跳、embedding 每批后节流心跳（30s）；终态条件写入；embedding 分批 batch=10（1024 维）；pending:&lt;kbId&gt;:&lt;jobId&gt; 临时 kb_id，成功在 DB 锁内删旧正式向量再 promote，失败按 jobId 清理临时向量
- [ ] 向量 metadata 键对齐 Java：kb_id、source_kb_id、source_name、source_filename、chunk_strategy、chunk_index/total_chunks、source_id=md5(fileHash+configJson+i)；向量存储层泛化承载 resume（source='resume' 行为不变）与 kb
- [ ] Redis Stream 与 Java 逐字一致：knowledgebase:vectorize:stream / vectorize-group / 字段 kbId；重试 3 次、拉取 batch 10、pending 认领空闲 5 分钟、stream maxlen 1000
- [ ] 恢复调度器 60s 巡检：PENDING 超 10 分钟补投、PROCESSING 超 15 分钟无心跳先条件重置再补投、recovery_count≥3 转 FAILED；revectorize 接口清零恢复计数重新领取（限流 GLOBAL=2/IP=2）
- [ ] 测试：纯函数缝（切分黄金用例、RTF 剥离、动态参数）、Redis/恢复缝（fakeStream/fakeCache/fakeStore 接口 fake）、HTTP 契约缝（上传接口）；DB 条件更新 SQL 语义不在单测覆盖范围，由票 05 的 smoke 真库兜底

## Comments

### 实现留痕（2026-10-29，分支 kb-01-vectorization-pipeline，合并 c2480e4）

验收全过，`go build/vet/test` 全绿。切分器黄金用例由 Java 真实类（gradle 缓存中 spring-ai-commons 2.0.0 + jtokkit 1.1.0 反射）生成，22 例 Go 输出逐字节一致；cl100k_base 词表从 jtokkit jar 提取 go:embed。

与 Java 的偏离（收口时记入 spec 偏离清单）：
1. PDF/DOC 解析引擎差异（spec 已预案）：Go 双路 pdftotext/纯 Go 替代 Tika，提取文本空白/排版细节不同。
2. RTF 编码范围：支持 `\uN`/UTF-8 序列/cp1252；`ansicpg936` 原生 GBK 字节流可能乱码（需 x/text 依赖，超票面授权未引入）。
3. 5MB 文本上限实现点：Java 在 Tika 写入流中中断，Go 解析完成后整体校验拒绝——外部契约相同（解析失败），失败点略后。
4. tokenizer 特殊 token：jtokkit 对 `<|endoftext|>` 等抛异常，Go 对齐为返回错误走向量化失败路径。
5. fileKey 文件名净化：Java 转拼音大驼峰，Go 沿 resumes 先例替换为下划线（仅影响存储键美观）。
6. cleanText 首尾修剪用 Go TrimSpace（Unicode 空白集合比 Java strip 略宽）。
7. Stream maxlen 近似裁剪、心跳 monotonic clock、promote+快照合并为单事务（Java attemptId 形态同）。
