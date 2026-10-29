# 04: 阶段四 · 过期巡检 sweeper + smoke -mode schedule + 文档收口

**What to build:**
复刻 ScheduleStatusUpdater：批量把 interview_time 已过仍 PENDING 的日程置 CANCELLED（对齐 Java 每小时 cron → Go 间隔入 Config 默认 1h、now 注入、Start/Stop 骨架、main.go 接线；JPQL 批量 UPDATE 不触发 @PreUpdate → updated_at 不变的细节对齐）。cmd/smoke 加 -mode schedule 真库验收。收口文档：spec 偏离清单归并、GLOSSARY 节入库（随 spec 已建）、面试助手-架构依赖.md 登记。

实现细节以 `.scratch/interview-schedule/spec.md`「阶段四」为准。

**Blocked by:** 01（sweeper 与 smoke 都依赖 CRUD 与表映射在位；02/03 的规则解析样例可选纳入 smoke）

**Status:** resolved

- [ ] sweeper：一轮 = 单条批量 UPDATE（PENDING→CANCELLED WHERE interview_time < now），返回影响行数，>0 记日志「已将 N 条过期面试标记为已取消」；间隔 Config 默认 1h；不更新 updated_at（对齐 JPQL 批量语义）；Start(ctx)/Stop() 幂等，main.go Start + defer Stop
- [ ] sweeper 缝测试：fake Store 断言 old/new status 与 cutoff、影响行数、时钟注入；不复刻 Java 遗留死方法 findByStatusAndInterviewTimeBefore
- [ ] cmd/smoke `-mode schedule`：真库 CRUD 全链路（创建→详情→两过滤→更新→状态流转→删除）+ 过期取消验证（插过期 PENDING → 跑一轮 → 断言 CANCELLED）+ 规则解析三平台样例（不打真 LLM）+ 清理；不进 CI
- [ ] 架构依赖文档：第 3 层清单加 interview-schedule 行、§2 代码范围、§3 运行时依赖；smoke 行补 schedule 模式
- [ ] spec 偏离清单归并收口（预案内第 1 条 source 怪癖 + 各票实现中新增），票 Comments 留痕

## Comments

### 实现留痕（2026-10-29，feature/interview-schedule 直接提交）

验收全过，`go build/vet/test` 全绿。status_updater.go（Start/Stop 骨架 + CancelStore 窄接缝）+ repo.CancelExpiredPending + 巡检缝测试 4 组 + cmd/smoke/schedule.go（-mode schedule 十步）+ 文档收口。

实现要点与坑：
1. GORM API：单列免 hook 更新是 `UpdateColumn(col, val)`；`UpdateColumns(...)` 是多变体形式（map/struct），签名不同——jpql 批量语义用前者（绕过 hooks 与 updated_at 自动更新）。
2. 首轮不立即执行：Java cron 语义首个触发点在下一个整点，Go ticker 首轮在一个间隔后（voice 不活动巡检是 fixedRate 立即首轮，两者语义不同，勿混淆）。
3. CancelStore 取 Store 的单方法子集（巡检器窄依赖），契约测试的 fakeStore 无需跟随扩充。
4. smoke 过期取消步依赖服务器短间隔启动（`APP_APP_INTERVIEW_SCHEDULE_SWEEPER_INTERVAL=2s`），`-sweeper-wait`（默认 30s，0 跳过）轮询过期行 PENDING→CANCELLED；默认 1h 间隔下该步会超时失败，属预期。
5. smoke 解析样例只覆盖飞书/腾讯（规则可解）；纯 Zoom 文本因 Zoom 规则不提取公司岗位必走 AI 兜底，不打真 LLM 的冒烟不覆盖（AI 兜底已由单测覆盖，票 03）。
6. 文档收口：面试助手-架构依赖.md 登记（第 3 层 interview-schedule 行、smoke 行补 schedule 模式、§2 代码范围 15 文件、§3 依赖边与 PG 表清单补 interview_schedule）；config.yaml 补 app.interview_schedule 段；spec 偏离清单 9 条收口（预案内 2 条 + 实现期 7 条，均为不可达路径或既有评审口径）。

