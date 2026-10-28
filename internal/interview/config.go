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
}
