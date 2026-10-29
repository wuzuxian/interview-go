# 02: 阶段一 · 知识库管理接口（13 个收尾）

**What to build:**
求职者能在前端知识库页完成全部日常管理：列表（按向量状态过滤、按字段排序）、详情、关键词搜索、统计总览、分类列表与按分类浏览、未分类、给知识库设置分类、下载原始文件、删除知识库。删除时连带清理向量、存储文件与 RAG 会话关联，且任何一步失败不产生脏数据。

实现细节以 `.scratch/knowledge-base/spec.md` 的「阶段一：知识库核心（管理 REST）」为准。

**Blocked by:** 01（上传与向量入库先在，接口才有可验收的真实数据路径）

**Status:** resolved

- [ ] list 支持 vectorStatus 过滤与 sortBy（null 或 "time" 为时间序，其余取值语义对照 Java sortEntities 实现）；详情、search、stats（totalCount/totalQuestionCount/totalAccessCount/completedCount/processingCount）、categories、category/{category}、uncategorized、set-category、download（Content-Disposition 带 UTF-8 编码文件名）的响应 DTO 字段与 Java 逐字对齐
- [ ] 删除补偿顺序：事务内先摘 RAG 会话关联 → 删 DB 记录 → 事务外删向量（按 metadata kb_id，失败不阻断仅记日志）→ 删 S3 文件（失败留 storageKey 供人工补偿）
- [ ] 错误码 6xxx 段对齐 Java ErrorCode：6001 不存在、6002 解析失败、6004 查询失败、6005 删除失败、6006 向量化失败；6003 在 Java 侧即缺号，保持缺口
- [ ] 全部接口走统一 Result 封装；限流数值对齐 Java 注解（窗口语义按 Java 切面实现核对，实现票时确认）
- [ ] HTTP 契约测试（主缝）锁全部接口的请求/响应 JSON 字段名与错误码

## Comments

### 实现留痕（2026-10-29，分支 kb-02-management-api，基于集成分支 tip 0cd8159）

验收全过，`go build/vet/test` 全绿。本票 10 个新接口（list/detail/delete/categories/category/uncategorized/set-category/download/search/stats）+ 票 01 已有的 upload/revectorize 收尾；query/query/stream 属阶段二随票 03。契约测试 21 例锁字段名与错误码（含 sortBy 七态表驱动、下载错误三态、删除补偿顺序事件断言）。

按 Java 原文核实的契约事实（逐字照抄，勿当 bug"修正"）：
1. 详情不存在 → 500（`Result.error(String)` 固定 SERVER_ERROR），非 6001；list 非法 vectorStatus 同为 500「无效的向量化状态: 原值」。
2. 删除不存在 → 404「知识库不存在」（`BusinessException(NOT_FOUND, ...)`）；设置分类/下载不存在 → 6001；下载 storageKey 缺失 → 4002「文件存储信息不存在」。
3. stats.totalQuestionCount 口径 = rag_chat_messages USER 消息数（多知识库提问只算一次），非 sum(question_count)。
4. 本票接口在 Java 侧除 revectorize(2/2) 外均无 @RateLimit 注解，不加限流中间件。

与 Java 的偏离（收口时记入 spec 偏离清单）：
1. 限流窗口语义：Java 切面为 Redis ZSET 滑动窗口（按请求逐条过期），Go ratelimit 为共享固定窗口（INCR+PEXPIRE）——既有 infra（resume 同用），票面仅要求核对汇报，数值（3/3、2/2）已对齐，窗口形态未改。
2. sortEntities 的 null 拆箱：Java 对 null fileSize/accessCount/questionCount 比较时 NPE，Go 按 0 参与排序（正常数据列值非空，行为不可区分）。
3. PUT category 请求体非法 JSON：Go 返回 400「请求参数错误」；Java 走全局兜底为 500「系统繁忙」（前端恒发合法 JSON，不可达路径）。
4. 下载文件名编码自实现 javaURLEncode（URLEncoder.encode+replace("+","%20") 逐字节等价，表驱动用例锁定）；上传/下载时间字段为 Go RFC3339（带时区后缀）vs Java LocalDateTime ISO（无时区），前端按字符串透传展示。
5. search 缺 keyword 参数：Java @RequestParam 必填返回参数错误，Go 空参按空白关键词返回全量列表（前端恒带参）。
