# Spec: 面试日程复刻

Status: ready-for-agent
Created: 2026-10-29
Origin: /grill-with-docs 设计访谈（6 问裁决，全部采推荐项；GLOSSARY「面试日程」节）
Tracker: 本地 Markdown（约定见 docs/agents/issue-tracker.md）

## Problem Statement

Java 版 interview-guide 的面试日程模块（interviewschedule）在 Go 版整体缺失：求职者无法记录面试邀约（公司、岗位、时间、形式、会议链接、轮次、面试官、备注），无法在日历/列表视图管理面试安排，也无法把飞书/腾讯会议/Zoom 的邀约文本一键解析成日程。React 前端的面试日程页（react-big-calendar，拖拽改期）已按 Java 契约实现且不再改动；后端缺位意味着该页面不可用。

源模块是全仓库自包含度最高的业务模块：与简历、文字面试、语音面试、知识库**零数据关联**（`interview_schedule` 表无外键、无 user_id，全仓库无其他 Java 文件引用该模块）。复刻为独立小模块即可，不需要任何跨模块业务接缝。

## Solution

新增 `internal/interviewschedule` 模块，复刻全部 7 个 HTTP 接口（基路径 `/api/interview-schedule`）+ 1 个每小时过期巡检任务，另在 `cmd/smoke` 增加 `-mode schedule` 真库验收。协议逐字对齐 Java（统一 Result 封装、错误码 9001、DTO 字段与时间序列化格式），表沿用 Java 共库 schema 不做迁移（ADR 0001）。解析链为「三平台正则规则优先（confidence 0.95）→ LLM 兜底（0.8）→ 失败（0.0）」，注入防御复用 interview 模块 PromptSanitizer 设施。

## User Stories

1. 作为求职者，我想把收到的面试邀约文本（飞书/腾讯会议/Zoom 格式）粘贴进来一键解析出公司、岗位、时间、会议链接，以便不必逐字段手工录入。
2. 作为求职者，解析失败或邀约格式不标准时，我想手动填表创建日程，以便任何面试都能记录。
3. 作为求职者，我想在列表中查看全部面试安排、按状态过滤、按时间范围（日历视图拉取数据）过滤，以便掌握近期安排。
4. 作为求职者，我想查看/编辑单条日程的全部业务字段，以便信息有变时更新。
5. 作为求职者，我想删除不再需要的日程，以便列表不留垃圾。
6. 作为求职者，我想把待面试的日程标记为已完成或已取消，以便状态与实际一致。
7. 作为求职者，拖拽改期时前端以新时间整条提交更新，后端只需支持整条更新不改状态。
8. 作为求职者，面试时间已过仍待面试的日程应被系统自动置为已取消，以便列表无需手动清理陈旧项。

## Implementation Decisions

总体：独立包 `internal/interviewschedule`（同包平铺 config/model/repo/service/parse/dto/controller/updater），复用既有 infra（result/apperr/httpx Recovery、aibridge 默认 Chat 通道、interview.PromptSanitizer）；无 Redis Stream、无存储/向量依赖。四票交付，票间线性依赖。

### 阶段一（票 01）：CRUD 6 接口 + Entity/Repo + 契约测试

