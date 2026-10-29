// 知识库题库 DTO（票 04）：字段名与 Java QuestionGenStatusResponse /
// QuestionGenerationConfig / KnowledgeBaseQuestionDTO / KnowledgeBaseQuestionFollowUpDTO /
// CategoryCount 逐字对齐（可空列输出 JSON null 与 Java Jackson 一致）。
package knowledgebase

import "time"

// QuestionGenStatusResponse GET /questions/generation-status 与 generate 的响应体
// （Java QuestionGenStatusResponse record；计数为原生 int，可空字段输出 null）。
type QuestionGenStatusResponse struct {
	KnowledgeBaseID   int64                     `json:"knowledgeBaseId"`
	QuestionGenStatus string                    `json:"questionGenStatus"`
	QuestionGenTaskID *string                   `json:"questionGenTaskId"`
	QuestionGenConfig *QuestionGenerationConfig `json:"questionGenConfig"`
	SavedCount        int                       `json:"savedCount"`
	SkippedCount      int                       `json:"skippedCount"`
	Message           *string                   `json:"message"`
	Error             *string                   `json:"error"`
	UpdatedAt         *time.Time                `json:"updatedAt"`
}

// QuestionFollowUpDTO 追问（Java KnowledgeBaseQuestionFollowUpDTO record）。
type QuestionFollowUpDTO struct {
	Question        string   `json:"question"`
	ReferenceAnswer *string  `json:"referenceAnswer"`
	KeyPoints       []string `json:"keyPoints"`
	ScoringRubric   *string  `json:"scoringRubric"`
}

// KnowledgeBaseQuestionDTO 题库题目 DTO（Java KnowledgeBaseQuestionDTO record；
// keyPoints/followUps 无数据时输出空数组，与 Java readStringList/readFollowUps 的
// List.of() 兜底一致）。
type KnowledgeBaseQuestionDTO struct {
	ID                int64                 `json:"id"`
	KnowledgeBaseID   int64                 `json:"knowledgeBaseId"`
	KnowledgeBaseName *string               `json:"knowledgeBaseName"`
	SkillID           string                `json:"skillId"`
	Difficulty        *string               `json:"difficulty"`
	Type              *string               `json:"type"`
	Category          *string               `json:"category"`
	Question          string                `json:"question"`
	TopicSummary      *string               `json:"topicSummary"`
	ReferenceAnswer   *string               `json:"referenceAnswer"`
	KeyPoints         []string              `json:"keyPoints"`
	ScoringRubric     *string               `json:"scoringRubric"`
	FollowUps         []QuestionFollowUpDTO `json:"followUps"`
	SourceContext     *string               `json:"sourceContext"`
	Status            string                `json:"status"`
	CreatedAt         time.Time             `json:"createdAt"`
	UpdatedAt         time.Time             `json:"updatedAt"`
}

// CategoryCount 方向统计（Java KnowledgeBaseQuestionRepository.CategoryCount：
// getCategory/getCount 接口视图，Jackson 序列化为 {category, count}）。
type CategoryCount struct {
	Category string `json:"category"`
	Count    int64  `json:"count"`
}
