# 文字面试 LLM 编排用 eino ChatModel 直调，不引入 Agent/Graph

文字面试的出题与评估 prompt 均不依赖工具调用（Java 版出题显式使用无工具客户端；评估虽挂载 SkillsTool 但 prompt 从不要求调工具），且多步控制流（出题双路并行、评估分批 → 二分恢复 → 二次汇总）本质是 Go 代码里的普通逻辑。决定：直接用 eino ChatModel（OpenAI 兼容协议，复用 `internal/ai/registry.go` 的动态构建），配自研结构化输出器（prompt 拼 JSON 格式指令 → 解析 → 失败带修复提示重试 2 次 + JSON 修复），出题双路用 goroutine 并行。

不采用 eino ReAct Agent / ADK / Graph 编排：它们引入的工具调用不确定性会干扰结构化输出，图编排反而掩盖控制流、增加排查成本。目标是与 Java 版行为逐字对齐，最直接的路径就是最接近 Spring AI ChatClient.call() 的用法。

## Consequences

- eino 在本模块的用法收敛为：ChatModel 抽象、`schema.Message`、（可选）`prompt.FromMessages` 模板；不使用 compose 图与 Agent 运行时。
- 结构化输出的解析重试与 JSON 修复逻辑需自行维护（对齐 Java 的 StructuredOutputInvoker）。

## 语音面试增补（2026-10-28）

语音面试沿用同一决策：LLM 腿用 eino ChatModel 直调的**流式接口**（`Stream`，OpenAI 兼容协议走 provider 注册表），流式出句、逐句 TTS 的编排仍是 Go 普通代码，不用 Agent/Graph/Tools。ASR/TTS 不走 eino（eino 无语音组件），见 ADR 0003。一处记录在案的有意偏离：Java 版语音面试官通过 SkillsTool 工具调用中途加载 SKILL.md，Go 版把方向内容静态嵌入 system prompt（与文字面试同法），消除"工具调用失败导致面试官无题可问"这一整类故障。
