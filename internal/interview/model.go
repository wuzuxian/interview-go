// SessionEntity / AnswerEntity 对应表 interview_sessions / interview_answers。
// 列名与 Java 版 V1__init_schema.sql（含 V20260803 补的 request_id 列）一一对应，
// snake_case + gorm column tag；本仓库不为这两张表建表/迁移（ADR 0001，schema 由 DBA 管理）。
package interview

import "time"

// SessionStatus 面试场次状态机（单向推进），对齐 Java InterviewSessionEntity.SessionStatus。
type SessionStatus string

const (
	StatusCreated    SessionStatus = "CREATED"
	StatusInProgress SessionStatus = "IN_PROGRESS"
	StatusCompleted  SessionStatus = "COMPLETED"
	StatusEvaluated  SessionStatus = "EVALUATED"
)

// EvaluateStatus 异步评估状态，对齐 Java AsyncTaskStatus。
type EvaluateStatus string

const (
	EvaluatePending   EvaluateStatus = "PENDING"
	EvaluateProcess   EvaluateStatus = "PROCESSING"
	EvaluateCompleted EvaluateStatus = "COMPLETED"
	EvaluateFailed    EvaluateStatus = "FAILED"
)

// SourceType 场次来源，对齐 Java persistence 层默认值。
type SourceType string

const (
	SourceNormal        SourceType = "NORMAL"
	SourceKnowledgeBase SourceType = "KNOWLEDGE_BASE"
)

// IsUnfinished 场次是否仍未完成（复用判定与未完成映射共用）。
func IsUnfinished(s SessionStatus) bool {
	return s == StatusCreated || s == StatusInProgress
}

// SessionEntity 对应表 interview_sessions（列清单 = V1 schema 58-84 行 + V20260803 request_id）。
type SessionEntity struct {
	ID                   int64           `gorm:"column:id;primaryKey;autoIncrement"`
	SessionID            string          `gorm:"column:session_id;size:36;not null;uniqueIndex:uk_interview_sessions_session_id"`
	RequestID            *string         `gorm:"column:request_id;size:64;uniqueIndex:uk_interview_sessions_request_id"`
	SkillID              *string         `gorm:"column:skill_id;size:64"`
	Difficulty           *string         `gorm:"column:difficulty;size:16"`
	ResumeID             *int64          `gorm:"column:resume_id"`
	TotalQuestions       *int            `gorm:"column:total_questions"`
	CurrentQuestionIndex *int            `gorm:"column:current_question_index"`
	Status               *SessionStatus  `gorm:"column:status;size:20"`
	QuestionsJSON        *string         `gorm:"column:questions_json;type:text"`
	OverallScore         *int            `gorm:"column:overall_score"`
	OverallFeedback      *string         `gorm:"column:overall_feedback;type:text"`
	StrengthsJSON        *string         `gorm:"column:strengths_json;type:text"`
	ImprovementsJSON     *string         `gorm:"column:improvements_json;type:text"`
	ReferenceAnswersJSON *string         `gorm:"column:reference_answers_json;type:text"`
	CreatedAt            time.Time       `gorm:"column:created_at;not null"`
	CompletedAt          *time.Time      `gorm:"column:completed_at"`
	EvaluateStatus       *EvaluateStatus `gorm:"column:evaluate_status;size:20"`
	EvaluateError        *string         `gorm:"column:evaluate_error;size:500"`
	LLMProvider          *string         `gorm:"column:llm_provider;size:50"`
	SourceType           *SourceType     `gorm:"column:source_type;size:32"`
	KnowledgeBaseID      *int64          `gorm:"column:knowledge_base_id"`
	InterviewCategory    *string         `gorm:"column:interview_category;size:64"`
}

func (SessionEntity) TableName() string { return "interview_sessions" }

// AnswerEntity 对应表 interview_answers，(session_id, question_index) 唯一。
type AnswerEntity struct {
	ID              int64     `gorm:"column:id;primaryKey;autoIncrement"`
	SessionID       int64     `gorm:"column:session_id;not null"` // interview_sessions.id（内部自增主键）
	QuestionIndex   *int      `gorm:"column:question_index"`
	Question        *string   `gorm:"column:question;type:text"`
	Category        *string   `gorm:"column:category;size:255"`
	UserAnswer      *string   `gorm:"column:user_answer;type:text"`
	Score           *int      `gorm:"column:score"`
	Feedback        *string   `gorm:"column:feedback;type:text"`
	ReferenceAnswer *string   `gorm:"column:reference_answer;type:text"`
	KeyPointsJSON   *string   `gorm:"column:key_points_json;type:text"`
	AnsweredAt      time.Time `gorm:"column:answered_at;not null"`
}

func (AnswerEntity) TableName() string { return "interview_answers" }
