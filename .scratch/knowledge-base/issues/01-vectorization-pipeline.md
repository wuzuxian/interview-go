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
