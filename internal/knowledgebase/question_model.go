// 知识库题库实体与出题状态机枚举（Java KnowledgeBaseQuestionEntity /
// QuestionGenStatus / KnowledgeBaseQuestionStatus / QuestionGenerationConfig），
// 对应表 knowledge_base_questions 与 knowledge_bases 的 question_gen_* 列
// （Java schema，应用不做迁移，ADR 0001）。
package knowledgebase

import "time"

// QuestionGenStatus 出题状态机（Java QuestionGenStatus 枚举）。
type QuestionGenStatus string

const (
	QuestionGenNone       QuestionGenStatus = "NONE"       // 未开始
	QuestionGenQueued     QuestionGenStatus = "QUEUED"     // 等待处理
	QuestionGenProcessing QuestionGenStatus = "PROCESSING" // 生成中
	QuestionGenCompleted  QuestionGenStatus = "COMPLETED"  // 生成成功
	QuestionGenFailed     QuestionGenStatus = "FAILED"     // 生成失败
)

// QuestionBankStatus 题目状态（Java KnowledgeBaseQuestionStatus 枚举）。
type QuestionBankStatus string

const (
	QuestionDraft    QuestionBankStatus = "DRAFT"
	QuestionActive   QuestionBankStatus = "ACTIVE"
	QuestionArchived QuestionBankStatus = "ARCHIVED"
	QuestionStale    QuestionBankStatus = "STALE"
)

// QuestionSkillID 题目固定 skill_id（Java KnowledgeBaseQuestionEntity.DEFAULT_SKILL_ID）。
const QuestionSkillID = "knowledge-base"

// KnowledgeBaseQuestionEntity 对应表 knowledge_base_questions。
type KnowledgeBaseQuestionEntity struct {
	ID              int64              `gorm:"column:id;primaryKey;autoIncrement"`
	KnowledgeBaseID int64              `gorm:"column:knowledge_base_id;not null"`
	SkillID         string             `gorm:"column:skill_id;size:64;not null"`
	Difficulty      *string            `gorm:"column:difficulty;size:16"`
	Type            *string            `gorm:"column:type;size:64"`
	Category        *string            `gorm:"column:category;size:64"`
	Question        string             `gorm:"column:question;type:text;not null"`
	TopicSummary    *string            `gorm:"column:topic_summary;size:300"`
	ReferenceAnswer *string            `gorm:"column:reference_answer;type:text"`
	KeyPointsJSON   *string            `gorm:"column:key_points_json;type:text"`
	ScoringRubric   *string            `gorm:"column:scoring_rubric;type:text"`
	FollowUpsJSON   *string            `gorm:"column:follow_ups_json;type:text"`
	SourceContext   *string            `gorm:"column:source_context;type:text"`
	KBContentHash   *string            `gorm:"column:kb_content_hash;size:64"`
	Status          QuestionBankStatus `gorm:"column:status;size:20;not null"`
	CreatedAt       time.Time          `gorm:"column:created_at;not null"`
	UpdatedAt       time.Time          `gorm:"column:updated_at;not null"`
}

func (KnowledgeBaseQuestionEntity) TableName() string { return "knowledge_base_questions" }

// QuestionGenerationConfig 出题参数快照（Java record 同名；llmProvider 可空，
// JSON 序列化含 null 键与 Java Jackson 一致）。
type QuestionGenerationConfig struct {
	Difficulty    string  `json:"difficulty"`
	QuestionCount int     `json:"questionCount"`
	FollowUpCount int     `json:"followUpCount"`
	CategoryLimit int     `json:"categoryLimit"`
	LLMProvider   *string `json:"llmProvider"`
}
