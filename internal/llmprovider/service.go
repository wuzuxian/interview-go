package llmprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/wuzuxian/interview/internal/apperr"
)

// Reloadable Provider 变更后需要刷新的运行时注册表（由 ai.Registry 实现）。
type Reloadable interface {
	Reload() error
}

// Service 设置模块服务，对应 Java 版 LlmProviderConfigService。
type Service struct {
	db       *gorm.DB
	crypto   *CryptoService
	registry Reloadable
	logger   *slog.Logger
	// DefaultEmbeddingDimensions 未配置向量维度时的兜底值。
	DefaultEmbeddingDimensions int
}

// NewService 创建设置服务。
func NewService(db *gorm.DB, crypto *CryptoService, registry Reloadable, logger *slog.Logger, defaultEmbeddingDimensions int) *Service {
	return &Service{db: db, crypto: crypto, registry: registry, logger: logger, DefaultEmbeddingDimensions: defaultEmbeddingDimensions}
}

// ===== 查询 =====

func (s *Service) getSettingOrThrow() (*GlobalSettingEntity, error) {
	var setting GlobalSettingEntity
	err := s.db.First(&setting, SingletonID).Error
	if err != nil {
		return nil, apperr.New(apperr.CodeProviderReadFailed, "默认 Provider 配置未初始化")
	}
	return &setting, nil
}

func (s *Service) getProviderEntityOrThrow(id string) (*ProviderEntity, error) {
	var entity ProviderEntity
	if err := s.db.First(&entity, "id = ?", id).Error; err != nil {
		return nil, apperr.New(apperr.CodeProviderNotFound, "Provider '"+id+"' 不存在")
	}
	return &entity, nil
}

func (s *Service) toDTO(e *ProviderEntity, setting *GlobalSettingEntity) (*ProviderDTO, error) {
	apiKey, err := s.crypto.Decrypt(e.APIKeyNonce, e.APIKeyCiphertext)
	if err != nil {
		return nil, err
	}
	return &ProviderDTO{
		ID:                       e.ID,
		BaseURL:                  e.BaseURL,
		MaskedAPIKey:             MaskAPIKey(apiKey),
		Model:                    e.Model,
		EmbeddingModel:           e.EmbeddingModel,
		EmbeddingDimensions:      s.resolveDimensions(e.EmbeddingDimensions),
		SupportsEmbedding:        e.SupportsEmbedding,
		Temperature:              e.Temperature,
		DefaultChatProvider:      e.ID == setting.DefaultChatProviderID,
		DefaultEmbeddingProvider: e.ID == setting.DefaultEmbeddingProviderID,
	}, nil
}

// ListProviders GET /list。
func (s *Service) ListProviders() ([]ProviderDTO, error) {
	setting, err := s.getSettingOrThrow()
	if err != nil {
		return nil, err
	}
	var entities []ProviderEntity
	if err := s.db.Find(&entities).Error; err != nil {
		return nil, apperr.Wrap(apperr.CodeProviderReadFailed, "读取 Provider 配置失败", err)
	}
	out := make([]ProviderDTO, 0, len(entities))
	for i := range entities {
		dto, err := s.toDTO(&entities[i], setting)
		if err != nil {
			return nil, err
		}
		out = append(out, *dto)
	}
	return out, nil
}

// GetProvider GET /{id}。
func (s *Service) GetProvider(id string) (*ProviderDTO, error) {
	setting, err := s.getSettingOrThrow()
	if err != nil {
		return nil, err
	}
	entity, err := s.getProviderEntityOrThrow(id)
	if err != nil {
		return nil, err
	}
	return s.toDTO(entity, setting)
}

// GetDefaultProvider GET /default-provider。
func (s *Service) GetDefaultProvider() (*DefaultProviderDTO, error) {
	setting, err := s.getSettingOrThrow()
	if err != nil {
		return nil, err
	}
	chat := setting.DefaultChatProviderID
	emb := setting.DefaultEmbeddingProviderID
	return &DefaultProviderDTO{DefaultProvider: &chat, DefaultEmbeddingProvider: &emb}, nil
}

