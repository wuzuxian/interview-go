# 01: 语音面试会话域与 REST 契约

**What to build:** 求职者可以通过 REST 管理语音面试场次：创建（方向/难度/简历/四阶段开关/计划时长/LLM 提供方，返回 webSocketUrl）、查单场、暂停、恢复、列表（可按状态过滤）、删除（级联删消息与评估）、拉取消息列表、结束（置 COMPLETED、写实际时长、评估状态置 PENDING 并在事务提交后投递评估流消息）。会话走 Redis 缓存（TTL 1h）。全部 DTO 字段名、默认值、错误语义与 Java 逐字对齐。本票同时为后续票（网关、管道、评估）提供会话服务、消息仓储与 `app.voice_interview` 配置段基础。

**Blocked by:** None（可立即开始）

**Status:** resolved

- [ ] POST /api/voice-interview/sessions 响应字段 sessionId/roleType/currentPhase/status/startTime/plannedDuration/webSocketUrl 逐字对齐；webSocketUrl 为相对路径 `/ws/voice-interview/{id}`
- [ ] 创建默认值对齐：introEnabled=false、tech/project/hrEnabled=true、plannedDuration=30、skillId 缺省取默认方向、roleType 向后兼容可空；difficulty 取值 junior/mid/senior
- [ ] GET 单场不存在时错误消息对齐 Java（"Session not found: ..."）
- [ ] pause 仅 IN_PROGRESS 可暂停（否则业务错误），落 pausedAt、失效缓存、不断连；resume 仅 PAUSED 可恢复，落 resumedAt、重建缓存，返回含 webSocketUrl 的完整响应
- [ ] 列表支持 userId/status 过滤，SessionMetaDTO 十字段（sessionId/roleType/status/currentPhase/createdAt/updatedAt/actualDuration/messageCount/evaluateStatus/evaluateError）对齐；userId 缺省固定默认用户
- [ ] DELETE 级联删除消息与评估记录
- [ ] GET messages 返回 VoiceInterviewMessageDTO 八字段（id/sessionId/messageType/phase/userRecognizedText/aiGeneratedText/timestamp/sequenceNum），按 sequenceNum 有序
- [ ] POST end 置 COMPLETED + endTime + actualDuration（秒）+ evaluate_status=PENDING，事务提交后向 voice:evaluate:stream 投递（无消费者时静默堆积，不报错）
- [ ] Redis 会话缓存键 voice:interview:session:{id}，TTL 1h，读写路径对齐 Java（读命中、写后更新/失效）
- [ ] GORM Entity 按三张语音表 Java 列名定义（timestamp 保留字列显式映射）；应用不做建表迁移（ADR 0001）
- [ ] 契约测试：全部请求/响应 DTO 的 JSON 字段名 golden 对齐
- [ ] app.voice_interview 配置段落位（本票所需默认值，snake_case）
