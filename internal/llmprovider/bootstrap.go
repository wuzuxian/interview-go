package llmprovider

import (
	"encoding/json"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/wuzuxian/interview/internal/apperr"
)

// SeedProvider 应用配置中的预设 Provider，对应 Java 版 app.ai.providers。
type SeedProvider struct {
	BaseURL             string   `config:"base_url"`
	APIKey              string   `config:"api_key"`
	Model               string   `config:"model"`
	EmbeddingModel      string   `config:"embedding_model"`
	EmbeddingDimensions int      `config:"embedding_dimensions"`
	SupportsEmbedding   bool     `config:"supports_embedding"`
	Temperature         *float64 `config:"temperature"`
}

// Bootstrap 启动期引导：空表时按配置播种 Provider、初始化全局默认与语音配置。
// 对应 Java 版 LlmProviderBootstrapService。
type Bootstrap struct {
	db     *gorm.DB
	crypto *CryptoService
	logger *slog.Logger

	DefaultChatProvider       string
	DefaultEmbeddingProvider  string
	DefaultEmbeddingDimension int
	SeedProviders             map[string]SeedProvider
	SeedAsr                   AsrConfig
	SeedTts                   TtsConfig
	SeedVoiceAPIKey           string
}

// NewBootstrap 创建引导器。
func NewBootstrap(db *gorm.DB, crypto *CryptoService, logger *slog.Logger) *Bootstrap {
	return &Bootstrap{db: db, crypto: crypto, logger: logger}
}

// Run 执行播种逻辑（幂等）。
func (b *Bootstrap) Run() error {
	if err := b.seedProvidersIfNecessary(); err != nil {
		return err
	}
	if err := b.ensureGlobalSetting(); err != nil {
		return err
	}
	return b.ensureVoiceConfig()
}

func (b *Bootstrap) seedProvidersIfNecessary() error {
	var count int64
	if err := b.db.Model(&ProviderEntity{}).Count(&count).Error; err != nil {
		return apperr.Wrap(apperr.CodeProviderReadFailed, "读取 Provider 配置失败", err)
	}
	if count > 0 {
		return nil
	}
	if len(b.SeedProviders) == 0 {
		b.logger.Warn("No app.ai.providers seed configuration found")
		return nil
	}
	for id, cfg := range b.SeedProviders {
		if cfg.BaseURL == "" || cfg.Model == "" {
			b.logger.Warn("Skip invalid provider seed", slog.String("id", id))
			continue
		}
		apiKey := cfg.APIKey
		if apiKey == "" {
			apiKey = ""
		}
		encrypted, err := b.crypto.Encrypt(apiKey)
		if err != nil {
			return err
		}
		supportsEmbedding := cfg.SupportsEmbedding || cfg.EmbeddingModel != ""
		dimensions := cfg.EmbeddingDimensions
		if dimensions <= 0 {
			dimensions = b.DefaultEmbeddingDimension
		}
		entity := ProviderEntity{
			ID:                  id,
			BaseURL:             cfg.BaseURL,
			APIKeyNonce:         encrypted.Nonce,
			APIKeyCiphertext:    encrypted.Ciphertext,
			Model:               cfg.Model,
			EmbeddingModel:      strPtr(cfg.EmbeddingModel),
			EmbeddingDimensions: intPtr(dimensions),
			SupportsEmbedding:   supportsEmbedding,
			Temperature:         cfg.Temperature,
			Enabled:             true,
			Builtin:             true,
			CreatedAt:           time.Now(),
			UpdatedAt:           time.Now(),
		}
		if err := b.db.Create(&entity).Error; err != nil {
			return apperr.Wrap(apperr.CodeProviderWriteFailed, "播种 Provider 失败: "+id, err)
		}
	}
	var total int64
	_ = b.db.Model(&ProviderEntity{}).Count(&total)
	b.logger.Info("Seeded LLM providers from application configuration", slog.Int64("count", total))
	return nil
}

