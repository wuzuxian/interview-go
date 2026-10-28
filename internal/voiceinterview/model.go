// SessionEntity / MessageEntity / EvaluationEntity 对应表 voice_interview_sessions /
// voice_interview_messages / voice_interview_evaluations。列名与 Java 版
// V1__init_schema.sql（221-281 行）+ V20260904（summary_covered_sequence_num 列与
// idx_voice_messages_session_seq 索引）一一对应；本仓库不为这三张表建表/迁移
// （ADR 0001，schema 由 Java Flyway / DBA 管理，共享库中已存在）。
// 全部列名用显式 column 标签：messages."timestamp" 是 SQL 保留字列（Java DDL 原样
// 带引号），GORM 生成的 SQL 一律给列名加引号，显式标签保证命名不依赖约定推断。
package voiceinterview

import "time"

// VoicePhase 面试阶段，对齐 Java VoiceInterviewSessionEntity.InterviewPhase。
type VoicePhase string

const (
	PhaseIntro     VoicePhase = "INTRO"
	PhaseTech      VoicePhase = "TECH"
	PhaseProject   VoicePhase = "PROJECT"
	PhaseHR        VoicePhase = "HR"
	PhaseCompleted VoicePhase = "COMPLETED"
)

// VoiceSessionStatus 会话状态，对齐 Java VoiceInterviewSessionStatus。
type VoiceSessionStatus string

const (
	VoiceStatusInProgress VoiceSessionStatus = "IN_PROGRESS"
	VoiceStatusPaused     VoiceSessionStatus = "PAUSED"
	VoiceStatusCompleted  VoiceSessionStatus = "COMPLETED"
	VoiceStatusFailed     VoiceSessionStatus = "FAILED"
)

// VoiceEvaluateStatus 异步评估状态，对齐 Java AsyncTaskStatus。
type VoiceEvaluateStatus string

const (
	VoiceEvaluatePending   VoiceEvaluateStatus = "PENDING"
	VoiceEvaluateProcess   VoiceEvaluateStatus = "PROCESSING"
	VoiceEvaluateCompleted VoiceEvaluateStatus = "COMPLETED"
	VoiceEvaluateFailed    VoiceEvaluateStatus = "FAILED"
)

// 消息类型取值（Java VoiceInterviewMessageEntity.MESSAGE_TYPE_SUMMARY 常量；
// 表 CHECK 无约束，实际只写 DIALOGUE 与 SUMMARY 两种值）。
const (
	MessageDialogue = "DIALOGUE"
	MessageSummary  = "SUMMARY"
)

// SessionEntity 对应表 voice_interview_sessions（列清单 = V1 schema L246-278）。
// 时间列全部指针：DDL 全部可空，空值保持 null 而不是零值时间。
// created_at/updated_at 由 GORM 按字段名自动填充（插入/更新时），语义对齐 Java
// @PrePersist/@PreUpdate；start_time 由 repo 在插入时补齐（@PrePersist 一并写入）。
type SessionEntity struct {
	ID              int64                `gorm:"column:id;primaryKey;autoIncrement"`
	UserID          *string              `gorm:"column:user_id;size:255"`
	RoleType        string               `gorm:"column:role_type;size:255;not null"`
	SkillID         *string              `gorm:"column:skill_id;size:64"`
	Difficulty      *string              `gorm:"column:difficulty;size:16"`
	CustomJdText    *string              `gorm:"column:custom_jd_text;type:text"`
	ResumeID        *int64               `gorm:"column:resume_id"`
	IntroEnabled    *bool                `gorm:"column:intro_enabled"`
	TechEnabled     *bool                `gorm:"column:tech_enabled"`
	ProjectEnabled  *bool                `gorm:"column:project_enabled"`
	HrEnabled       *bool                `gorm:"column:hr_enabled"`
	LLMProvider     *string              `gorm:"column:llm_provider;size:50"`
	CurrentPhase    *VoicePhase          `gorm:"column:current_phase;size:255"`
	Status          *VoiceSessionStatus  `gorm:"column:status;size:255"`
	PlannedDuration *int                 `gorm:"column:planned_duration"`
	ActualDuration  *int                 `gorm:"column:actual_duration"`
	StartTime       *time.Time           `gorm:"column:start_time"`
	EndTime         *time.Time           `gorm:"column:end_time"`
	CreatedAt       *time.Time           `gorm:"column:created_at"`
	UpdatedAt       *time.Time           `gorm:"column:updated_at"`
	PausedAt        *time.Time           `gorm:"column:paused_at"`
	ResumedAt       *time.Time           `gorm:"column:resumed_at"`
	EvaluateStatus  *VoiceEvaluateStatus `gorm:"column:evaluate_status;size:255"`
	EvaluateError   *string              `gorm:"column:evaluate_error;size:500"`
}

func (SessionEntity) TableName() string { return "voice_interview_sessions" }

// MessageEntity 对应表 voice_interview_messages（V1 schema L226-239 + V20260904 的
// summary_covered_sequence_num）。timestamp 为保留字列，显式 column 标签映射。
type MessageEntity struct {
	ID                        int64       `gorm:"column:id;primaryKey;autoIncrement"`
	SessionID                 int64       `gorm:"column:session_id"`
	MessageType               string      `gorm:"column:message_type;size:255;not null"`
	Phase                     *VoicePhase `gorm:"column:phase;size:255"`
	UserRecognizedText        *string     `gorm:"column:user_recognized_text;type:text"`
	AiGeneratedText           *string     `gorm:"column:ai_generated_text;type:text"`
	Timestamp                 *time.Time  `gorm:"column:timestamp"`
	SequenceNum               *int        `gorm:"column:sequence_num"`
	SummaryCoveredSequenceNum *int        `gorm:"column:summary_covered_sequence_num"`
	CreatedAt                 *time.Time  `gorm:"column:created_at"`
}

func (MessageEntity) TableName() string { return "voice_interview_messages" }

// EvaluationEntity 对应表 voice_interview_evaluations（V1 schema L221-234，
// session_id 有 UNIQUE 约束；JSON 列全部 TEXT）。票 08 评估链路读写，
// 本票仅提供级联删除所需的表映射。
type EvaluationEntity struct {
	ID                      int64      `gorm:"column:id;primaryKey;autoIncrement"`
	SessionID               *int64     `gorm:"column:session_id"`
	OverallScore            *int       `gorm:"column:overall_score"`
	OverallFeedback         *string    `gorm:"column:overall_feedback;type:text"`
	QuestionEvaluationsJSON *string    `gorm:"column:question_evaluations_json;type:text"`
	StrengthsJSON           *string    `gorm:"column:strengths_json;type:text"`
	ImprovementsJSON        *string    `gorm:"column:improvements_json;type:text"`
	ReferenceAnswersJSON    *string    `gorm:"column:reference_answers_json;type:text"`
	InterviewerRole         *string    `gorm:"column:interviewer_role;size:255"`
	InterviewDate           *time.Time `gorm:"column:interview_date"`
	CreatedAt               *time.Time `gorm:"column:created_at"`
}

func (EvaluationEntity) TableName() string { return "voice_interview_evaluations" }
