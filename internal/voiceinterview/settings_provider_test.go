// VoiceConfigSource 生产适配器单测：llmprovider 持久化结构 → 语音模块运行时配置
// 的字段映射（Key 明文透传，调用方负责不落日志）。
package voiceinterview

import (
	"testing"

	"github.com/wuzuxian/interview/internal/llmprovider"
)

func TestToAsrSettings字段映射(t *testing.T) {
	asr := llmprovider.AsrConfig{
		URL:                            "wss://dashscope.aliyuncs.com/api-ws/v1/realtime",
		Model:                          "qwen3-asr-flash-realtime",
		Language:                       "zh",
		Format:                         "pcm",
		SampleRate:                     16000,
		EnableTurnDetection:            true,
		TurnDetectionType:              "server_vad",
		TurnDetectionThreshold:         0.5,
		TurnDetectionSilenceDurationMs: 2000,
	}
	got := toAsrSettings(asr, "sk-plain-key")
	want := AsrSettings{
		URL:                            asr.URL,
		Model:                          asr.Model,
		APIKey:                         "sk-plain-key",
		Language:                       asr.Language,
		Format:                         asr.Format,
		SampleRate:                     asr.SampleRate,
		EnableTurnDetection:            asr.EnableTurnDetection,
		TurnDetectionType:              asr.TurnDetectionType,
		TurnDetectionThreshold:         asr.TurnDetectionThreshold,
		TurnDetectionSilenceDurationMs: asr.TurnDetectionSilenceDurationMs,
	}
	if got != want {
		t.Errorf("ASR 映射不一致:\n got %+v\nwant %+v", got, want)
	}
}

func TestToTtsSettings字段映射(t *testing.T) {
	tts := llmprovider.TtsConfig{
		Model:        "qwen3-tts-flash-realtime",
		Voice:        "Cherry",
		Format:       "pcm",
		SampleRate:   24000,
		Mode:         "commit",
		LanguageType: "zh",
		SpeechRate:   1.0,
		Volume:       50,
	}
	got := toTtsSettings(tts, "sk-plain-key")
	want := TtsSettings{
		// URL 保持空：voice_config.tts_json 无 url 键（Java SDK 内置端点）。
		URL:          "",
		Model:        tts.Model,
		APIKey:       "sk-plain-key",
		Voice:        tts.Voice,
		Format:       tts.Format,
		SampleRate:   tts.SampleRate,
		Mode:         tts.Mode,
		LanguageType: tts.LanguageType,
		SpeechRate:   tts.SpeechRate,
		Volume:       tts.Volume,
	}
	if got != want {
		t.Errorf("TTS 映射不一致:\n got %+v\nwant %+v", got, want)
	}
}
