# 02: 阶段二 · 规则解析器 + POST /parse（规则路径）

**What to build:**
`POST /api/interview-schedule/parse`：粘贴面试邀约文本（可选 source=feishu|tencent|zoom|other），三套正则规则解析出公司、岗位、时间、会议链接、轮次（interviewType 一律 VIDEO）；source 指定直走对应解析器，无 source 走特征探测再逐个兜底；产物三要素（公司/岗位/时间）齐全才算成功（confidence 0.95 / parseMethod "rule" / log "规则解析成功"），否则留待票 03 的 AI 兜底（本票先返回解析失败形态）。空文本 → 固定失败形态「输入文本为空」。

实现细节以 `.scratch/interview-schedule/spec.md`「阶段二」为准（三套正则逐字清单在 spec 内）。

**Blocked by:** 01（parse 响应复用 CreateInterviewRequest DTO 与统一 Result 封装，随票 01 路由注册）

**Status:** ready-for-agent

- [ ] 三套正则逐字移植（Java Pattern → Go regexp 直译），三平台解析器 + tryRuleParsing 选择顺序（source → 特征探测 → 逐个兜底）
- [ ] isValidResult：companyName/position/interviewTime 三者非空；规则成功 0.95/"rule"/"规则解析成功"
- [ ] parseDateTime（`/`→`-`、16/19 位、ISO 兜底、失败留空不中断）与 parseRoundNumber（纯数字/中文数字表/兜底 1）
- [ ] parse 接口入参校验：rawText 必填（「文本不能为空」→ 400）；空文本失败形态 0.0/"none"/「输入文本为空」
- [ ] 纯函数缝表驱动测试：三平台真实样例文本、特征探测、逐个兜底、无效产物回落、多格式时间、中文数字轮次（一~十）

## Comments
