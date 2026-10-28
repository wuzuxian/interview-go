// Package llmprovider 对应 Java 版设置模块（LLM Provider 管理）：
// 表结构与应用层行为与 interview.guide.modules.llmprovider 保持一致。
package llmprovider

import "time"

// ProviderEntity 对应表 llm_provider_config。
type ProviderEntity struct {
	ID                  string    `gorm:"column:id;primaryKey;size:64"`
	BaseURL             string    `gorm:"column:base_url;size:512;not null"`
	APIKeyCiphertext    string    `gorm:"column:api_key_ciphertext;size:4096;not null"`
	APIKeyNonce         string    `gorm:"column:api_key_nonce;size:64;not null"`
	Model               string    `gorm:"column:model;size:128;not null"`
	EmbeddingModel      *string   `gorm:"column:embedding_model;size:128"`
	EmbeddingDimensions *int      `gorm:"column:embedding_dimensions"`
	SupportsEmbedding   bool      `gorm:"column:supports_embedding;not null"`
	Temperature         *float64  `gorm:"column:temperature"`
	Enabled             bool      `gorm:"column:enabled;not null"`
	Builtin             bool      `gorm:"column:builtin;not null"`
	CreatedAt           time.Time `gorm:"column:created_at;not null"`
	UpdatedAt           time.Time `gorm:"column:updated_at;not null"`
}

func (ProviderEntity) TableName() string { return "llm_provider_config" }

// GlobalSettingEntity 对应表 llm_global_setting，单例行 id=1。
type GlobalSettingEntity struct {
	ID                         int64     `gorm:"column:id;primaryKey"`
	DefaultChatProviderID      string    `gorm:"column:default_chat_provider_id;size:64;not null"`
	DefaultEmbeddingProviderID string    `gorm:"column:default_embedding_provider_id;size:64;not null"`
	CreatedAt                  time.Time `gorm:"column:created_at;not null"`
	UpdatedAt                  time.Time `gorm:"column:updated_at;not null"`
}

func (GlobalSettingEntity) TableName() string { return "llm_global_setting" }

// SingletonID 全局设置单例行 ID，与 Java 版一致。
const SingletonID = 1

// VoiceConfigEntity 语音（ASR/TTS）配置单例，JSON 存储于 voice_config 表。
// Java 版写入 YAML 文件；Go 版落库以获得等价的持久化语义。
// ASR 与 TTS 共享同一个 API Key（与 Java 版 updateEnvValue("AI_BAILIAN_API_KEY", ...) 一致）。
type VoiceConfigEntity struct {
	ID               int64     `gorm:"column:id;primaryKey"`
	AsrJSON          string    `gorm:"column:asr_json;type:text;not null"`
	TtsJSON          string    `gorm:"column:tts_json;type:text;not null"`
	APIKeyNonce      string    `gorm:"column:api_key_nonce;size:64;not null"`
	APIKeyCiphertext string    `gorm:"column:api_key_ciphertext;size:4096;not null"`
	UpdatedAt        time.Time `gorm:"column:updated_at;not null"`
}

func (VoiceConfigEntity) TableName() string { return "voice_config" }

// VoiceConfigID 语音配置单例行 ID。
const VoiceConfigID = 1
