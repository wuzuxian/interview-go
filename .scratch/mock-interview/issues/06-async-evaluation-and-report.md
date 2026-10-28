# 06: 异步评估接入 + 报告/明细端点

**Spec:** [../spec.md](../spec.md)（决策 10 的异步与端点部分、决策 14）

**What to build:** 复用简历模块的 Redis Stream 消费基座，新建评估流（MAXLEN 1000、消费组）：交卷/末题提交时先置 PENDING 再入队；消费者跳过非 PENDING、置 PROCESSING、从 DB 重放会话与作答（评估用简历文本取自 DB 关联，不用请求参数）→ 调票 05 引擎 → 保存报告（总分/总评语/优势/改进/参考答案落列）→ 状态转 EVALUATED、逐题回填作答表（score/feedback/reference_answer/key_points）；失败重试至多 3 次（重新入队），最终失败落 evaluate_error、状态 FAILED。上线 `GET /sessions/{id}/report`（COMPLETED/EVALUATED 可查，含同步兜底评估路径，与 Java 一致）与 `GET /sessions/{id}/details`（题目 JSON 列 + 作答表组装，未答题生成空记录）。前端经列表/详情轮询 evaluateStatus。

**Blocked by:** 05（使用评估引擎）、04（交卷触发点）

**Status:** ready-for-agent

- [ ] 末题提交/交卷后 evaluateStatus 经 PENDING → PROCESSING → COMPLETED，场次最终转 EVALUATED
- [ ] 报告 DTO 字段与 Java 契约逐字一致（总分/分类得分/逐题/优势/改进/参考答案）
- [ ] 作答表回填 score/feedback/reference_answer/key_points；未答题 score=0 且标注未回答
- [ ] 消费失败重试 3 次后置 FAILED 且 evaluate_error 落库可查（stub 引擎失败驱动）
- [ ] 服务重启后消费恢复（基座的 pending 认领），消息不丢失
- [ ] details 对未答题生成空记录，明细字段与 Java 一致
- [ ] report 对 COMPLETED（未评估）场次走同步兜底评估路径