- **错误码**：apperr 补 `9001 CodeInterviewScheduleNotFound`「面试日程不存在」（Java 9xxx 段唯一错误码）；`getById`/`update`/`updateStatus` 对不存在 id 抛 9001，消息「面试日程不存在: {id}」（对齐 Java BusinessException 携带 id 后缀）。`delete` 不校验存在性：删除不存在 id 同样返回 success（Java deleteById 语义）。
- **Entity**：GORM 按 Java `interview_schedule` 列逐列映射（id identity 主键、company_name/position/interview_time/status NOT NULL、interview_type/round_number/interviewer 可空、meeting_link/notes TEXT 可空、created_at/updated_at 可空）；可空列一律指针；`InterviewStatus` 自定义 string 枚举四值 PENDING/COMPLETED/CANCELLED/RESCHEDULED；`TableName()` 返回 `interview_schedule`；**不写迁移**（ADR 0001，表由 Java Flyway V1 建，应用启动不负责创建）。createdAt/updatedAt 由应用维护（创建刷两者、每次更新刷 updatedAt），语义对齐 Java `@PrePersist/@PreUpdate`。
- **创建**：拷贝 8 业务字段 + **强制 status=PENDING**（客户端传什么都不看）；roundNumber 请求缺省为 1（`*int` nil → 1）。`interviewType` 纯字符串存储不校验取值（Java 现状：无枚举校验）。
- **更新**：同样 8 业务字段，**status 恒不变**（只能经状态接口改）；不存在 → 9001。
- **列表**：三选一分支逐字复刻——`start` 与 `end` 同时非空 → `interview_time BETWEEN start AND end`（范围优先，status 被忽略）；否则 `status` 非空 → 按 status 过滤；否则全量。无分页、无排序。status 非法取值 → 400（Java `valueOf` 抛 IllegalArgumentException；最终错误码与消息格式在实现票对照 GlobalExceptionHandler 核实后落地）。
- **状态更新**：`PATCH|PUT /{id}/status?status=XXX` 双 method 注册；query 参数必填、取值限四枚举（非法 → 400，同上核实）；任意状态可跳任意状态，**无状态机校验**（Java 现状）；RESCHEDULED 仅保留取值，无任何接口会自动置它（前端拖拽改期走整条 PUT）。
- **时间序列化**：DTO `interviewTime` 输出格式 `yyyy-MM-dd'T'HH:mm:ss`（无时区后缀）；请求 `interviewTime` 接受 `yyyy-MM-dd'T'HH:mm` 或带 `:ss`（对齐 Java `@JsonFormat(pattern="yyyy-MM-dd'T'HH:mm[:ss]")`，Go 自定义 Unmarshal）；`createdAt`/`updatedAt` 输出同 ISO 无时区格式（Java LocalDateTime Jackson 默认）。全链路无时区概念，存什么是什么。
- **参数校验**：创建/更新 companyName/position/interviewTime 必填，失败 → code=400、消息为字段错误文案逗号拼接（对齐 Java `@Valid` + GlobalExceptionHandler；具体拼接分隔符在实现票对照 Java 原文逐字落地）。parse 的 rawText 必填（「文本不能为空」）。
- **响应**：全部走 `result.OK/FailErr`；删除成功 data=null。

### 阶段二（票 02）：规则解析器 + POST /parse（规则路径）

- **接口**：`POST /parse`，body `{rawText 必填, source 可选(feishu|tencent|zoom|other)}`；响应 `ParseResponse{success, data?=CreateInterviewRequest, confidence, parseMethod, log}`。
- **空文本**（nil/trim 后空）→ `{success:false, data:null, confidence:0.0, parseMethod:"none", log:"输入文本为空"}`。
- **三套正则逐字移植**（Java Pattern → Go regexp，语法兼容直译）：
  - 飞书：时间 `(?:时间|时段)[：:]\s*(\d{4}[-/]\d{2}[-/]\d{2}\s+\d{2}:\d{2})`、链接 `https://meeting\.feishu\.cn/[^\s\n]+`、公司 `(?:公司|单位|组织)[：:]\s*([^\s\n]{1,50})`、岗位 `(?:岗位|职位|职务)[：:]\s*([^\s\n]{1,50})`、轮次 `第\s*[一二三四五六七八九十\d]+\s*[轮场]`。
  - 腾讯会议：时间 `(\d{4}[-/]\d{2}[-/]\d{2})\s+(\d{2}:\d{2})`、会议号 `(?:会议号|ID)[：:]?\s*(\d{9,})`、密码 `密码[：:]?\s*(\d{4,})`（两者拼为「会议号: X 密码: Y」存 meetingLink）、公司/岗位正则同飞书去「组织」「职务」变体。
  - Zoom：链接 `https://zoom\.us/j/[^\s\n]+`、日期 `(\d{4}[-/]\d{2}[-/]\d{2})`、时刻 `(\d{1,2}:\d{2})`。
  - 三套规则的 interviewType 一律置 "VIDEO"（Java 现状）。
