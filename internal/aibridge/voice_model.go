// 语音面试的流式模型解析（spec 实现决策 19 的「语音模型解析」接缝生产实现）：
// 按场次 llmProvider 从 ai.Registry 构建可流式调用的 eino ChatModel（OpenAI 兼容
// 协议，ADR 0002）。返回 ai.StreamChatModel（voiceinterview.StreamChatModel 为其
// 包内别名），*Bridge 在 main 中直接注入 voiceinterview.VoiceModelResolver，
// 两个包互不依赖。
package aibridge

import (
	"context"
	"strings"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/schema"

	"github.com/wuzuxian/interview/internal/ai"
)

// VoiceStreamModelFor 按场次 llmProvider 解析流式模型：空/空白/"default"（忽略
// 大小写）→ 默认聊天 Provider（对齐 Java LlmProviderRegistry.resolveProviderId）。
// Provider 不存在/禁用时透传注册表业务错误。
func (b *Bridge) VoiceStreamModelFor(ctx context.Context, providerID string) (ai.StreamChatModel, error) {
	id := resolveVoiceProviderID(providerID, b.Reg.DefaultChatID())
	m, err := b.Reg.ChatModelFor(ctx, b.Crypto, id, b.Timeout)
	if err != nil {
		return nil, err
	}
	return einoStreamModel{m}, nil
}

// resolveVoiceProviderID Java resolveProviderId 语义（与 interview.DefaultChatProviderID
// 同规则；独立实现以保持 aibridge 不依赖业务模块）。
func resolveVoiceProviderID(providerID, defaultID string) string {
	trimmed := strings.TrimSpace(providerID)
	if trimmed == "" || strings.EqualFold(trimmed, "default") {
		return defaultID
	}
	return trimmed
}

// einoStreamModel eino ChatModel 的无参流式适配（语音管道不使用模型级 Option）。
type einoStreamModel struct {
	m *einoopenai.ChatModel
}

func (e einoStreamModel) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	return e.m.Stream(ctx, input)
}
