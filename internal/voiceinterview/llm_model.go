// 流式模型接缝（spec 实现决策 19 的两个新增接缝之一，本包唯一 LLM 接缝）：
// VoiceModelResolver 按场次 llmProvider 解析可流式调用的模型（空/空白/"default" →
// 默认 Provider，对齐 Java LlmProviderRegistry.resolveProviderId）。StreamChatModel
// 为 internal/ai.StreamChatModel 的包内别名（eino schema.Message/StreamReader，
// ADR 0002：ChatModel 直调、不用 Agent/Tools）——先例同 interview.ModelResolver 的
// ai.ChatCompleter；生产实现由 internal/aibridge 提供（einoext openai 组件按 DB
// Provider 构建），测试注入 schema.Pipe stub 流。
package voiceinterview

import (
	"context"

	"github.com/wuzuxian/interview/internal/ai"
)

// StreamChatModel 可流式调用的对话模型（ai.StreamChatModel 别名，包内统一引用名）。
type StreamChatModel = ai.StreamChatModel

// VoiceModelResolver 按场次 llmProvider 解析流式模型。
type VoiceModelResolver interface {
	// VoiceStreamModelFor 解析失败（Provider 不存在/禁用等）透传设置模块业务错误。
	VoiceStreamModelFor(ctx context.Context, providerID string) (StreamChatModel, error)
}

// PlainChatCompleter 非流式普通对话模型（ai.ChatCompleter 别名；上下文摘要专用）。
type PlainChatCompleter = ai.ChatCompleter

// VoicePlainModelResolver 按场次 llmProvider 解析非流式普通模型（上下文摘要专用：
// 无工具、无对话记忆注入，对齐 Java LlmProviderRegistry.getPlainChatClient(llmProvider)——
// 摘要是纯文本压缩，不混入面试素材工具，也不重新注入完整历史抵消压缩收益）。
// 生产实现同为 *aibridge.Bridge（Generate 直调，与流式共用注册表构建）。
type VoicePlainModelResolver interface {
	// VoicePlainCompleterFor 解析失败（Provider 不存在/禁用等）透传设置模块业务错误。
	VoicePlainCompleterFor(ctx context.Context, providerID string) (PlainChatCompleter, error)
}
