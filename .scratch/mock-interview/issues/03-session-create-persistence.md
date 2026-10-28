# 03: 面试场次创建与持久化（兜底题源）

**Spec:** [../spec.md](../spec.md)（决策 2、5（兜底部分）、7、8、15；ADR 0001）

**What to build:** interview_sessions / interview_answers 两表的 GORM Entity 完全按 Java 列名定义（ADR 0001：应用迁移**不覆盖**这两张表，schema 已存在于库中）；repo 层（含事务句柄透传、按需 FOR UPDATE）；Redis 场次缓存 TTL 24h（写路径双写、读路径缓存优先 DB 兜底）。上线创建面试场次：幂等三层（幂等键 DB 唯一约束 + Redis SETNX 锁 等待 185s/持有 600s + 结果键 TTL 1 天）、未完成场次复用（有简历且未 forceCreate 时返回旧场次，Redis 映射 24h）、题目来源本票为**静态兜底题**（从 Java 逐字移植，真实出题在票 07）、题量校验 3-20、skillId 默认 java-backend、difficulty 默认 mid、整场绑定 llmProvider。上线 `GET /sessions`（列表）、`GET /sessions/{id}`、`GET /sessions/unfinished/{resumeId}`、`DELETE /sessions/{id}`。所有 DTO 字段按 Java 契约**一次建全**（evaluateStatus/evaluateError/overallScore 等先为默认值，票 06 才有真实值）。apperr 新增 interview 段错误码；建立 app.interview 配置段。

**Blocked by:** 01（创建需校验方向存在）

**Status:** resolved

- [ ] `POST /sessions` 创建兜底题场次：状态 CREATED、题目为兜底题、默认值与 Java 一致
- [ ] 相同幂等键重复提交返回同一场次；并发请求不产生重复行（唯一约束兜底生效）
- [ ] 同简历再次创建返回未完成旧场次；forceCreate=true 绕过复用
- [ ] 列表/详情/unfinished 的 DTO 字段名与 Java 契约逐字一致（evaluateStatus 等先为默认值）
- [ ] DELETE 删除场次（级联删作答，当前无作答也不报错）
- [ ] Entity 列名与 Java V1 schema 一一对应（对照检查）
- [ ] Redis 缓存过期后读路径回源 DB 并重建缓存
- [ ] 幂等锁等待/持有超时、结果键 TTL 有配置项且默认值对齐 Java
