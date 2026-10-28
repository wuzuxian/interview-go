// Package voiceinterview 模拟面试-语音面试模块：会话管理、WebSocket 实时管道
// （ASR 字幕 → 提交 → LLM 流式回复 → 逐句 TTS 分块音频）、阶段机、上下文压缩
// 与异步评估。契约与 Java 版 voiceinterview 模块逐字对齐
// （spec：.scratch/voice-interview/spec.md；ADR 0001/0002/0003）。
package voiceinterview

import "context"

// AsrSettings 语音识别运行时配置（来自设置模块 voice_config 表；APIKey 已解密，
// 仅用于与 DashScope 握手，不得写入日志或错误信息）。
// 字段形状对齐设置模块的 AsrConfig 持久化结构。
type AsrSettings struct {
	URL                            string
	Model                          string
	APIKey                         string
	Language                       string
	Format                         string
	SampleRate                     int
	EnableTurnDetection            bool
	TurnDetectionType              string
	TurnDetectionThreshold         float64
	TurnDetectionSilenceDurationMs int
}

// TtsSettings 语音合成运行时配置（同上；与 ASR 共享同一把 APIKey）。
type TtsSettings struct {
	Model        string
	APIKey       string
	Voice        string
	Format       string
	SampleRate   int
	Mode         string
	LanguageType string
	SpeechRate   float64
	Volume       int
}

// VoiceConfigSource 语音运行时配置源（spec 实现决策 19 的两个新增接缝之一）。
// 生产实现由设置模块满足（读取 voice_config 表并解密 Key，设置接口修改后热生效）；
// 测试提供静态实现。
type VoiceConfigSource interface {
	VoiceAsrSettings(ctx context.Context) (AsrSettings, error)
	VoiceTtsSettings(ctx context.Context) (TtsSettings, error)
}
