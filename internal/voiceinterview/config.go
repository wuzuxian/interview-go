// Config app.voice_interview 配置段（键为项目惯例 snake_case，非 Java kebab-case，
// spec 有意偏离 #4）。默认值来源：Java application.yml 的 app.voice-interview 与
// VoiceInterviewWebSocketHandler/Service 中的常量，逐字一致。
// 阶段判据（票 05）与上下文压缩（票 07）的子配置由对应票在 Config 末尾追加字段，
// 以减少并行合并冲突。
package voiceinterview

import "time"

// Config 语音面试行为配置（config.yaml 的 app.voice_interview 段）。
type Config struct {
	// 会话缓存（Java Redisson bucket TTL 1h）。
	SessionCacheTTL time.Duration `config:"session_cache_ttl" default:"1h"`

	// ASR 行为参数（Java Handler/QwenAsrService 常量的配置化）。
	AsrReadyTimeout        time.Duration `config:"asr_ready_timeout" default:"10s"`
	AsrReadyRetries        int           `config:"asr_ready_retries" default:"2"`
	AsrRestartPause        time.Duration `config:"asr_restart_pause" default:"200ms"`
	AsrRestartReadyTimeout time.Duration `config:"asr_restart_ready_timeout" default:"1s"`
	AsrResendMaxAttempts   int           `config:"asr_resend_max_attempts" default:"15"`
	AsrResendInterval      time.Duration `config:"asr_resend_interval" default:"80ms"`

	// 回合管道（Java：回声冷却 800ms、分块音频默认开、语音化清洗上限 120 字、
	// 流式推送间隔 180ms 且增量 12 字符、逐句 TTS 并发 3）。
	EchoCooldown               time.Duration `config:"echo_cooldown" default:"800ms"`
	ChunkedAudioEnabled        bool          `config:"chunked_audio_enabled" default:"true"`
	AIQuestionMaxChars         int           `config:"ai_question_max_chars" default:"120"`
	LLMStreamPushInterval      time.Duration `config:"llm_stream_push_interval" default:"180ms"`
	LLMStreamMinCharsDelta     int           `config:"llm_stream_min_chars_delta" default:"12"`
	MaxConcurrentTTSPerSession int           `config:"max_concurrent_tts_per_session" default:"3"`

	// TTS 超时（Java tts-connect-timeout-seconds=5 / tts-timeout-seconds=8 / 整段 30s 常量）。
	TTSConnectTimeout time.Duration `config:"tts_connect_timeout" default:"5s"`
	TTSTimeout        time.Duration `config:"tts_timeout" default:"8s"`
	TTSFullTimeout    time.Duration `config:"tts_full_timeout" default:"30s"`

	// 开场白（Java opening-audio-warmup-enabled 默认 false）。
	OpeningAudioWarmupEnabled bool `config:"opening_audio_warmup_enabled" default:"false"`

	// WebSocket 网关（Java：连接级 256KB 消息上限、发送装饰器 10s 写超时）。
	WSMessageLimit int64         `config:"ws_message_limit" default:"262144"`
	WSWriteTimeout time.Duration `config:"ws_write_timeout" default:"10s"`

	// 不活动暂停（Java 常量：30s 巡检、4:30 预警、5:00 暂停）。
	InactivityCheckInterval time.Duration `config:"inactivity_check_interval" default:"30s"`
	InactivityWarnAfter     time.Duration `config:"inactivity_warn_after" default:"270s"`
	InactivityPauseAfter    time.Duration `config:"inactivity_pause_after" default:"300s"`

	// 僵尸清理与评估恢复（Java cleanupStaleSessions：60s 巡检、2h 强制结束、
	// 3min PENDING 补投、30min PROCESSING 置败）。
	CleanupInterval             time.Duration `config:"cleanup_interval" default:"60s"`
	StaleInProgressAfter        time.Duration `config:"stale_in_progress_after" default:"2h"`
	EvaluatePendingRequeueAfter time.Duration `config:"evaluate_pending_requeue_after" default:"3m"`
	EvaluateProcessingFailAfter time.Duration `config:"evaluate_processing_fail_after" default:"30m"`

	// 后续票追加字段（票 05：Phase 阶段判据；票 07：ContextCompression 上下文压缩）。
}
