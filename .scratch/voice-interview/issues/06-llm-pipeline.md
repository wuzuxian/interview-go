# 06: LLM 流式管道（语音回合闭环）

**What to build:** 求职者提交话语后完成一整轮面试官回复：模型按场次提供方流式生成（eino ChatModel.Stream 直调，ADR 0002），token 累积检测句边界逐句成段，按节流（≥180ms 且增量 ≥12 字符）推送 text(content,final=false) 并以 final 收尾；每句立即并发 TTS（上限 3）并按句序下发 audio_chunk(index,isLast)，全部完成推 audio_complete；单句 TTS 失败跳过，零句成功回退整段合成单条 audio；分块开关关闭时走整段路径。回复经语音化清洗（去 Markdown/列表、压空白、超 120 字截断到句子边界）。prompt：system = 面试官人设 + 方向内容静态嵌入（复用既有 Skill 资源，ADR 0002 增补偏离）+ 语音约束（每轮 1 主问 + ≤1 追问、2-4 句、禁 Markdown）+ 简历文本（分隔符包裹）+ 防注入；user = 【之前的对话】+【当前对话】，历史逐条清洗（本票先用原始最近历史，07 替换为压缩加载）。完成顺序：subtitle(final) → text(final) → 消息落库（用户话语 + AI 回复 + 阶段 + 序号）→ 音频。LLM 异常（认证/超时/限流/网络）映射友好话术、下发 error、会话不中断。

**Blocked by:** 03（TTS 客户端）, 04（网关回合挂钩）

**Status:** ready-for-agent

- [ ] 语音模型解析接缝（本特性唯一新增接缝之一）：按场次 llmProvider（空 → 默认）解析流式模型；aibridge 生产实现、测试注入 stub 流
- [ ] 流式出句：终止标点（。！？；!?;.）切分；节流推送部分帧、final 帧携带完整回复；不产生句子的短回复走整段路径
- [ ] 分块音频：按句序下发，index 递增、末句 isLast=true，随后 audio_complete；单句失败跳过不阻塞后续句；0 句成功回退整段合成单 audio；分块开关关闭时整段路径
- [ ] 逐句 TTS 并发上限 3（信号量语义）
- [ ] 语音化清洗对齐 Java optimizeForVoice（去 Markdown/列表标记、压空白、>120 字截断到句界），清洗发生在出句前
- [ ] prompt 组装对齐 Java 逐字（人设、语音约束、简历包裹、防注入、【之前的对话】/【当前对话】结构、逐条 sanitize）；方向内容静态嵌入、不使用工具调用
- [ ] 一轮完成顺序契约：subtitle(final) → text(final) → DIALOGUE 消息落库 → 音频下发；答案回填最近未答问题语义（fillLatestUnansweredQuestion 对应行为）
- [ ] 阶段字段随消息落库（消费 05 的当前阶段）；05 未落地时按会话当前阶段字段工作
- [ ] LLM 异常四类话术对齐 Java；error 下发后会话保持进行中、处理锁释放
- [ ] 管道测试（stub 流 × 假 TTS 服务端 × 内存存储）：正常轮、清洗截断轮、单句失败跳句、全句失败整段回退、错误话术、双答拒绝、AI 说话期间回声冷却生效
