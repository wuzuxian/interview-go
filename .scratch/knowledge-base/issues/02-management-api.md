# 02: 阶段一 · 知识库管理接口（13 个收尾）

**What to build:**
求职者能在前端知识库页完成全部日常管理：列表（按向量状态过滤、按字段排序）、详情、关键词搜索、统计总览、分类列表与按分类浏览、未分类、给知识库设置分类、下载原始文件、删除知识库。删除时连带清理向量、存储文件与 RAG 会话关联，且任何一步失败不产生脏数据。

实现细节以 `.scratch/knowledge-base/spec.md` 的「阶段一：知识库核心（管理 REST）」为准。

**Blocked by:** 01（上传与向量入库先在，接口才有可验收的真实数据路径）

**Status:** ready-for-agent

- [ ] list 支持 vectorStatus 过滤与 sortBy（null 或 "time" 为时间序，其余取值语义对照 Java sortEntities 实现）；详情、search、stats（totalCount/totalQuestionCount/totalAccessCount/completedCount/processingCount）、categories、category/{category}、uncategorized、set-category、download（Content-Disposition 带 UTF-8 编码文件名）的响应 DTO 字段与 Java 逐字对齐
- [ ] 删除补偿顺序：事务内先摘 RAG 会话关联 → 删 DB 记录 → 事务外删向量（按 metadata kb_id，失败不阻断仅记日志）→ 删 S3 文件（失败留 storageKey 供人工补偿）
- [ ] 错误码 6xxx 段对齐 Java ErrorCode：6001 不存在、6002 解析失败、6004 查询失败、6005 删除失败、6006 向量化失败；6003 在 Java 侧即缺号，保持缺口
- [ ] 全部接口走统一 Result 封装；限流数值对齐 Java 注解（窗口语义按 Java 切面实现核对，实现票时确认）
- [ ] HTTP 契约测试（主缝）锁全部接口的请求/响应 JSON 字段名与错误码
