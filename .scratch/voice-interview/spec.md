# Spec: 模拟面试语音面试复刻

Status: ready-for-agent
Created: 2026-10-28
Origin: /grill-with-docs 设计访谈（共识总结 + ADR 0001 增补 / ADR 0002 增补 / ADR 0003）
Tracker: 本地 Markdown（约定见 docs/agents/issue-tracker.md）

## Problem Statement

Java 版 interview-guide 已停用，Go 版是唯一后端，但模拟面试只剩文字链路可用：求职者无法进行语音面试——实时对话、字幕、逐句语音回复、分阶段流程、暂停恢复与语音评估报告全部缺失。React 前端语音面试页已按 Java 版契约实现且不再改动，后端缺位意味着整条语音练习产品线不可用。

## Solution

在 Go 后端完整复刻语音面试：REST 会话管理（创建/查询/暂停/恢复/列表/删除/消息/评估）+ 原生 WebSocket 实时管道（用户音频 → ASR 实时字幕 → 提交 → 面试官模型流式回复 → 逐句 TTS 分块音频）+ INTRO→TECH→PROJECT→HR 阶段机 + 上下文压缩（窗口/摘要）+ 暂停超时与僵尸清理 + Redis Stream 异步评估。REST DTO 与 WS 消息契约与 Java 版逐字对齐，ASR/TTS 保持 DashScope 同模型同端点，前端零改动切换。

## User Stories