// ===== 写操作 =====

// CreateProvider POST /。
func (s *Service) CreateProvider(ctx context.Context, req *CreateProviderRequest) error {
	id := strings.TrimSpace(req.ID)
	if id == "" {
		return apperr.BadRequest("id 不能为空")
	}
	baseURL, err := requireNonBlank(req.BaseURL, "baseUrl")
	if err != nil {
		return err
	}
	modelName, err := requireNonBlank(req.Model, "model")
	if err != nil {
		return err
	}
	apiKey, err := requireNonBlank(req.APIKey, "apiKey")
	if err != nil {
		return err
	}

	var existing ProviderEntity
	if err := s.db.First(&existing, "id = ?", id).Error; err == nil {
		return apperr.New(apperr.CodeProviderExists, "Provider '"+req.ID+"' 已存在")
	}

	embeddingModel := strings.TrimSpace(deref(req.EmbeddingModel))
	dimensions := s.resolveDimensions(req.EmbeddingDimensions)
	supportsEmbedding := req.SupportsEmbedding != nil && *req.SupportsEmbedding
	if req.SupportsEmbedding == nil {
		supportsEmbedding = embeddingModel != ""
	}
	if err := validateEmbeddingConfig(supportsEmbedding, embeddingModel, dimensions); err != nil {
		return err
	}

	encrypted, err := s.crypto.Encrypt(apiKey)
	if err != nil {
		return err
	}
	entity := ProviderEntity{
		ID:                  id,
		BaseURL:             baseURL,
		APIKeyNonce:         encrypted.Nonce,
		APIKeyCiphertext:    encrypted.Ciphertext,
		Model:               modelName,
		EmbeddingModel:      strPtr(embeddingModel),
		EmbeddingDimensions: intPtr(derefInt(dimensions)),
		SupportsEmbedding:   supportsEmbedding,
		Temperature:         req.Temperature,
		Enabled:             true,
		Builtin:             false,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	if err := s.db.Create(&entity).Error; err != nil {
		return apperr.Wrap(apperr.CodeProviderWriteFailed, "写入 Provider 配置失败", err)
	}
	return s.reload()
}

// UpdateProvider PUT /{id}。
func (s *Service) UpdateProvider(ctx context.Context, id string, req *UpdateProviderRequest) error {
	entity, err := s.getProviderEntityOrThrow(id)
	if err != nil {
		return err
	}

	if req.BaseURL != nil {
		v := strings.TrimSpace(*req.BaseURL)
		if v == "" {
			return apperr.BadRequest("baseUrl 不能为空字符串")
		}
		entity.BaseURL = v
	}
	if req.Model != nil {
		v := strings.TrimSpace(*req.Model)
		if v == "" {
			return apperr.BadRequest("model 不能为空字符串")
		}
		entity.Model = v
	}
	if req.EmbeddingModel != nil {
		entity.EmbeddingModel = strPtr(strings.TrimSpace(*req.EmbeddingModel))
	}
	if req.EmbeddingDimensions != nil {
		entity.EmbeddingDimensions = intPtr(*req.EmbeddingDimensions)
	}
	if req.SupportsEmbedding != nil {
		entity.SupportsEmbedding = *req.SupportsEmbedding
	}
	if err := validateEmbeddingConfig(entity.SupportsEmbedding, deref(entity.EmbeddingModel), s.resolveDimensions(entity.EmbeddingDimensions)); err != nil {
		return err
	}
	if req.Temperature != nil {
		entity.Temperature = req.Temperature
	}
	if req.APIKey != nil {
		v := strings.TrimSpace(*req.APIKey)
		if v == "" {
			return apperr.BadRequest("apiKey 不能为空字符串")
		}
		encrypted, err := s.crypto.Encrypt(v)
		if err != nil {
			return err
		}
		entity.APIKeyNonce = encrypted.Nonce
		entity.APIKeyCiphertext = encrypted.Ciphertext
	}
	entity.UpdatedAt = time.Now()

	if err := s.db.Save(entity).Error; err != nil {
		return apperr.Wrap(apperr.CodeProviderWriteFailed, "写入 Provider 配置失败", err)
	}
	return s.reload()
}

// DeleteProvider DELETE /{id}，默认 Provider 不可删除。
func (s *Service) DeleteProvider(ctx context.Context, id string) error {
	setting, err := s.getSettingOrThrow()
	if err != nil {
		return err
	}
	if id == setting.DefaultChatProviderID || id == setting.DefaultEmbeddingProviderID {
		return apperr.New(apperr.CodeDefaultNoDelete, "默认 Provider '"+id+"' 不可删除，请先切换默认 Provider")
	}
	if _, err := s.getProviderEntityOrThrow(id); err != nil {
		return err
	}
	if err := s.db.Delete(&ProviderEntity{}, "id = ?", id).Error; err != nil {
		return apperr.Wrap(apperr.CodeProviderWriteFailed, "删除 Provider 配置失败", err)
	}
	return s.reload()
}

// UpdateDefaultProvider PUT /default-provider。
func (s *Service) UpdateDefaultProvider(ctx context.Context, req *DefaultProviderDTO) error {
	id := strings.TrimSpace(deref(req.DefaultProvider))
	if id == "" {
		return apperr.BadRequest("defaultProvider 不能为空")
	}
	if _, err := s.getProviderEntityOrThrow(id); err != nil {
		return err
	}
	setting, err := s.getSettingOrThrow()
	if err != nil {
		return err
	}
	setting.DefaultChatProviderID = id
	setting.UpdatedAt = time.Now()
	if err := s.db.Save(setting).Error; err != nil {
		return apperr.Wrap(apperr.CodeProviderWriteFailed, "写入默认 Provider 配置失败", err)
	}
	return s.reload()
}

// UpdateDefaultEmbeddingProvider PUT /default-embedding-provider。
func (s *Service) UpdateDefaultEmbeddingProvider(ctx context.Context, req *DefaultProviderDTO) error {
	id := strings.TrimSpace(deref(req.DefaultEmbeddingProvider))
	if id == "" {
		return apperr.BadRequest("defaultEmbeddingProvider 不能为空")
	}
	entity, err := s.getProviderEntityOrThrow(id)
	if err != nil {
		return err
	}
	embeddingModel := strings.TrimSpace(deref(entity.EmbeddingModel))
	if !entity.SupportsEmbedding || embeddingModel == "" {
		return apperr.BadRequest("Provider '" + id + "' 不支持 Embedding，不能设为默认向量服务")
	}
	if err := validateEmbeddingConfig(true, embeddingModel, s.resolveDimensions(entity.EmbeddingDimensions)); err != nil {
		return err
	}
	setting, err := s.getSettingOrThrow()
	if err != nil {
		return err
	}
	setting.DefaultEmbeddingProviderID = id
	setting.UpdatedAt = time.Now()
	if err := s.db.Save(setting).Error; err != nil {
		return apperr.Wrap(apperr.CodeProviderWriteFailed, "写入默认向量 Provider 配置失败", err)
	}
	return s.reload()
}

// ReloadProviders POST /reload。
func (s *Service) ReloadProviders() error {
	return s.reload()
}

func (s *Service) reload() error {
	if s.registry == nil {
		return nil
	}
	if err := s.registry.Reload(); err != nil {
		s.logger.Warn("Provider 注册表刷新失败", slog.String("error", err.Error()))
	}
	return nil
}

// ===== 连通性测试 =====

// TestProvider POST /{id}/test：尝试候选 URL 的 chat/completions。
func (s *Service) TestProvider(ctx context.Context, id string) (*ProviderTestResult, error) {
	entity, err := s.getProviderEntityOrThrow(id)
	if err != nil {
		return nil, err
	}
	apiKey, err := s.crypto.Decrypt(entity.APIKeyNonce, entity.APIKeyCiphertext)
	if err != nil {
		return nil, err
	}
	return testProviderConnectivity(ctx, entity.BaseURL, apiKey, entity.Model), nil
}

// candidateTestURLs 构建连通性测试候选：{base}/chat/completions，
// base 不含版本段时补充 {base}/v1/chat/completions（与 Java ApiPathResolver 一致）。
func candidateTestURLs(baseURL string) []string {
	base := strings.TrimRight(baseURL, "/")
	urls := []string{base + "/chat/completions"}
	if !baseUrlContainsVersion(base) {
		urls = append(urls, base+"/v1/chat/completions")
	}
	return urls
}

func baseUrlContainsVersion(u string) bool {
	// 最后一段形如 v1/v2/... 即视为含版本。
	parts := strings.Split(strings.TrimRight(u, "/"), "/")
	last := parts[len(parts)-1]
	if len(last) >= 2 && (last[0] == 'v' || last[0] == 'V') {
		for _, ch := range last[1:] {
			if ch < '0' || ch > '9' {
				return false
			}
		}
		return true
	}
	return false
}

func testProviderConnectivity(ctx context.Context, baseURL, apiKey, model string) *ProviderTestResult {
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with OK only."},
		},
		"max_tokens": 1,
	}
	payload, _ := json.Marshal(body)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}

	lastFailure := "Unknown error"
	for _, target := range candidateTestURLs(baseURL) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(payload)))
		if err != nil {
			lastFailure = fmt.Sprintf("BadRequest on %s: %v", target, err)
			continue
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastFailure = fmt.Sprintf("RequestError on %s: %v", target, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return &ProviderTestResult{Success: true, Message: "连接成功", Model: model}
		}
		lastFailure = fmt.Sprintf("HTTP %d on %s", resp.StatusCode, target)
	}
	return &ProviderTestResult{Success: false, Message: "连接失败: " + lastFailure, Model: model}
}

