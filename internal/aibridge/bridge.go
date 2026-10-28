// Package aibridge 将 ai.Registry 适配为简历模块所需的 ModelFactory 与向量能力，
// 避免业务模块与 AI 基础设施之间的循环依赖。
package aibridge

import (
	"context"
	"log/slog"
	"time"

	einoembedding "github.com/cloudwego/eino-ext/components/embedding/openai"

	"github.com/wuzuxian/interview/internal/ai"
	"github.com/wuzuxian/interview/internal/llmprovider"
)

// Bridge 持有注册表与加密服务，暴露业务侧所需的窄接口。
type Bridge struct {
	Reg     *ai.Registry
	Crypto  *llmprovider.CryptoService
	Timeout time.Duration
	Logger  *slog.Logger
}

// New 创建适配桥。
func New(reg *ai.Registry, crypto *llmprovider.CryptoService, timeout time.Duration, logger *slog.Logger) *Bridge {
	return &Bridge{Reg: reg, Crypto: crypto, Timeout: timeout, Logger: logger}
}

// JSONChatModelFor 实现 resume.ModelFactory：构建 JSON 响应模式的 ChatModel。
func (b *Bridge) JSONChatModelFor(ctx context.Context, providerID string) (ai.ChatCompleter, error) {
	return b.Reg.JSONChatModelFor(ctx, b.Crypto, providerID, b.Timeout)
}

// DefaultChatID 返回默认聊天 Provider ID。
func (b *Bridge) DefaultChatID() string { return b.Reg.DefaultChatID() }

// DefaultEmbedder 构建默认 embedding Provider 的 eino Embedder。
func (b *Bridge) DefaultEmbedder(ctx context.Context) (*einoembedding.Embedder, error) {
	return b.Reg.EmbedderFor(ctx, b.Crypto, b.Reg.DefaultEmbedderID(), b.Timeout)
}
