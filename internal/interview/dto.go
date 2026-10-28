package interview

import "time"

// SkillDTO GET /api/interview/skills 与 /{id} 的响应体，
// 字段名与 Java 版 InterviewSkillService.SkillDTO（record 驼峰序列化）逐字一致：
// id/name/description/categories/isPreset/sourceJd/persona/display。
// sourceJd 对预设方向恒为 null（JSON 保留 null，与 Java 默认 Jackson 行为一致）。
type SkillDTO struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Categories  []CategoryDTO `json:"categories"`
	IsPreset    bool          `json:"isPreset"`
	SourceJd    *string       `json:"sourceJd"`
	Persona     *string       `json:"persona"`
	Display     *DisplayDTO   `json:"display"`
}

// CategoryDTO 方向下的考察分类，对应 Java SkillCategoryDTO。
// ref 缺失时序列化为 null；shared 为布尔值（Java Boolean.TRUE.equals 兜底语义）。
type CategoryDTO struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Priority string  `json:"priority"`
	Ref      *string `json:"ref"`
	Shared   bool    `json:"shared"`
}

// JdCategoryDTO POST /api/interview/skills/parse-jd 的响应元素，
// 对应 Java InterviewSkillService.CategoryDTO（record）：key/label/priority/ref/shared。
// ref 与 shared 为可空（Java String/Boolean 语义）：LLM 未给出时序列化为 null 原样透传，
// 纠正 ref/shared 发生在创建会话的 buildCustomSkill（票 07），不在本接口。
type JdCategoryDTO struct {
	Key      string  `json:"key"`
	Label    string  `json:"label"`
	Priority string  `json:"priority"`
	Ref      *string `json:"ref"`
	Shared   *bool   `json:"shared"`
}

// DisplayDTO 前端方向卡片展示字段，对应 Java DisplayDTO。
type DisplayDTO struct {
	Icon      string `json:"icon"`
	Gradient  string `json:"gradient"`
	IconBg    string `json:"iconBg"`
	IconColor string `json:"iconColor"`
}

// ===== 场次 DTO（字段名与 Java record 序列化逐字一致，笔记 08 §5）=====

// CreateRequest POST /api/interview/sessions 请求体，对应 Java CreateInterviewRequest。
// 字段一次建全：customCategories/jdText 本票接收但不出题使用（票 07 出题时启用）。
type CreateRequest struct {
	ResumeText       *string       `json:"resumeText"`
	QuestionCount    int           `json:"questionCount"`
	ResumeID         *int64        `json:"resumeId"`
	ForceCreate      *bool         `json:"forceCreate"`
	LLMProvider      *string       `json:"llmProvider"`
	SkillID          *string       `json:"skillId"`
	Difficulty       *string       `json:"difficulty"`
	CustomCategories []CategoryDTO `json:"customCategories"`
	JDText           *string       `json:"jdText"`
	RequestID        string        `json:"requestId"`
}

// SessionDTO 创建/详情响应体，对应 Java InterviewSessionDTO：
// sessionId, resumeText, totalQuestions, currentIndex, questions[], status,
// knowledgeBaseId, interviewCategory。
type SessionDTO struct {
	SessionID         string        `json:"sessionId"`
	ResumeText        string        `json:"resumeText"`
	TotalQuestions    int           `json:"totalQuestions"`
	CurrentIndex      int           `json:"currentIndex"`
	Questions         []QuestionDTO `json:"questions"`
	Status            SessionStatus `json:"status"`
	KnowledgeBaseID   *int64        `json:"knowledgeBaseId"`
	InterviewCategory *string       `json:"interviewCategory"`
}

// SessionListItemDTO GET /api/interview/sessions 列表项，对应 Java SessionListItemDTO。
// 字段一次建全：评估相关列（evaluateStatus/evaluateError/overallScore）票 06 才有真实值；
// sourceType/knowledgeBaseId/interviewCategory 知识库入口（不在本仓范围）先为零值。
type SessionListItemDTO struct {
	SessionID         string     `json:"sessionId"`
	SkillID           *string    `json:"skillId"`
	Difficulty        *string    `json:"difficulty"`
	ResumeID          *int64     `json:"resumeId"`
	TotalQuestions    *int       `json:"totalQuestions"`
	Status            *string    `json:"status"`
	EvaluateStatus    *string    `json:"evaluateStatus"`
	EvaluateError     *string    `json:"evaluateError"`
	OverallScore      *int       `json:"overallScore"`
	SourceType        *string    `json:"sourceType"`
	KnowledgeBaseID   *int64     `json:"knowledgeBaseId"`
	InterviewCategory *string    `json:"interviewCategory"`
	CreatedAt         time.Time  `json:"createdAt"`
	CompletedAt       *time.Time `json:"completedAt"`
}