// ===== 语音 ASR/TTS 配置 =====

func (s *Service) getVoiceConfig() (asr AsrConfig, tts TtsConfig, apiKey string, err error) {
	var entity VoiceConfigEntity
	if e := s.db.First(&entity, VoiceConfigID).Error; e != nil {
		return AsrConfig{}, TtsConfig{}, "", apperr.New(apperr.CodeVoiceReadFailed, "语音服务配置未初始化")
	}
	if e := json.Unmarshal([]byte(entity.AsrJSON), &asr); e != nil {
		return AsrConfig{}, TtsConfig{}, "", apperr.New(apperr.CodeVoiceReadFailed, "解析 ASR 配置失败")
	}
	if e := json.Unmarshal([]byte(entity.TtsJSON), &tts); e != nil {
		return AsrConfig{}, TtsConfig{}, "", apperr.New(apperr.CodeVoiceReadFailed, "解析 TTS 配置失败")
	}
	apiKey, err = s.crypto.Decrypt(entity.APIKeyNonce, entity.APIKeyCiphertext)
	if err != nil {
		return AsrConfig{}, TtsConfig{}, "", err
	}
	return asr, tts, apiKey, nil
}

// GetAsrConfig GET /voice/asr。
func (s *Service) GetAsrConfig() (*AsrConfigDTO, error) {
	asr, _, apiKey, err := s.getVoiceConfig()
	if err != nil {
		return nil, err
	}
	return &AsrConfigDTO{
		URL:                            asr.URL,
		Model:                          asr.Model,
		MaskedAPIKey:                   MaskAPIKey(apiKey),
		Language:                       asr.Language,
		Format:                         asr.Format,
		SampleRate:                     asr.SampleRate,
		EnableTurnDetection:            asr.EnableTurnDetection,
		TurnDetectionType:              asr.TurnDetectionType,
		TurnDetectionThreshold:         asr.TurnDetectionThreshold,
		TurnDetectionSilenceDurationMs: asr.TurnDetectionSilenceDurationMs,
	}, nil
}

