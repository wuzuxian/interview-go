// Package ai 的流式对话模型接口：eino ChatModel.Stream 的无参视图（ADR 0002：
// ChatModel 直调，语音面试流式出句为本仓库首个 Stream 使用点）。接口置于本包使
// 业务模块（voiceinterview 定义 VoiceModelResolver 接缝）与适配桥（aibridge 构建
// 生产实现）共享同一类型，双方互不依赖——先例同 ChatCompleter。
package ai

import (
	"context"

	"github.com/cloudwego/eino/schema"
)

// StreamChatModel 可流式调用的对话模型；流末以 io.EOF 收尾，调用方负责 Close。
type StreamChatModel interface {
	Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader[*schema.Message], error)
}