func (b *Bootstrap) ensureGlobalSetting() error {
	var count int64
	if err := b.db.Model(&GlobalSettingEntity{}).Where("id = ?", SingletonID).Count(&count).Error; err != nil {
		return apperr.Wrap(apperr.CodeProviderReadFailed, "读取全局默认配置失败", err)
	}
	if count > 0 {
		return nil
	}

	var ids []string
	if err := b.db.Model(&ProviderEntity{}).Order("created_at ASC").Pluck("id", &ids).Error; err != nil {
		return apperr.Wrap(apperr.CodeProviderReadFailed, "读取 Provider 列表失败", err)
	}
	fallback := "dashscope"
	if len(ids) > 0 {
		fallback = ids[0]
	}

	defaultChat := fallback
	for _, id := range ids {
		if id == b.DefaultChatProvider {
			defaultChat = id
			break
		}
	}

	// embedding 默认：配置的优先，其次第一个支持 embedding 且启用的 Provider，最后回退 chat 默认。
	defaultEmbedding := defaultChat
	preferred := b.DefaultEmbeddingProvider
	if preferred == "" {
		preferred = defaultChat
	}
	var candidates []ProviderEntity
	if err := b.db.Order("created_at ASC").Find(&candidates).Error; err != nil {
		return apperr.Wrap(apperr.CodeProviderReadFailed, "读取 Provider 列表失败", err)
	}
	pick := func(id string) (string, bool) {
		for _, p := range candidates {
			if p.ID == id && p.Enabled && p.SupportsEmbedding && deref(p.EmbeddingModel) != "" {
				return p.ID, true
			}
		}
		return "", false
	}
	if id, ok := pick(preferred); ok {
		defaultEmbedding = id
	} else {
		for _, p := range candidates {
			if p.Enabled && p.SupportsEmbedding && deref(p.EmbeddingModel) != "" {
				defaultEmbedding = p.ID
				break
			}
		}
	}

	setting := GlobalSettingEntity{
		ID:                         SingletonID,
		DefaultChatProviderID:      defaultChat,
		DefaultEmbeddingProviderID: defaultEmbedding,
		CreatedAt:                  time.Now(),
		UpdatedAt:                  time.Now(),
	}
	if err := b.db.Create(&setting).Error; err != nil {
		return apperr.Wrap(apperr.CodeProviderWriteFailed, "初始化全局默认 Provider 失败", err)
	}
	b.logger.Info("Initialized LLM global setting",
		slog.String("chatProvider", defaultChat), slog.String("embeddingProvider", defaultEmbedding))
	return nil
}

func (b *Bootstrap) ensureVoiceConfig() error {
	var count int64
	if err := b.db.Model(&VoiceConfigEntity{}).Where("id = ?", VoiceConfigID).Count(&count).Error; err != nil {
		return apperr.Wrap(apperr.CodeVoiceReadFailed, "读取语音配置失败", err)
	}
	if count > 0 {
		return nil
	}
	asr := b.SeedAsr
	if asr.URL == "" {
		asr = AsrConfig{
			URL:                            "wss://dashscope.aliyuncs.com/api-ws/v1/realtime",
			Model:                          "qwen3-asr-flash-realtime",
			Language:                       "zh",
			Format:                         "pcm",
			SampleRate:                     16000,
			EnableTurnDetection:            true,
			TurnDetectionType:              "server_vad",
			TurnDetectionThreshold:         0,
			TurnDetectionSilenceDurationMs: 2000,
		}
	}
	tts := b.SeedTts
	if tts.Model == "" {
		tts = TtsConfig{
			Model:        "qwen3-tts-flash-realtime",
			Voice:        "Cherry",
			Format:       "pcm",
			SampleRate:   24000,
			Mode:         "commit",
			LanguageType: "Chinese",
			SpeechRate:   1.0,
			Volume:       60,
		}
	}
	encrypted, err := b.crypto.Encrypt(b.SeedVoiceAPIKey)
	if err != nil {
		return err
	}
	asrJSON, _ := json.Marshal(asr)
	ttsJSON, _ := json.Marshal(tts)
	entity := VoiceConfigEntity{
		ID:               VoiceConfigID,
		AsrJSON:          string(asrJSON),
		TtsJSON:          string(ttsJSON),
		APIKeyNonce:      encrypted.Nonce,
		APIKeyCiphertext: encrypted.Ciphertext,
		UpdatedAt:        time.Now(),
	}
	if err := b.db.Create(&entity).Error; err != nil {
		return apperr.Wrap(apperr.CodeVoiceWriteFailed, "初始化语音配置失败", err)
	}
	b.logger.Info("Initialized voice config")
	return nil
}