// GetTtsConfig GET /voice/tts。
func (s *Service) GetTtsConfig() (*TtsConfigDTO, error) {
	_, tts, apiKey, err := s.getVoiceConfig()
	if err != nil {
		return nil, err
	}
	return &TtsConfigDTO{
		Model:        tts.Model,
		MaskedAPIKey: MaskAPIKey(apiKey),
		Voice:        tts.Voice,
		Format:       tts.Format,
		SampleRate:   tts.SampleRate,
		Mode:         tts.Mode,
		LanguageType: tts.LanguageType,
		SpeechRate:   tts.SpeechRate,
		Volume:       tts.Volume,
	}, nil
}

// UpdateAsrConfig PUT /voice/asr。API Key 更新时同步作用于 TTS（与 Java 行为一致）。
func (s *Service) UpdateAsrConfig(ctx context.Context, req *AsrConfigRequest) error {
	return s.updateVoice(func(asr *AsrConfig, tts *TtsConfig, apiKey *string) {
		if req.URL != nil {
			asr.URL = *req.URL
		}
		if req.Model != nil {
			asr.Model = *req.Model
		}
		if req.Language != nil {
			asr.Language = *req.Language
		}
		if req.Format != nil {
			asr.Format = *req.Format
		}
		if req.SampleRate != nil {
			asr.SampleRate = *req.SampleRate
		}
		if req.EnableTurnDetection != nil {
			asr.EnableTurnDetection = *req.EnableTurnDetection
		}
		if req.TurnDetectionType != nil {
			asr.TurnDetectionType = *req.TurnDetectionType
		}
		if req.TurnDetectionThreshold != nil {
			asr.TurnDetectionThreshold = *req.TurnDetectionThreshold
		}
		if req.TurnDetectionSilenceDurationMs != nil {
			asr.TurnDetectionSilenceDurationMs = *req.TurnDetectionSilenceDurationMs
		}
		if req.APIKey != nil {
			*apiKey = *req.APIKey
		}
	})
}

