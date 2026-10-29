# 01: 阶段一 · CRUD 6 接口 + Entity/Repo + 契约测试

**What to build:**
求职者能创建（强制 PENDING）、查详情、按状态/时间范围过滤列表、整条更新（status 不变）、删除（物理删、不校验存在性）、更新状态（PATCH|PUT 双 method、无流转校验）。全部响应走统一 Result 封装，不存在 → 9001。

实现细节以 `.scratch/interview-schedule/spec.md`「阶段一」为准。

**Blocked by:** 无（首票）

**Status:** ready-for-agent

- [ ] `internal/interviewschedule` 独立包：config.go / model.go / repo.go（Store 接口 + *Repo）/ service.go / dto.go / controller.go；Entity 按 Java `interview_schedule` 列逐列映射（可空列指针、显式 column 标签、TableName()），不写迁移（ADR 0001）
- [ ] 路由挂 `/api/interview-schedule`：POST ""、GET "/{id}"、GET ""、PUT "/{id}"、DELETE "/{id}"、PATCH+PUT "/{id}/status"；main.go 注册一行
- [ ] 错误码 9001 补入 apperr（CodeInterviewScheduleNotFound「面试日程不存在」）；不存在消息带 id 后缀「面试日程不存在: {id}」；delete 不存在恒 success
- [ ] 列表三选一分支：start&end 同时非空 → Between（忽略 status）；否则 status 非空 → 按状态；否则全量；无分页无排序
- [ ] 时间序列化：DTO interviewTime 输出 `yyyy-MM-dd'T'HH:mm:ss` 无时区；请求接受 16/19 位两种；createdAt/updatedAt 同 ISO 无时区
- [ ] 校验：companyName/position/interviewTime 必填 → code=400 消息逗号拼接；status 非法取值的错误码与消息在实现票对照 Java GlobalExceptionHandler 逐字核实后落地
- [ ] HTTP 契约测试（主缝）锁全部接口路径/method、JSON 字段名、9001、时间格式、分支语义

## Comments
