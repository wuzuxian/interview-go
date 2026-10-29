# 04: 阶段四 · 过期巡检 sweeper + smoke -mode schedule + 文档收口

**What to build:**
复刻 ScheduleStatusUpdater：批量把 interview_time 已过仍 PENDING 的日程置 CANCELLED（对齐 Java 每小时 cron → Go 间隔入 Config 默认 1h、now 注入、Start/Stop 骨架、main.go 接线；JPQL 批量 UPDATE 不触发 @PreUpdate → updated_at 不变的细节对齐）。cmd/smoke 加 -mode schedule 真库验收。收口文档：spec 偏离清单归并、GLOSSARY 节入库（随 spec 已建）、面试助手-架构依赖.md 登记。

实现细节以 `.scratch/interview-schedule/spec.md`「阶段四」为准。

**Blocked by:** 01（sweeper 与 smoke 都依赖 CRUD 与表映射在位；02/03 的规则解析样例可选纳入 smoke）

**Status:** ready-for-agent

- [ ] sweeper：一轮 = 单条批量 UPDATE（PENDING→CANCELLED WHERE interview_time < now），返回影响行数，>0 记日志「已将 N 条过期面试标记为已取消」；间隔 Config 默认 1h；不更新 updated_at（对齐 JPQL 批量语义）；Start(ctx)/Stop() 幂等，main.go Start + defer Stop
- [ ] sweeper 缝测试：fake Store 断言 old/new status 与 cutoff、影响行数、时钟注入；不复刻 Java 遗留死方法 findByStatusAndInterviewTimeBefore
- [ ] cmd/smoke `-mode schedule`：真库 CRUD 全链路（创建→详情→两过滤→更新→状态流转→删除）+ 过期取消验证（插过期 PENDING → 跑一轮 → 断言 CANCELLED）+ 规则解析三平台样例（不打真 LLM）+ 清理；不进 CI
- [ ] 架构依赖文档：第 3 层清单加 interview-schedule 行、§2 代码范围、§3 运行时依赖；smoke 行补 schedule 模式
- [ ] spec 偏离清单归并收口（预案内第 1 条 source 怪癖 + 各票实现中新增），票 Comments 留痕

## Comments