// UpdateTtsConfig PUT /voice/tts。
func (s *Service) UpdateTtsConfig(ctx context.Context, req *TtsConfigRequest) error {
	return s.updateVoice(func(asr *AsrConfig, tts *TtsConfig, apiKey *string) {
		if req.Model != nil {
			tts.Model = *req.Model
		}
		if req.Voice != nil {
			tts.Voice = *req.Voice
		}
		if req.Format != nil {
			tts.Format = *req.Format
		}
		if req.SampleRate != nil {
			tts.SampleRate = *req.SampleRate
		}
		if req.Mode != nil {
			tts.Mode = *req.Mode
		}
		if req.LanguageType != nil {
			tts.LanguageType = *req.LanguageType
		}
		if req.SpeechRate != nil {
			tts.SpeechRate = *req.SpeechRate
		}
		if req.Volume != nil {
			tts.Volume = *req.Volume
		}
		if req.APIKey != nil {
			*apiKey = *req.APIKey
		}
	})
}

func (s *Service) updateVoice(mutate func(asr *AsrConfig, tts *TtsConfig, apiKey *string)) error {
	var entity VoiceConfigEntity
	if err := s.db.First(&entity, VoiceConfigID).Error; err != nil {
		return apperr.New(apperr.CodeVoiceReadFailed, "语音服务配置未初始化")
	}
	var asr AsrConfig
	var tts TtsConfig
	if err := json.Unmarshal([]byte(entity.AsrJSON), &asr); err != nil {
		return apperr.New(apperr.CodeVoiceReadFailed, "解析 ASR 配置失败")
	}
	if err := json.Unmarshal([]byte(entity.TtsJSON), &tts); err != nil {
		return apperr.New(apperr.CodeVoiceReadFailed, "解析 TTS 配置失败")
	}
	apiKey, err := s.crypto.Decrypt(entity.APIKeyNonce, entity.APIKeyCiphertext)
	if err != nil {
		return err
	}

	mutate(&asr, &tts, &apiKey)

	encrypted, err := s.crypto.Encrypt(apiKey)
	if err != nil {
		return err
	}
	asrJSON, e := json.Marshal(asr)
	if e != nil {
		return apperr.Wrap(apperr.CodeVoiceWriteFailed, "序列化 ASR 配置失败", e)
	}
	ttsJSON, e := json.Marshal(tts)
	if e != nil {
		return apperr.Wrap(apperr.CodeVoiceWriteFailed, "序列化 TTS 配置失败", e)
	}
	entity.AsrJSON = string(asrJSON)
	entity.TtsJSON = string(ttsJSON)
	entity.APIKeyNonce = encrypted.Nonce
	entity.APIKeyCiphertext = encrypted.Ciphertext
	entity.UpdatedAt = time.Now()
	if err := s.db.Save(&entity).Error; err != nil {
		return apperr.Wrap(apperr.CodeVoiceWriteFailed, "写入语音服务配置失败", err)
	}
	return nil
}