- **选择顺序**：source 指定（feishu/tencent/zoom，忽略大小写）→ 直接走对应解析器；无 source → 文本特征探测（含"飞书"/"Feishu"/"meeting.feishu.cn" 依次试飞书、含"腾讯会议"/"Tencent Meeting"/"会议号" 试腾讯、含"Zoom"/"zoom.us" 试 Zoom，各自成功即返回）→ 最后无条件逐个兜底（feishu → tencent → zoom）。
- **有效性判定** `isValidResult`：companyName、position、interviewTime 三者非空。规则成功 → `{success:true, data, confidence:0.95, parseMethod:"rule", log:"规则解析成功"}`；规则产物无效则继续 AI 兜底。
- **时间解析** `parseDateTime`：`/`→`-` 后按长度分发（16 位 `yyyy-MM-dd HH:mm`、19 位带秒、否则 ISO 尝试）；失败该字段留空并记日志（Java catch 返回 null 语义，不中断其他字段提取）。
- **轮次解析** `parseRoundNumber`：纯数字直接取；否则用 `[一二三四五六七八九十]|\d` 取首个匹配查中文数字表；兜底 1。

### 阶段三（票 03）：AI 兜底解析 + 注入防御

- **触发**：规则解析无效后进行；prompt 逐字移植 Java `PARSE_PROMPT`（含当前日期注入槽、ISO 8601 要求、默认 30 分钟时长说明、纯 JSON 返回要求与示例）；组装顺序对齐 Java：`String.format(PARSE_PROMPT, currentDate, DATA_BOUNDARY_INSTRUCTION + "\n" + wrapWithDelimiters("parse-input", sanitize(rawText)))`。
- **注入防御**：复用 `interview.PromptSanitizer`（Sanitize 四正则 + WrapWithDelimiters 随机边界标签，跨模块复用先例 voiceinterview/knowledgebase）；`DATA_BOUNDARY_INSTRUCTION` 常量在 interview 包补导出别名（先例 AntiInjectionInstruction），文本逐字一致。
- **LLM 通道**：走 aibridge 默认 provider 的普通 ChatModel（等价 Java `getChatClientOrDefault` 默认分支；**非** JSON 响应模式——Java 是普通文本补全 + 手工提取 JSON）。
- **【访谈裁决 Q1，偏离清单第 1 条】** Java 把 `source` 直接当 providerId 传注册表（source=feishu/tencent/zoom 时按 id 直查 provider，几乎必然不存在→异常被吞→解析失败），是缺陷而非特性。Go 修正为：**AI 兜底一律用默认 Chat Provider**，`source` 仅参与规则解析的格式选择。其余行为（异常全吞、失败返回 null 走「解析失败」）保持一致。
- **响应解析**：content 空 → 失败；含 ``` 时用 ```(?:json)?\s*([\s\S]*?)\s*``` 提取首块；map 逐字段映射（全部 trim；interviewTime 长度 16 补 ":00" 再解析、解析失败仅该字段留空；roundNumber 解析失败回落 1）。
- **置信度语义**：AI 成功 `{confidence:0.8, parseMethod:"ai", log:"AI 解析成功"}`；全失败 `{success:false, data:null, confidence:0.0, parseMethod:"none", log:"解析失败"}`。

### 阶段四（票 04）：过期巡检 sweeper + smoke + 文档收口

- **sweeper**：对齐 Java `ScheduleStatusUpdater`（每小时 cron）。Go 按 voice 不活动巡检先例：`Start(ctx)/Stop()` 骨架 + 间隔进模块 Config（默认 1h，`config`/`default` 标签）+ `now` 注入。一轮 = 单条批量 UPDATE：`status: PENDING→CANCELLED WHERE interview_time < now`，返回影响行数；>0 记日志「已将 N 条过期面试标记为已取消」。**细节对齐**：Java JPQL 批量 UPDATE 绕过 `@PreUpdate`，`updated_at` 不变——Go 用不触发 GORM hook/`updated_at` 自动更新的列更新方式逐字对齐。`findByStatusAndInterviewTimeBefore`（Java 遗留死方法）不复刻。
- **smoke**：`cmd/smoke` 加 `-mode schedule`：真库 CRUD 全链路（创建→详情→列表两过滤→更新→状态流转→删除）+ 过期取消验证（插入 interview_time 在过去的 PENDING 行、跑一轮巡检、断言置 CANCELLED）+ 规则解析三平台样例文本（不打真 LLM）+ 清理。不进 CI，由维护者真实环境执行收口。
- **文档收口**：偏离清单归并（见下）；`面试助手-架构依赖.md` 登记 interview-schedule 模块（第 3 层清单 + §2 代码范围 + §3 依赖）与 smoke 行更新；GLOSSARY「面试日程」节随本 spec 一并入库。

## Testing Decisions

沿 kb/voice spec 五缝，本模块主缝是 HTTP 契约缝 + 纯函数缝：

1. **HTTP 契约缝（主缝）**：httptest 真路由 + Store 接口 fake，锁 7 接口路径与方法（含 PATCH+PUT 双注册）、请求/响应 JSON 字段名、9001 错误码与消息、时间序列化格式（无时区 `yyyy-MM-dd'T'HH:mm:ss`）、校验失败 400 文案、列表三选一分支语义。先例：internal/knowledgebase/contract_test.go。
2. **纯函数缝**：三平台解析器表驱动（真实样例邀约文本：飞书含中文数字轮次、腾讯含会议号+密码、Zoom、特征探测、逐个兜底、无效产物回落）、parseDateTime 多格式、parseRoundNumber、JSON 代码块提取。中文用例名。
3. **假上游缝**：httptest 假 OpenAI 兼容服务端测 AI 兜底：prompt 含边界标签与数据边界声明、字段映射与回落、非 JSON/空返回/上游报错全吞为「解析失败」。先例：voiceinterview 假 DashScope、kb 假上游。
4. **sweeper 缝**：fake Store 断言 old/new status 与 cutoff 参数、影响行数日志阈值、Stop 幂等。时钟注入。先例：inactivity_sweeper 测试。
5. **真库缝**：cmd/smoke `-mode schedule`（见阶段四）。

质量门：每票 `go build ./... && go vet ./... && go test ./...` 全绿 + 票 Comments 留痕；全部完成后 code-review（两轴：Standards + Spec 对照票面）。

## Out of Scope

- 前端任何改动（前端已按 Java 契约实现）。
- 数据库迁移：`interview_schedule` 沿用 Java schema（ADR 0001）；表不存在时先跑 Java Flyway 或手工建表，不在 Go 应用做。
- 时区处理、时间冲突检测、提醒/通知、日程到点触发模拟面试——Java 均无，非省略；模块与文字/语音面试零联动是源设计。
- 状态机流转校验、CONFIRMED 状态（Java 无）。
- `findByStatusAndInterviewTimeBefore` 死方法复刻。
- 限流中间件（Java 日程控制器无 @RateLimit 注解，已核实）。

## Further Notes

- **偏离清单**：实现完成后在本文件末尾补「与 Java 行为的有意偏离」完整清单（先例：mock 11 条、voice 11 条、kb 31 条）。已知预案内偏离：Q1 裁决的 source→providerId 修正。
- **术语**：遵循 GLOSSARY「面试日程」节——日程与面试场次是两个东西，页面叫「面试日程」，练的那场叫「面试场次」。
- **验收**：每票 TDD 自验；全部完成后 code-review；最终以 smoke schedule 模式真链路通过收口。
- **JPQL 细节**：批量 UPDATE 不触发 @PreUpdate（updated_at 不变）——实现票按对齐细节处理，非偏离。

## 与 Java 行为的有意偏离（收口清单，2026-10-29）

四票完成后按票 Comments 归并收口。前 2 条为预案内偏离。

**预案内**

1. AI 兜底 provider 选择修正（访谈裁决 Q1）：Java 把 `source` 直接当 LLM providerId 传注册表（parse 调用点 L116/L306），source=feishu/tencent/zoom 时按 id 直查 Provider 几乎必然不存在、异常被吞、AI 兜底失效——缺陷而非特性；Go 一律走默认 Chat Provider（bridge.RagChatModelFor，等价 Java getChatClientOrDefault 默认分支），`source` 仅参与规则解析的格式选择。（票 03）
2. 过期巡检节奏（访谈裁决 Q3-A）：Java `@Scheduled(cron = "0 0 * * * ?")` 每小时整点；Go 间隔节拍（Config `sweeper_interval` 默认 1h，首轮在一个间隔之后、不立即执行，对齐 cron 不立即触发的语义）。行为等价性以 smoke `-mode schedule` 过期取消步验收（服务以 `APP_APP_INTERVIEW_SCHEDULE_SWEEPER_INTERVAL=2s` 短间隔启动）。（票 04）

**实现期新增（均不可达路径 / 评审统一口径）**

3. 列表 status 非法值：Java `InterviewStatus.valueOf` 抛 IllegalArgumentException、GlobalExceptionHandler 有专 handler → code=400 + 枚举英文内部消息；Go 400「无效的面试状态: 原值」。（票 01；kb 偏离 22 同口径，前端恒发合法值）
4. updateStatus 的 status 参数缺失/非法值：Java 缺参（MissingServletRequestParameterException）与非法值（MethodArgumentTypeMismatchException）均无专门 handler，落全局兜底 500「系统繁忙」；Go 400「缺少必填参数: status」/「无效的面试状态: 原值」快速失败。（票 01；kb 偏离 29 评审口径）
5. 列表 start/end 时间参数非法：Java 类型转换失败落兜底 500；Go 400「无效的时间参数: 原值」。（票 01）
6. roundNumber 请求显式 null：Java Jackson setter 覆盖字段默认 1 → null 入库；Go nil → 1（显式 null 与缺失同路径，前端从不传 null）。（票 01）
7. createdAt/updatedAt 序列化：Java Jackson 对 LocalDateTime 微秒非零时输出带 .ffffff 小数节；Go 恒输出秒级无小数节（前端按字符串透传展示、不解析这两字段——kb 偏离 12 同口径）。（票 01）
8. 非法请求体 JSON / 时间字段格式非法：Java 走全局兜底 500「系统繁忙」；Go 400「请求参数错误」。（票 01；kb 偏离 11 既有口径）
9. Java 遗留死方法 `findByStatusAndInterviewTimeBefore` 不复刻（全仓库无调用）。（票 04）
10. AI 把必需字段返回为空串时：Java `isValidResult` 仅判非 null → 仍成功 0.8 并产出空公司名；Go 按空串无效落「解析失败」（Go string 无 null 语义，不值得为该边角引入存在性追踪；规则路径正则要求非空，不受影响）。（票 03，code-review 发现）

共 10 条。code-review 修复（2026-10-29）：parse 响应 roundNumber 对齐 Java 字段默认 1（原实现未命中轮次时输出 null）；parse 响应 interviewTime 对齐 @JsonFormat 可选秒（秒为 0 省略 :ss，新增 FlexTime）；smoke 补 Zoom 样例（特征探测→兜底链路）凑齐三平台；slog 键名统一英文、「状态机」等 GLOSSARY 禁用词清理；回退 voice 冒烟文件的顺手格式化。验收留痕：真链路 smoke（-mode schedule）由维护者在真实环境执行收口（过期取消步需短巡检间隔启动服务）。
