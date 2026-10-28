package llmprovider

// ProviderDTO GET /api/llm-provider/list 与 /{id} 的响应项，
// 字段名与 Java 版 ProviderDTO（驼峰序列化）一致。
type ProviderDTO struct {
	ID                       string   `json:"id"`
	BaseURL                  string   `json:"baseUrl"`
	MaskedAPIKey             string   `json:"maskedApiKey"`
	Model                    string   `json:"model"`
	EmbeddingModel           *string  `json:"embeddingModel"`
	EmbeddingDimensions      *int     `json:"embeddingDimensions"`
	SupportsEmbedding        bool     `json:"supportsEmbedding"`
	Temperature              *float64 `json:"temperature"`
	DefaultChatProvider      bool     `json:"defaultChatProvider"`
	DefaultEmbeddingProvider bool     `json:"defaultEmbeddingProvider"`
}

// CreateProviderRequest POST /api/llm-provider 请求体。
type CreateProviderRequest struct {
	ID                  string   `json:"id"`
	BaseURL             string   `json:"baseUrl"`
	APIKey              string   `json:"apiKey"`
	Model               string   `json:"model"`
	EmbeddingModel      *string  `json:"embeddingModel"`
	EmbeddingDimensions *int     `json:"embeddingDimensions"`
	SupportsEmbedding   *bool    `json:"supportsEmbedding"`
	Temperature         *float64 `json:"temperature"`
}

// UpdateProviderRequest PUT /api/llm-provider/{id} 请求体，全部可选、部分更新。
type UpdateProviderRequest struct {
	BaseURL             *string  `json:"baseUrl"`
	APIKey              *string  `json:"apiKey"`
	Model               *string  `json:"model"`
	EmbeddingModel      *string  `json:"embeddingModel"`
	EmbeddingDimensions *int     `json:"embeddingDimensions"`
	SupportsEmbedding   *bool    `json:"supportsEmbedding"`
	Temperature         *float64 `json:"temperature"`
}

// DefaultProviderDTO 默认 Provider 读写体，对应 GET/PUT default-provider。
type DefaultProviderDTO struct {
	DefaultProvider          *string `json:"defaultProvider"`
	DefaultEmbeddingProvider *string `json:"defaultEmbeddingProvider"`
}

// ProviderTestResult 连通性测试结果。
type ProviderTestResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Model   string `json:"model"`
}

// AsrConfigDTO 语音识别配置响应。
type AsrConfigDTO struct {
	URL                            string  `json:"url"`
	Model                          string  `json:"model"`
	MaskedAPIKey                   string  `json:"maskedApiKey"`
	Language                       string  `json:"language"`
	Format                         string  `json:"format"`
	SampleRate                     int     `json:"sampleRate"`
	EnableTurnDetection            bool    `json:"enableTurnDetection"`
	TurnDetectionType              string  `json:"turnDetectionType"`
	TurnDetectionThreshold         float64 `json:"turnDetectionThreshold"`
	TurnDetectionSilenceDurationMs int     `json:"turnDetectionSilenceDurationMs"`
}

// TtsConfigDTO 语音合成配置响应。
type TtsConfigDTO struct {
	Model        string  `json:"model"`
	MaskedAPIKey string  `json:"maskedApiKey"`
	Voice        string  `json:"voice"`
	Format       string  `json:"format"`
	SampleRate   int     `json:"sampleRate"`
	Mode         string  `json:"mode"`
	LanguageType string  `json:"languageType"`
	SpeechRate   float64 `json:"speechRate"`
	Volume       int     `json:"volume"`
}

// AsrConfig 语音识别配置持久化结构（不含 API Key，Key 单独存储）。
type AsrConfig struct {
	URL                            string  `json:"url"`
	Model                          string  `json:"model"`
	Language                       string  `json:"language"`
	Format                         string  `json:"format"`
	SampleRate                     int     `json:"sampleRate"`
	EnableTurnDetection            bool    `json:"enableTurnDetection"`
	TurnDetectionType              string  `json:"turnDetectionType"`
	TurnDetectionThreshold         float64 `json:"turnDetectionThreshold"`
	TurnDetectionSilenceDurationMs int     `json:"turnDetectionSilenceDurationMs"`
}

// TtsConfig 语音合成配置持久化结构。
type TtsConfig struct {
	Model        string  `json:"model"`
	Voice        string  `json:"voice"`
	Format       string  `json:"format"`
	SampleRate   int     `json:"sampleRate"`
	Mode         string  `json:"mode"`
	LanguageType string  `json:"languageType"`
	SpeechRate   float64 `json:"speechRate"`
	Volume       int     `json:"volume"`
}

// AsrConfigRequest PUT /api/llm-provider/voice/asr 请求体（部分更新）。
type AsrConfigRequest struct {
	URL                            *string  `json:"url"`
	Model                          *string  `json:"model"`
	APIKey                         *string  `json:"apiKey"`
	Language                       *string  `json:"language"`
	Format                         *string  `json:"format"`
	SampleRate                     *int     `json:"sampleRate"`
	EnableTurnDetection            *bool    `json:"enableTurnDetection"`
	TurnDetectionType              *string  `json:"turnDetectionType"`
	TurnDetectionThreshold         *float64 `json:"turnDetectionThreshold"`
	TurnDetectionSilenceDurationMs *int     `json:"turnDetectionSilenceDurationMs"`
}

// TtsConfigRequest PUT /api/llm-provider/voice/tts 请求体（部分更新）。
type TtsConfigRequest struct {
	Model        *string  `json:"model"`
	APIKey       *string  `json:"apiKey"`
	Voice        *string  `json:"voice"`
	Format       *string  `json:"format"`
	SampleRate   *int     `json:"sampleRate"`
	Mode         *string  `json:"mode"`
	LanguageType *string  `json:"languageType"`
	SpeechRate   *float64 `json:"speechRate"`
	Volume       *int     `json:"volume"`
}
