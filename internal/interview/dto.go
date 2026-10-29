package interview

import (
	"encoding/json"
	"time"
)

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
// sessionId, resumeText, totalQuestions, currentQuestionIndex, questions[], status,
// knowledgeBaseId, interviewCategory。
type SessionDTO struct {
	SessionID            string        `json:"sessionId"`
	ResumeText           string        `json:"resumeText"`
	TotalQuestions       int           `json:"totalQuestions"`
	CurrentQuestionIndex int           `json:"currentQuestionIndex"`
	Questions            []QuestionDTO `json:"questions"`
	Status               SessionStatus `json:"status"`
	KnowledgeBaseID      *int64        `json:"knowledgeBaseId"`
	InterviewCategory    *string       `json:"interviewCategory"`
}

// AnswerRequest PUT/POST /sessions/{id}/answers 请求体，对齐 Java 松散 Map 取值
// {"questionIndex": int, "answer": string}。questionIndex 为指针：缺失时由 service
// 显式返回 400（Java 缺字段会 NPE → 500 "系统繁忙"，笔记 08 §2.5 建议 Go 显式校验）；
// answer 为指针：缺失/为 null 时 userAnswer 存 NULL（对齐 Java (String) body.get("answer")）。
type AnswerRequest struct {
	QuestionIndex *int    `json:"questionIndex"`
	Answer        *string `json:"answer"`
}

// SubmitAnswerResponse POST /sessions/{id}/answers 响应体，对应 Java record
// SubmitAnswerResponse：hasNextQuestion, nextQuestion（末题为 null）, currentIndex,
// totalQuestions。字段名与 record 组件名逐字一致。
type SubmitAnswerResponse struct {
	HasNextQuestion bool         `json:"hasNextQuestion"`
	NextQuestion    *QuestionDTO `json:"nextQuestion"`
	CurrentIndex    int          `json:"currentIndex"`
	TotalQuestions  int          `json:"totalQuestions"`
}

// CurrentQuestionResponse GET /sessions/{id}/question 响应体，对齐 Java
// getCurrentQuestionResponse 的 Map.of 精确形状（笔记 05 §4 / 08 §2.4）：
//   - 未完成：{"completed": false, "question": {...}}（无 message 键）
//   - 完成：  {"completed": true, "message": "所有问题已回答完毕"}（无 question 键）
//
// 指针 + omitempty 实现"每分支恰好两个键"。
type CurrentQuestionResponse struct {
	Completed bool         `json:"completed"`
	Question  *QuestionDTO `json:"question,omitempty"`
	Message   *string      `json:"message,omitempty"`
}

// SessionListItemDTO GET /api/interview/sessions 列表项，对应 Java SessionListItemDTO。
// 字段一次建全：评估相关列（evaluateStatus/evaluateError/overallScore）票 06 才有真实值；
// sourceType/knowledgeBaseId/interviewCategory 票 05 起由知识库面试创建链路接真值。
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

// InterviewDetailDTO GET /sessions/{id}/details 响应体，对应 Java InterviewDetailDTO record
// （笔记 07）：id/sessionId/totalQuestions/status/evaluateStatus/evaluateError/overallScore/
// sourceType/knowledgeBaseId/overallFeedback/createdAt/completedAt/questions/strengths/
// improvements/referenceAnswers/answers。questions 与 referenceAnswers 为 JSON 列原样透传
// （Java List<Object> 语义：null/非法解析 → null）；时间沿用列表项的 RFC3339 约定
// （笔记 08 §5：Go 侧时间格式与前端约定，全模块一致）。
type InterviewDetailDTO struct {
	ID               int64             `json:"id"`
	SessionID        string            `json:"sessionId"`
	TotalQuestions   *int              `json:"totalQuestions"`
	Status           *string           `json:"status"`
	EvaluateStatus   *string           `json:"evaluateStatus"`
	EvaluateError    *string           `json:"evaluateError"`
	OverallScore     *int              `json:"overallScore"`
	SourceType       *string           `json:"sourceType"`
	KnowledgeBaseID  *int64            `json:"knowledgeBaseId"`
	OverallFeedback  *string           `json:"overallFeedback"`
	CreatedAt        time.Time         `json:"createdAt"`
	CompletedAt      *time.Time        `json:"completedAt"`
	Questions        json.RawMessage   `json:"questions"`
	Strengths        []string          `json:"strengths"`
	Improvements     []string          `json:"improvements"`
	ReferenceAnswers json.RawMessage   `json:"referenceAnswers"`
	Answers          []AnswerDetailDTO `json:"answers"`
}

// AnswerDetailDTO 明细中的单题作答，对应 Java InterviewDetailDTO.AnswerDetailDTO。
// 未答题生成空记录（Java buildAnswerDetailList 逐字）：userAnswer/referenceAnswer/
// keyPoints/answeredAt 为 null，score 取题目 score ?? 0、feedback 取题目 feedback。
type AnswerDetailDTO struct {
	QuestionIndex   *int       `json:"questionIndex"`
	Question        *string    `json:"question"`
	Category        *string    `json:"category"`
	UserAnswer      *string    `json:"userAnswer"`
	Score           *int       `json:"score"`
	Feedback        *string    `json:"feedback"`
	ReferenceAnswer *string    `json:"referenceAnswer"`
	KeyPoints       []string   `json:"keyPoints"`
	AnsweredAt      *time.Time `json:"answeredAt"`
}
