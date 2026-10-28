// Package ai 基于 eino 框架封装 LLM 运行时：
// ProviderRegistry 从数据库加载 Provider 配置并按需构建 eino ChatModel / Embedder，
// StructuredOutputInvoker 提供带修复重试的结构化输出调用。
package ai

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cloudwego/eino-ext/components/embedding/openai"
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"

	"gorm.io/gorm"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/llmprovider"
)

// providerSnapshot 内存中的 Provider 运行时配置快照。
type providerSnapshot struct {
	ID                  string
	BaseURL             string
	APIKey              string
	Model               string
	EmbeddingModel      *string
	EmbeddingDimensions *int
	SupportsEmbedding   bool
	Temperature         *float64
	Enabled             bool
}

// Registry 对应 Java 版 LlmProviderRegistry：缓存数据库 Provider 配置，
// 提供 DefaultChatModel / Embedder 构建与 Reload。
type Registry struct {
	db     *gorm.DB
	logger *slog.Logger

	mu        sync.RWMutex
	providers map[string]*providerSnapshot
	defChat   string
	defEmbed  string
}

// NewRegistry 创建注册表并执行首次加载。
func NewRegistry(db *gorm.DB, logger *slog.Logger) (*Registry, error) {
	r := &Registry{db: db, logger: logger}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload 重新从数据库加载全部 Provider 与全局默认配置。
func (r *Registry) Reload() error {
	var entities []llmprovider.ProviderEntity
	if err := r.db.Find(&entities).Error; err != nil {
		return fmt.Errorf("ai: 加载 Provider 配置失败: %w", err)
	}
	var setting llmprovider.GlobalSettingEntity
	if err := r.db.First(&setting, llmprovider.SingletonID).Error; err != nil {
		return fmt.Errorf("ai: 加载全局默认 Provider 失败: %w", err)
	}

	providers := make(map[string]*providerSnapshot, len(entities))
	for _, e := range entities {
		if !e.Enabled {
			continue
		}
		providers[e.ID] = &providerSnapshot{
			ID:                  e.ID,
			BaseURL:             e.BaseURL,
			APIKey:              "",
			Model:               e.Model,
			EmbeddingModel:      e.EmbeddingModel,
			EmbeddingDimensions: e.EmbeddingDimensions,
			SupportsEmbedding:   e.SupportsEmbedding,
			Temperature:         e.Temperature,
			Enabled:             e.Enabled,
		}
	}

	r.mu.Lock()
	r.providers = providers
	r.defChat = setting.DefaultChatProviderID
	r.defEmbed = setting.DefaultEmbeddingProviderID
	r.mu.Unlock()
	r.logger.Info("LLM Provider 注册表已加载", slog.Int("providers", len(providers)),
		slog.String("chat", setting.DefaultChatProviderID), slog.String("embedding", setting.DefaultEmbeddingProviderID))
	return nil
}

// getSnapshot 读取指定 Provider 快照，并附带解密后的 API Key。
func (r *Registry) getSnapshot(crypto *llmprovider.CryptoService, id string) (*providerSnapshot, error) {
	var entity llmprovider.ProviderEntity
	if err := r.db.First(&entity, "id = ?", id).Error; err != nil {
		return nil, apperr.New(apperr.CodeProviderNotFound, "Provider '"+id+"' 不存在")
	}
	apiKey, err := crypto.Decrypt(entity.APIKeyNonce, entity.APIKeyCiphertext)
	if err != nil {
		return nil, err
	}
	return &providerSnapshot{
		ID:                  entity.ID,
		BaseURL:             entity.BaseURL,
		APIKey:              apiKey,
		Model:               entity.Model,
		EmbeddingModel:      entity.EmbeddingModel,
		EmbeddingDimensions: entity.EmbeddingDimensions,
		SupportsEmbedding:   entity.SupportsEmbedding,
		Temperature:         entity.Temperature,
		Enabled:             entity.Enabled,
	}, nil
}

func (r *Registry) chatModelFrom(ctx context.Context, snap *providerSnapshot, timeout time.Duration, jsonMode bool) (*einoopenai.ChatModel, error) {
	cfg := &einoopenai.ChatModelConfig{
		BaseURL: snap.BaseURL,
		APIKey:  snap.APIKey,
		Model:   snap.Model,
		Timeout: timeout,
	}
	if snap.Temperature != nil {
		t := float32(*snap.Temperature)
		cfg.Temperature = &t
	}
	if jsonMode {
		cfg.ResponseFormat = &einoopenai.ChatCompletionResponseFormat{
			Type: einoopenai.ChatCompletionResponseFormatTypeJSONObject,
		}
	}
	cm, err := einoopenai.NewChatModel(ctx, cfg)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeAIServiceError, "构建 ChatModel 失败", err)
	}
	return cm, nil
}

// DefaultChatModel 构建默认聊天 Provider 的 eino ChatModel。
func (r *Registry) DefaultChatModel(ctx context.Context, crypto *llmprovider.CryptoService, timeout time.Duration) (*einoopenai.ChatModel, string, error) {
	r.mu.RLock()
	id := r.defChat
	r.mu.RUnlock()
	cm, err := r.ChatModelFor(ctx, crypto, id, timeout)
	if err != nil {
		return nil, id, err
	}
	return cm, id, nil
}

// ChatModelFor 为指定 Provider 构建 eino ChatModel。
func (r *Registry) ChatModelFor(ctx context.Context, crypto *llmprovider.CryptoService, id string, timeout time.Duration) (*einoopenai.ChatModel, error) {
	snap, err := r.getSnapshot(crypto, id)
	if err != nil {
		return nil, err
	}
	return r.chatModelFrom(ctx, snap, timeout, false)
}

// JSONChatModelFor 构建 JSON 响应模式（response_format=json_object）的 ChatModel。
func (r *Registry) JSONChatModelFor(ctx context.Context, crypto *llmprovider.CryptoService, id string, timeout time.Duration) (*einoopenai.ChatModel, error) {
	snap, err := r.getSnapshot(crypto, id)
	if err != nil {
		return nil, err
	}
	return r.chatModelFrom(ctx, snap, timeout, true)
}

// EmbedderFor 为指定 Provider 构建 eino Embedder；不支持 embedding 时报错。
func (r *Registry) EmbedderFor(ctx context.Context, crypto *llmprovider.CryptoService, id string, timeout time.Duration) (*openai.Embedder, error) {
	snap, err := r.getSnapshot(crypto, id)
	if err != nil {
		return nil, err
	}
	if !snap.SupportsEmbedding || snap.EmbeddingModel == nil || *snap.EmbeddingModel == "" {
		return nil, apperr.New(apperr.CodeBadRequest, "Provider '"+id+"' 不支持 Embedding")
	}
	cfg := &openai.EmbeddingConfig{
		BaseURL: snap.BaseURL,
		APIKey:  snap.APIKey,
		Model:   *snap.EmbeddingModel,
		Timeout: timeout,
	}
	if snap.EmbeddingDimensions != nil && *snap.EmbeddingDimensions > 0 {
		d := *snap.EmbeddingDimensions
		cfg.Dimensions = &d
	}
	emb, err := openai.NewEmbedder(ctx, cfg)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeAIServiceError, "构建 Embedder 失败", err)
	}
	return emb, nil
}

// DefaultEmbedderID 返回默认 embedding Provider ID。
func (r *Registry) DefaultEmbedderID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defEmbed
}

// DefaultChatID 返回默认聊天 Provider ID。
func (r *Registry) DefaultChatID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defChat
}
