// VoiceConfigSource 生产实现：读设置模块 voice_config 表并解密 Key（spec 实现决策
// 19 的「语音配置源」接缝）。设置接口（/api/llm-provider/voice/*）修改即落库，
// 语音 WS 每次建连重读即热生效。适配器在本模块内做字段映射，避免暴露设置模块
// 持久化结构给网关消费方。
package voiceinterview

import (
	"context"

	"github.com/wuzuxian/interview/internal/llmprovider"
)

// providerConfigSource 基于 llmprovider.Service 的语音配置源。
type providerConfigSource struct {
	svc *llmprovider.Service
}

// NewProviderConfigSource 创建生产配置源（main 接线；测试注入静态实现）。
func NewProviderConfigSource(svc *llmprovider.Service) VoiceConfigSource {
	return &providerConfigSource{svc: svc}
}

// VoiceAsrSettings 读取 ASR 运行时配置（含解密 Key）。
func (s *providerConfigSource) VoiceAsrSettings(ctx context.Context) (AsrSettings, error) {
	asr, _, apiKey, err := s.svc.VoiceRuntimeConfig()
	if err != nil {
		return AsrSettings{}, err
	}
	return toAsrSettings(asr, apiKey), nil
}

// VoiceTtsSettings 读取 TTS 运行时配置（与 ASR 共享同一把解密 Key）。
func (s *providerConfigSource) VoiceTtsSettings(ctx context.Context) (TtsSettings, error) {
	_, tts, apiKey, err := s.svc.VoiceRuntimeConfig()
	if err != nil {
		return TtsSettings{}, err
	}
	return toTtsSettings(tts, apiKey), nil
}

// toAsrSettings 设置模块 ASR 持久化结构 → 语音模块运行时配置（字段一一对应）。
func toAsrSettings(asr llmprovider.AsrConfig, apiKey string) AsrSettings {
	return AsrSettings{
		URL:                            asr.URL,
		Model:                          asr.Model,
		APIKey:                         apiKey,
		Language:                       asr.Language,
		Format:                         asr.Format,
		SampleRate:                     asr.SampleRate,
		EnableTurnDetection:            asr.EnableTurnDetection,
		TurnDetectionType:              asr.TurnDetectionType,
		TurnDetectionThreshold:         asr.TurnDetectionThreshold,
		TurnDetectionSilenceDurationMs: asr.TurnDetectionSilenceDurationMs,
	}
}

// toTtsSettings 设置模块 TTS 持久化结构 → 语音模块运行时配置。
// URL 保持空：voice_config.tts_json 无 url 键（Java TTS SDK 内置端点，无 URL 配置项），
// TtsSettings.URL 供测试指向进程内假服务端；生产默认端点由票 06 的合成管道补齐。
func toTtsSettings(tts llmprovider.TtsConfig, apiKey string) TtsSettings {
	return TtsSettings{
		Model:        tts.Model,
		APIKey:       apiKey,
		Voice:        tts.Voice,
		Format:       tts.Format,
		SampleRate:   tts.SampleRate,
		Mode:         tts.Mode,
		LanguageType: tts.LanguageType,
		SpeechRate:   tts.SpeechRate,
		Volume:       tts.Volume,
	}
}
