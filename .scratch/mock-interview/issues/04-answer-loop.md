# 04: 逐题问答闭环

**Spec:** [../spec.md](../spec.md)（决策 8、9）

**What to build:** 打通单场作答闭环的四个端点。`GET /sessions/{id}/question` 返回当前题（首次调用把状态 CREATED→IN_PROGRESS，缓存与 DB 双写；全部答完返回 completed=true 与提示文案）；`PUT /sessions/{id}/answers` 暂存答案（按场次+题号 upsert 作答表，不推进索引）；`POST /sessions/{id}/answers` 提交答案并推进索引，末题提交自动交卷转 COMPLETED（票 06 在此触发评估入队，本票先留触发点）；`POST /sessions/{id}/complete` 提前交卷（仅未完成状态允许）。作答同时回写场次题目 JSON 中的 userAnswer（与 Java 行为一致）。

**Blocked by:** 03

**Status:** ready-for-agent

- [ ] 全流程 curl：创建 → 取题（状态转 IN_PROGRESS）→ 逐题提交 → 末题自动 COMPLETED
- [ ] 暂存不推进当前索引，重新取题仍是原题
- [ ] 作答表按（场次,题号）upsert：重复暂存/提交不产生重复行，answered_at 更新
- [ ] 提前交卷：未完成 → COMPLETED；已 COMPLETED/EVALUATED 再交卷返回业务错误（行为以 Java 源码为准）
- [ ] 全部答完后取题返回 completed=true 与提示文案（对齐 Java）
- [ ] 状态机流转有表驱动单测，非法流转被拒绝
