package interview

import (
	"context"

	"github.com/wuzuxian/interview/internal/ai"
)

// ModelResolver interview 模块唯一的 LLM 接缝（spec 决策 3）：
// 按 Provider ID 解析 JSON 响应模式的 ChatCompleter，并暴露默认 Provider ID。
// 由 main 用 *aibridge.Bridge 直接注入（方法名与 aibridge 语义一致，无需适配器）；
// 本模块后续所有 LLM 调用（出题、评估、汇总）都经由它，不新增其他接缝。
type ModelResolver interface {
	// JSONChatModelFor 按 Provider ID 构建 JSON 响应模式的 ChatModel。
	// Provider 不存在/禁用时透传 aibridge 与设置模块的既有业务错误。
	JSONChatModelFor(ctx context.Context, providerID string) (ai.ChatCompleter, error)

	// DefaultChatID 返回设置页配置的默认聊天 Provider ID。
	DefaultChatID() string
}