1. 作为求职者，我想创建一场语音面试（方向、难度、简历、四个阶段开关、计划时长、LLM 提供方），以便开始针对性的语音练习。
2. 作为求职者，创建成功后我想拿到 WebSocket 地址与当前阶段，以便前端立即建立实时连接。
3. 作为求职者，我想查看我的语音面试列表（可按状态过滤），以便找回历史场次。
4. 作为求职者，我想查看单场会话详情，以便了解当前阶段、状态与计划时长。
5. 作为求职者，我想暂停一场进行中的面试并附原因，以便中途休息。
6. 作为求职者，我想恢复一场已暂停的面试并重新拿到 WebSocket 地址，以便继续同一场的对话历史。
7. 作为求职者，我想删除一场面试并级联删除消息与评估，以便清理记录。
8. 作为求职者，我想拉取一场面试的完整消息列表（含阶段、识别文本与 AI 回复、序号），以便回顾对话。
9. 作为求职者，连接 WebSocket 后我想收到 welcome 控制消息，以便确认通道就绪。
10. 作为求职者，连接后我想自动收到面试官开场白（先文本后音频），以便不用手动触发就能开始说话。
11. 作为求职者，我说的话要实时显示为字幕（部分级随识别推进更新、最终级随话语拼接更新），以便确认被听清。
12. 作为求职者，ASR 未就绪或重连期间我想收到 asr_reconnecting 提示，以便理解为何暂时没有字幕。
13. 作为求职者，我想点提交后收到面试官的流式文本（按节流逐段推送、最终一次收尾），以便低等待感地看到回复。
14. 作为求职者，面试官回复要逐句以音频块按序下发（每块可独立播放），以便边收边播。
15. 作为求职者，一轮音频全部下发后我想收到 audio_complete，以便前端收尾播放状态。
16. 作为求职者，我可以在提交时携带修正后的文本（覆盖识别拼接结果），以便改掉口误再提交。
17. 作为求职者，面试官回复要做语音化清洗（去 Markdown/列表、压空白、超长截断到句子边界），以便听感自然。
18. 作为求职者，某句 TTS 失败时跳过该句而非整轮失败；全部失败时退回整段合成，以便尽量不丢语音。
19. 作为求职者，LLM 出错（认证/超时/限流/网络）时我想收到友好话术的错误消息且会话不中断，以便继续面试。
20. 作为求职者，面试官正在说话或刚说完的一小段窗口内我的麦克风输入被丢弃（回声冷却），以便 AI 的声音不被识别成我的回答。
21. 作为求职者，ASR 连接失效时自动重启并把该块重发，以便短暂故障不影响识别连续性。
22. 作为求职者，面试按 INTRO→TECH→PROJECT→HR 依阶段自动推进（按各阶段时长与题量判据），以便流程结构完整。
23. 作为求职者，前端可主动切换阶段（start_phase），以便跳过不适用的段落。
24. 作为求职者，我想主动结束面试（end_interview 或 REST end），以便立即进入评估。
25. 作为求职者，我不说话超过 4 分 30 秒收到预警、超过 5 分钟被自动暂停并断连，以便会话不悬挂。
26. 作为求职者，WebSocket 异常断开时进行中的会话被自动结束并触发评估，以便报告不丢。
27. 作为求职者，面试结束后我想轮询评估状态（PENDING/PROCESSING/COMPLETED/FAILED 与失败原因、状态更新时间），以便知道报告何时可看。
28. 作为求职者，评估失败时我想通过重试接口再次触发（完成态返回缓存结果），以便不必重考一场。
29. 作为求职者，全程没有对话的场次得到占位空评估而非失败，以便列表与详情不报错。
30. 作为求职者，语音评估报告的字段形状与文字面试明细一致（总分/总评语/优势/改进/逐题得分与反馈/参考答案/要点），以便前端复用同一报告面板。
31. 作为求职者，长对话历史被自动压缩（最近窗口 + 更早内容摘要），以便多轮后面试官不失忆且不超模型上下文。
32. 作为求职者，摘要生成失败时降级为最近窗口，面试不中断。
33. 作为运维，ASR/TTS 配置沿用设置页（已复刻的 /api/llm-provider/voice/*），修改后热生效，无需重启。
34. 作为开发者，僵尸会话与卡死评估被定时清理（超长未结束强制结束、未消费评估补投、处理中超时置败），以便状态不腐烂。
35. 作为开发者，我想用一条冒烟命令端到端验证语音链路（TTS 生成回答音频 → 重采样喂 ASR → 完整管道 → 交卷 → 轮询评估），以便发布前无麦克风自检。
36. 作为前端开发者，我希望 REST 字段名、WS 消息类型与字段名、全部枚举值与 Java 版逐字一致，以便零改动切换后端。

## Implementation Decisions

1. **模块结构**：新增 voiceinterview 业务模块（internal/voiceinterview），按仓库既有约定同包分层（controller / service / repo / model / dto 文件）；路由挂 /api 组（REST）与根级 /ws 组（WebSocket）；响应沿用 Result 包装，业务错误进 apperr 语音段（沿用已预留的 11xxx 语义，映射 Java 语音错误码 10001/10004/10006/11009-11011）。
2. **数据库（ADR 0001 增补）**：voice_interview_sessions / voice_interview_messages / voice_interview_evaluations 三张表完全沿用 Java Flyway schema（含 summary_covered_sequence_num 列与 session+sequence 联合索引），GORM Entity 按 Java 列名定义（注意 timestamp 列名需显式 column 标签），应用迁移不覆盖，已核实表在共享库中存在。消息类型只有 DIALOGUE 与 SUMMARY 两种值。
3. **ASR（ADR 0003）**：手写 DashScope OmniRealtime 协议客户端——连 wss、updateSession（modalities=TEXT、server_vad、静默时长取配置默认 2000ms）、事件解析（transcription.completed→最终、text/delta→部分、error→脱敏上报）、连接实例比对防旧连接 onClose 误删、ready 检查（10s × 2 次重试）、重启流程（stop + 200ms + start + 1s ready 验证）。模型 qwen3-asr-flash-realtime，运行时配置（URL/模型/语言/采样率/打断检测）与解密 Key 读已复刻的 voice_config 表。
4. **TTS（ADR 0003）**：手写 DashScope qwen3-tts-flash-realtime 客户端——每次合成新建 WebSocket 连接（commit 模式）、收集 response.audio.delta 为 PCM（24kHz/16bit/mono）、建连超时 5s、分句合成超时 8s（超时跳句）、整段合成 30s。后端手写 44 字节 WAV 头（24000Hz）包 base64 下发。
5. **LLM（ADR 0002 增补）**：eino ChatModel 直调的流式接口（Stream，OpenAI 兼容协议走既有 provider 注册表按场次 llmProvider 解析）。流式出句：token 累积中检测句边界（终止标点），逐句回调；节流推送实时文本（间隔 ≥180ms 且增量 ≥12 字符）。出题与追问完全由模型逐轮实时生成，无预生成题库；SKILL.md 方向内容静态嵌入 system prompt（Java 用 SkillsTool 工具调用加载，属有意偏离），复用 interview 模块已嵌入的 Skill 资源子系统（跨模块引用，对齐 Java voice→InterviewSkillService 结构）。Prompt 从 Java 逐字移植：面试官人设 + 语音约束（每轮 1 主问 + ≤1 追问、2-4 句、禁 Markdown）+ 简历文本（分隔符包裹）+ 防注入指令 + 上下文（【之前的对话】/【当前对话】），历史逐条过既有 PromptSanitizer。
6. **WebSocket 网关**：gorilla/websocket（新增直接依赖）；gin 路由 /ws/voice-interview/:sessionId，升级时校验来源（沿用 CORS 白名单）；消息上限 256KB、发送带 10s 写超时与积压上限（超限断开）、会话关闭后丢弃后续发送。上行两类：audio（base64 PCM，转发 ASR）与 control（submit 可带修正文本 / end_interview / start_phase）。下行六类消息形状逐字对齐：control（welcome、asr_ready、asr_reconnecting、audio_complete、pause_timeout_warning、pause_timeout）、subtitle(text,isFinal)、text(content,final)、audio(data,text)、audio_chunk(data,index,isLast)、error(message)。
7. **管道并发模型**：每会话独立状态（mutex 保护）：ASR 最终结果只拼入话语合并缓冲并更新字幕，不自动触发模型；提交时 CAS 抢处理锁（失败短暂等待重试）防双答；LLM 与 TTS 链路在独立 goroutine 执行；逐句 TTS 并发上限 3；分块发射器保证音频块按句序下发（失败句跳过、零成功句回退整段合成）；AI 说话期间与结束后 800ms 回声冷却窗口丢弃麦克风输入；迟到 ASR 结果在处理中/AI 说话时丢弃。
8. **音频约定**：上行裸 PCM 16kHz/16bit/单声道/200ms 帧、base64 装在 JSON 文本帧；下行 24kHz WAV 整段或逐句分块，均 base64。
9. **阶段机**：INTRO→TECH→PROJECT→HR→COMPLETED；各阶段时长（min/suggested/max）与题量（min/max）判据照 Java 配置与推进逻辑移植；阶段开关（intro/tech/project/hr enabled）来自创建请求。
10. **开场白**：voice-interview-opening.yml 资产拷贝嵌入（9 个方向 + 默认）；会话建立后先落库再推文本、随后 TTS 音频；开场音频内存缓存（预热默认关）。
11. **上下文压缩**：有界历史加载（SUMMARY 行 + 最近 20 轮 + 至多 1 个待摘要批 10 条）→ WINDOW/SUMMARY 两模式，字符预算（历史 12000/摘要 4000），SUMMARY 用无记忆的普通客户端做增量摘要，新边界在悲观锁（SELECT FOR UPDATE）内写回 SUMMARY 行；摘要失败保留旧摘要或降级 WINDOW，永不回退全量历史。摘要 prompt 模板逐字移植。
12. **消息落库**：每轮一条 DIALOGUE 行（user_recognized_text + ai_generated_text + phase + sequence_num），提交后回填最近未答问题的答案字段；SUMMARY 行由压缩器独占写。
13. **缓存**：Redis 键 voice:interview:session:{id}（TTL 1h）缓存会话实体，读优先写失效沿 Java 行为。
14. **暂停与超时**：REST pause 仅置状态（IN_PROGRESS→PAUSED）与失效缓存，不断连；不活动巡检（30s）：4:30 推 pause_timeout_warning，5:00 推 pause_timeout、暂停落库、断连、停 ASR。resume 仅 PAUSED→IN_PROGRESS。
15. **结束与评估**：end 置 COMPLETED/endTime/actualDuration（秒）并置评估 PENDING，事务提交后入 Redis Stream voice:evaluate:stream（组 voice-evaluate-group，MAXLEN 1000）；消费侧幂等跳过非 PENDING、失败重投至多 3 次后置 FAILED + evaluate_error（截断 500）；评估状态查询/重试接口：COMPLETED 返回缓存、PROCESSING 原样返回、否则置 PENDING 入队。评估恢复走 DB 状态巡检（PENDING 超 3min 补投、PROCESSING 超 30min 置败），不引入文字面试的 ZSET 机制（对齐 Java）。
16. **评估生成**：DIALOGUE 消息 → 问答配对（未答问题状态机 + 分类关键词推断）→ 复用文字面试评估引擎（批次、结构化输出、降级共用其配置段与接缝），注入按方向定位的评估参考资料（复用既有截断规则）；报告落 voice_interview_evaluations（JSON 列），明细 DTO 字段形状与文字面试明细逐字对齐（sessionId/totalQuestions/overallScore/overallFeedback/strengths/improvements/answers[]，AnswerDetail 七字段）；无对话场次生成占位空评估。
17. **僵尸清理**：60s 巡检：IN_PROGRESS 超 2h 强制结束并触发评估；与第 15 条评估恢复合并为同一调度器。
18. **配置**：新增 app.voice_interview 段（snake_case，项目惯例）：上下文压缩五参、阶段判据、语音化清洗上限（120 字）、流式推送节流（180ms/12 字符）、分块音频开关、TTS 双超时与并发上限、回声冷却、开场预热开关；默认值对齐 Java application.yml 与代码默认。Java 死配置不移植（rate-limit.*、audio.*、遗留 debounce 键、tts.default-voice）。ASR/TTS 运行时配置与 Key 一律来自 voice_config 表（llmprovider 模块已复刻，含热更新接口）。
19. **测试接缝（唯一新增）**：模块内定义两个消费方接口——语音模型解析（按 provider 解析出可流式调用的 ChatModel，由 aibridge 桥满足，测试注入 stub 流）与语音配置源（读 ASR/TTS 运行时配置与解密 Key，由 llmprovider 服务满足，测试给静态配置）。ASR/TTS 客户端**不设**接口接缝：管道测试通过配置 URL 指向进程内假 DashScope 服务端，走真实协议代码（最高接缝）。评估沿 EvaluationRunner/EvalStore 既有接缝。用户身份沿用仓库现状：固定默认用户，无鉴权。
20. **优雅关闭**：进程关闭时关闭全部活动 WS 连接，由断开处理路径自动结束会话并触发评估（复用第 26 条用户故事的行为），不另做收尾编排。

## Testing Decisions

- **好测试的标准**：只测外部行为——给定输入与脚本化的模型/协议响应，断言下发消息序列、落库状态与接口输出；不 mock 内部步骤、不断言实现细节。
- **封闭式管道测试（对"不做 HTTP 集成测试"的有意例外，仅限本模块）**：进程内 httptest 起真 WS 服务端（真网关代码），配进程内假 DashScope ASR/TTS 服务端（真协议客户端打假端点）+ stub LLM 流 + 内存 store；全程无真 DB、无外网、确定性可重复。覆盖：字幕两级下发、话语拼接、提交去重（CAS）、回声冷却丢帧、分块音频句序、整段兜底、暂停超时、断开自动结束。文字面试"不做 DB/HTTP 集成测试"的决策不变——本例外不含任何真 DB 依赖。
- **纯函数表驱动单测**（先例：interview 模块同包 `_test.go`、中文用例名）：问答配对与分类推断、上下文压缩边界与预算、句边界切分与语音化清洗、WAV 头字节、阶段推进判据、话语多段拼接、错误话术映射。
- **契约测试**（先例：interview/contract_test.go）：全部 REST DTO 与六类 WS 消息的 JSON 形状 golden 对齐 Java 字段名。
- **协议客户端契约测试**：真客户端 × 进程内假 DashScope 服务端，脚本化事件序列覆盖建连、ready 超时、重连、部分/最终事件、TTS 超时跳句。
- **真实冒烟**：cmd/smoke 增加 voice 模式（-mode，默认 text 保持不变）：TTS 合成回答音频 → 线性重采样 16k → 按 200ms 帧喂 ASR → 断言字幕/流式文本/音频块 → 结束 → 轮询评估 → 报告校验。手动执行，不进 CI。

## Out of Scope

- 鉴权（仓库现状即全接口匿名，userId 固定默认值）
- 前端任何改动（Java 仓库前端原样对接）
- Micrometer 指标对应的可观测性设施（以 slog 结构化日志替代）
- opus 等音频编解码转换（Java audio.* 配置本就未消费）
- 语音三张表的建表与迁移（Java Flyway / DBA 负责）
- 文字面试模块的任何行为变更
- 真 DashScope 计费调用进 CI；自动打断（barge-in）、多说话人、声纹
- RAG / 知识库与语音面试的结合

## Further Notes

- 参考实现：仓库外 `/home/wuzuxian/interview-guide`（Java 25 + Spring Boot 4.1）。实现时需逐字摘取：REST DTO 字段名与校验默认值、WS 六类消息的 JSON 形状、QwenAsrService/QwenTtsService 事件语义与常量（重试次数、间隔、ready 检查）、VoiceInterviewWebSocketHandler 管道细节（冷却、CAS、节流参数）、voice-interview-opening.yml、上下文摘要模板、application.yml 的 app.voice-interview 默认值、阶段推进判据、错误话术映射表。
- 术语以 `GLOSSARY.md` 语音面试节为准（语音面试场次、阶段、话语、字幕、开场白、分块音频、上下文压缩、摘要边界、回声冷却、问答配对）；必须遵守 ADR 0001（表结构沿用）、ADR 0002（ChatModel 直调 + Skill 嵌入 prompt 偏离）、ADR 0003（手写 DashScope 协议客户端）。
- 契约风险点：SessionResponseDTO 的 webSocketUrl 为相对路径（客户端自选 ws/wss）；评估明细 DTO 的 questionIndex 等字段名必须逐字（参考文字面试 currentQuestionIndex 字段名事故）；timestamp 列在 Java DDL 中是保留字。
- 分票建议（供 /to-tickets 参考，共 10 张）：01 会话域与 REST 契约；02 ASR 协议客户端 + 假服务端；03 TTS 协议客户端 + 假服务端；04 WS 网关；05 开场白与阶段机；06 LLM 流式管道；07 上下文压缩；08 评估链路；09 外围行为（暂停超时/僵尸清理/断开自动结束）；10 语音冒烟。依赖边：02、03 无依赖可并行先行；04←{01,02}；05←{03,04}；06←{03,04}；07←{01,06}；08←01；09←04；10←全部。

## 与 Java 行为的有意偏离

除"逐字对齐 Java"外，以下偏离是经过裁决的有意行为，回退任何一条前请先读对应理由（实现完成后在此补全完整清单）：

1. **Skill 加载改静态嵌入**：Java 语音面试官通过 SkillsTool 工具调用中途加载 SKILL.md；Go 把方向内容静态嵌入 system prompt（ADR 0002 增补），消除工具调用失败导致面试官无题可问的故障类。
2. **Micrometer 指标不移植**：Go 无指标设施，以 slog 结构化日志记录各阶段耗时替代，不影响前端契约。
3. **死配置不移植**：app.voice-interview.rate-limit.*、audio.*、遗留 debounce 键、tts.default-voice 在 Java 模块内零消费（已核实），Go 不搬运。
4. **配置键名 snake_case**（app.voice_interview）：项目既有惯例，非 Java 的 kebab-case。
