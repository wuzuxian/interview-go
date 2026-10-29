# 02: 阶段二 · 规则解析器 + POST /parse（规则路径）

**What to build:**
`POST /api/interview-schedule/parse`：粘贴面试邀约文本（可选 source=feishu|tencent|zoom|other），三套正则规则解析出公司、岗位、时间、会议链接、轮次（interviewType 一律 VIDEO）；source 指定直走对应解析器，无 source 走特征探测再逐个兜底；产物三要素（公司/岗位/时间）齐全才算成功（confidence 0.95 / parseMethod "rule" / log "规则解析成功"），否则留待票 03 的 AI 兜底（本票先返回解析失败形态）。空文本 → 固定失败形态「输入文本为空」。

实现细节以 `.scratch/interview-schedule/spec.md`「阶段二」为准（三套正则逐字清单在 spec 内）。

**Blocked by:** 01（parse 响应复用 CreateInterviewRequest DTO 与统一 Result 封装，随票 01 路由注册）

**Status:** resolved

- [ ] 三套正则逐字移植（Java Pattern → Go regexp 直译），三平台解析器 + tryRuleParsing 选择顺序（source → 特征探测 → 逐个兜底）
- [ ] isValidResult：companyName/position/interviewTime 三者非空；规则成功 0.95/"rule"/"规则解析成功"
- [ ] parseDateTime（`/`→`-`、16/19 位、ISO 兜底、失败留空不中断）与 parseRoundNumber（纯数字/中文数字表/兜底 1）
- [ ] parse 接口入参校验：rawText 必填（「文本不能为空」→ 400）；空文本失败形态 0.0/"none"/「输入文本为空」
- [ ] 纯函数缝表驱动测试：三平台真实样例文本、特征探测、逐个兜底、无效产物回落、多格式时间、中文数字轮次（一~十）

## Comments

### 实现留痕（2026-10-29，feature/interview-schedule 直接提交）

验收全过，`go build/vet/test` 全绿。parser.go（三套正则 + tryRuleParsing/isValidResult/parseDateTime/parseRoundNumber）+ parse_service.go（编排与失败形态）+ 契约测试（parse 路由）+ 纯函数表驱动测试。

按 Java 原文核实的契约事实（逐字照抄，勿当 bug"修正"）：
1. 腾讯 meetingLink 用 `matcher.group()`（全匹配，含「会议号：」标签本身），产物形如「会议号: 会议号：123456789 密码: 密码：1234」——前缀重复拼入，前端原样展示。
2. parseDateTime 按**字符串长度**分发：16/19 位只认空格分隔（`2026-04-10T14:30`、`2026-04-10T14:30:45` 均失败返回 null），ISO 兜底 `LocalDateTime.parse` 仅对其他长度生效（Go 补分数秒 layout 对齐其能力面）。
3. parseRoundNumber 只识别单个数字字符：「第十二轮」命中「十」→ 10。
4. Zoom 规则不提取公司/岗位：纯 Zoom 文本规则产物必无效，落到逐个兜底仍无效后走 AI（票 03）。
5. `source` 忽略大小写（equalsIgnoreCase）；「feishu/Feishu/meeting.feishu.cn」等特征探测为大小写敏感 contains。
6. 服务层「输入文本为空」分支经 HTTP 不可达（控制器 @NotBlank 先拦 400「文本不能为空」），按 Java 原样保留。
7. 腾讯规则无轮次提取；三套规则 interviewType 一律 VIDEO。

