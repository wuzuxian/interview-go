# 03: 阶段三 · AI 兜底解析 + 注入防御

**What to build:**
规则解析无效时走 LLM 兜底：PARSE_PROMPT 逐字移植（当前日期注入、ISO 8601 时间、默认 30 分钟、纯 JSON 返回），用户文本经 interview.PromptSanitizer 清洗 + 随机边界标签包裹 + DATA_BOUNDARY_INSTRUCTION 前缀；走 aibridge 默认 provider 普通 ChatModel（非 JSON 响应模式）；响应含 ``` 时正则提取 JSON 块，逐字段映射（trim、时间 16 位补 ":00"、roundNumber 失败回落 1）；成功 0.8/"ai"/「AI 解析成功」，失败全吞 0.0/"none"/「解析失败」。

实现细节以 `.scratch/interview-schedule/spec.md`「阶段三」为准。

**Blocked by:** 02（AI 兜底挂在规则解析失败之后）

**Status:** ready-for-agent

- [ ] PARSE_PROMPT 逐字移植（含两个 %s 槽位与组装顺序：currentDate、DATA_BOUNDARY_INSTRUCTION + "\n" + wrapWithDelimiters("parse-input", sanitize(rawText))）
- [ ] interview 包补导出 DataBoundaryInstruction（文本对齐 PromptSecurityConstants.DATA_BOUNDARY_INSTRUCTION，先例 AntiInjectionInstruction）
- [ ] 【访谈裁决 Q1，偏离清单第 1 条】AI 兜底一律走默认 Chat Provider；source 仅参与规则解析选择——不复刻 Java source→providerId 怪癖，代码注释注明对齐点与偏离
- [ ] 异常全吞语义：上游报错/超时/空 content/非 JSON → 返回 null → 「解析失败」形态，不向外抛
- [ ] 假上游缝测试：httptest 假 OpenAI 兼容服务端，锁 prompt 组装（边界标签出现、数据边界声明出现）、成功映射、```json 提取、字段回落、失败路径

## Comments
