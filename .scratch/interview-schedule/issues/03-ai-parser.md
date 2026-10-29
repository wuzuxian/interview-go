# 03: 阶段三 · AI 兜底解析 + 注入防御

**What to build:**
规则解析无效时走 LLM 兜底：PARSE_PROMPT 逐字移植（当前日期注入、ISO 8601 时间、默认 30 分钟、纯 JSON 返回），用户文本经 interview.PromptSanitizer 清洗 + 随机边界标签包裹 + DATA_BOUNDARY_INSTRUCTION 前缀；走 aibridge 默认 provider 普通 ChatModel（非 JSON 响应模式）；响应含 ``` 时正则提取 JSON 块，逐字段映射（trim、时间 16 位补 ":00"、roundNumber 失败回落 1）；成功 0.8/"ai"/「AI 解析成功」，失败全吞 0.0/"none"/「解析失败」。

实现细节以 `.scratch/interview-schedule/spec.md`「阶段三」为准。

**Blocked by:** 02（AI 兜底挂在规则解析失败之后）

**Status:** resolved

- [ ] PARSE_PROMPT 逐字移植（含两个 %s 槽位与组装顺序：currentDate、DATA_BOUNDARY_INSTRUCTION + "\n" + wrapWithDelimiters("parse-input", sanitize(rawText))）
- [ ] interview 包补导出 DataBoundaryInstruction（文本对齐 PromptSecurityConstants.DATA_BOUNDARY_INSTRUCTION，先例 AntiInjectionInstruction）
- [ ] 【访谈裁决 Q1，偏离清单第 1 条】AI 兜底一律走默认 Chat Provider；source 仅参与规则解析选择——不复刻 Java source→providerId 怪癖，代码注释注明对齐点与偏离
- [ ] 异常全吞语义：上游报错/超时/空 content/非 JSON → 返回 null → 「解析失败」形态，不向外抛
- [ ] 假上游缝测试：httptest 假 OpenAI 兼容服务端，锁 prompt 组装（边界标签出现、数据边界声明出现）、成功映射、```json 提取、字段回落、失败路径

## Comments

### 实现留痕（2026-10-29，feature/interview-schedule 直接提交）

验收全过，`go build/vet/test` 全绿。ai_parser.go（PARSE_PROMPT 逐字 + parseWithAI）+ interview 包导出 DataBoundaryInstruction + ParseService 扩充（models ChatModelFactory + sanitizer，镜像 kb ChatModelFactory 接缝形态，main 注入 bridge.RagChatModelFor）+ stub 模型测试 6 组。

实现要点：
1. 【偏离清单第 1 条落地】parseWithAI 签名保留 source 参数对位 Java，但不使用；一律默认 Provider（ai_parser.go 文件头注释详述怪癖与裁决）。
2. ChatModel 接缝为非流式 Generate（Java prompt().user().call()），**非** JSON 响应模式（kb 出题用 JSONChatModelFor 是 BeanOutputConverter 场景，此处 Java 是普通 ChatClient 文本调用，勿混淆）。
3. roundNumber 映射：`fmt.Sprintf("%v", v)` 后 Atoi，失败回落 1（对齐 Java toString+parseInt；JSON 数字在 encoding/json 为 float64，%v 输出 "2" 形态，行为等价）。
4. AI 时间 16 位补 ":00"（Java L350-353）；非法时间仅该字段留空（Java catch 后继续），三要素缺失则整体走「解析失败」。
5. PARSE_PROMPT 中的 "```json" 字样用 Go 字符串拼接规避原始字面量无法包含反引号序列的限制，字节内容逐字一致。
6. 测试踩坑记录：注入短语用例的文本若含「公司：/岗位：/时间：」标签会被规则解析成功而不触达 AI——AI 路径测试文本必须无结构。