// TestAsrConfig POST /voice/asr/test：TCP 可达性探测，与 Java 版一致。
func (s *Service) TestAsrConfig(ctx context.Context) (*ProviderTestResult, error) {
	asr, _, _, err := s.getVoiceConfig()
	if err != nil {
		return nil, err
	}
	host, port := wsHostPort(asr.URL)
	address := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		return &ProviderTestResult{Success: false, Message: "ASR 连接失败: " + err.Error(), Model: asr.Model}, nil
	}
	_ = conn.Close()
	return &ProviderTestResult{
		Success: true,
		Message: "ASR 服务网络端口可达: " + host + "；尚未验证 API Key、模型权限及语音识别能力",
		Model:   asr.Model,
	}, nil
}

// wsHostPort 解析 wss://host:port/path。
func wsHostPort(rawURL string) (string, int) {
	port := 80
	if strings.HasPrefix(rawURL, "wss://") || strings.HasPrefix(rawURL, "https://") {
		port = 443
	}
	u := rawURL
	if idx := strings.Index(u, "://"); idx >= 0 {
		u = u[idx+3:]
	}
	if idx := strings.IndexAny(u, "/?"); idx >= 0 {
		u = u[:idx]
	}
	if idx := strings.LastIndexByte(u, ':'); idx >= 0 {
		if p := parseIntSafe(u[idx+1:]); p > 0 {
			return u[:idx], p
		}
		return u[:idx], port
	}
	return u, port
}

func parseIntSafe(s string) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return -1
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

// ===== 辅助 =====

func (s *Service) resolveDimensions(v *int) *int {
	if v != nil && *v > 0 {
		return v
	}
	d := s.DefaultEmbeddingDimensions
	return &d
}

func validateEmbeddingConfig(supportsEmbedding bool, embeddingModel string, dimensions *int) error {
	if !supportsEmbedding {
		return nil
	}
	if embeddingModel == "" {
		return apperr.BadRequest("支持 Embedding 的 Provider 必须填写 embeddingModel")
	}
	if dimensions == nil || *dimensions <= 0 {
		return apperr.BadRequest("向量维度必须为正整数")
	}
	return nil
}

func requireNonBlank(v, field string) (string, error) {
	t := strings.TrimSpace(v)
	if t == "" {
		return "", apperr.BadRequest(field + " 不能为空")
	}
	return t, nil
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func strPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func intPtr(v int) *int { return &v }
