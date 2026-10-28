// Config app.interview 配置段，键与 Java 版对齐；本票先落创建链路所需项，
// 出题/评估相关默认值随票 07/08 扩充（Java 无对应配置的项为本仓库约定，注释注明）。
package interview

import "time"

// DefaultSkillID / DefaultDifficulty 对齐 Java CommonConstants.InterviewDefaults。
const (
	DefaultSkillID    = "java-backend"
	DefaultDifficulty = "mid"
)

// Config 创建链路配置（config.yaml 的 app.interview 段）。
type Config struct {
	// QuestionCountMin / QuestionCountMax 题量边界（Java CreateInterviewRequest
	// 上的 @Min(3)/@Max(20)；裁决 #1：Go 必须校验）。
	QuestionCountMin int `config:"question_count_min" default:"3"`
	QuestionCountMax int `config:"question_count_max" default:"20"`
	// FollowUpCount 每题追问数，配置值被钳制到 0-2（对齐 Java follow-up-count，默认 1）。
	FollowUpCount int `config:"follow_up_count" default:"1"`
	// SessionCacheTTL 会话缓存过期时间（Java SESSION_TTL = 24h）。
	SessionCacheTTL time.Duration `config:"session_cache_ttl" default:"24h"`
	// CreateLockWait / CreateLockLease 创建锁等待与持有时间（Java executeWithLock(185, 600, SECONDS)）。
	CreateLockWait  time.Duration `config:"create_lock_wait" default:"185s"`
	CreateLockLease time.Duration `config:"create_lock_lease" default:"600s"`
	// CreateResultTTL 幂等结果键 TTL（Java CREATE_RESULT_TTL = 1 天）。
	CreateResultTTL time.Duration `config:"create_result_ttl" default:"24h"`
	// 历史去重（出题去重输入；Java findTop10… 取最近 10 场、MAX_HISTORICAL_QUESTIONS=60）。
	HistoryMaxSessions  int `config:"history_max_sessions" default:"10"`
	HistoryMaxQuestions int `config:"history_max_questions" default:"60"`
	// 参考题库截断长度（Java MAX_SINGLE_REFERENCE_CHARS=3000 /
	// MAX_REFERENCE_SECTION_CHARS=12000 / MAX_EVALUATION_REFERENCE_SECTION_CHARS=6000）。
	MaxSingleReferenceChars            int `config:"max_single_reference_chars" default:"3000"`
	MaxReferenceSectionChars           int `config:"max_reference_section_chars" default:"12000"`
	MaxEvaluationReferenceSectionChars int `config:"max_evaluation_reference_section_chars" default:"6000"`
	// RateLimit 接口限流额度（对齐 Java @RateLimit 注解参数，spec 决策 12）。
	RateLimit RateLimit `config:"rate_limit"`
	// Evaluation 评估引擎配置（app.interview.evaluation，Java InterviewEvaluationProperties）。
	Evaluation Evaluation `config:"evaluation"`
}

// RateLimit 接口限流额度（单位：次/秒，窗口固定 1s 对齐 Java @RateLimit 默认
// interval=1/timeUnit=SECONDS；额度逐字对应三个 Controller 的注解参数）。
type RateLimit struct {
	CreateGlobalPerSecond int `config:"create_global_per_second" default:"5"`
	CreateIPPerSecond     int `config:"create_ip_per_second" default:"5"`
	SubmitGlobalPerSecond int `config:"submit_global_per_second" default:"10"`
	ParseJdIPPerSecond    int `config:"parse_jd_ip_per_second" default:"5"`
}

// Evaluation 评估引擎配置（票 06，键与 Java app.interview.evaluation 对齐；
// resume_max_chars / max_reference_chars 为 Java 常量的配置化，默认值逐字一致）。
type Evaluation struct {
	BatchSize             int  `config:"batch_size" default:"8"`
	FallbackSplitEnabled  bool `config:"fallback_split_enabled" default:"true"`
	FallbackMaxExtraCalls int  `config:"fallback_max_extra_calls" default:"6"`
	FallbackMinGroups     int  `config:"fallback_min_groups" default:"2"`
	// ResumeMaxChars 简历摘要截断长度（Java 3000）。
	ResumeMaxChars int `config:"resume_max_chars" default:"3000"`
	// MaxReferenceChars 参考基线截断长度（Java MAX_REFERENCE_CONTEXT_CHARS=6000）。
	MaxReferenceChars int `config:"max_reference_chars" default:"6000"`
}

// EngineConfig 转评估引擎配置（引擎构造自带钳制：batchSize≥1、预算≥0、最小组数≥2）。
func (e Evaluation) EngineConfig() EvaluationConfig {
	return EvaluationConfig{
		BatchSize:             e.BatchSize,
		FallbackSplitEnabled:  e.FallbackSplitEnabled,
		FallbackMaxExtraCalls: e.FallbackMaxExtraCalls,
		FallbackMinGroups:     e.FallbackMinGroups,
		ResumeMaxChars:        e.ResumeMaxChars,
		MaxReferenceChars:     e.MaxReferenceChars,
	}
}
