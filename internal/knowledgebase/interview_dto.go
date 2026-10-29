// 知识库面试 DTO（票 05）：字段名与 Java CreateKnowledgeBaseInterviewRequest /
// KnowledgeBaseInterviewCapacityResponse（含 CategoryOption/FollowUpOption）
// record 序列化逐字一致；创建响应复用 interview.SessionDTO（Java 同样跨模块复用
// InterviewSessionDTO）。
package knowledgebase

// CreateInterviewRequest POST /api/knowledgebase-interviews/sessions 请求体
// （Java CreateKnowledgeBaseInterviewRequest record）：knowledgeBaseId @NotNull、
// mainQuestionCount 1-20、followUpCount 0-5，category/difficulty/llmProvider 自由文本。
// 计数指针：缺失时按 Java 原生 int 缺省 0 参与校验（「主问题数量最少1题」）。
type CreateInterviewRequest struct {
	KnowledgeBaseID   *int64  `json:"knowledgeBaseId"`
	Category          *string `json:"category"`
	Difficulty        *string `json:"difficulty"`
	MainQuestionCount *int    `json:"mainQuestionCount"`
	FollowUpCount     *int    `json:"followUpCount"`
	LLMProvider       *string `json:"llmProvider"`
}

// KnowledgeBaseInterviewCapacityResponse 知识库面试在指定方向、难度和主问题数下的
// 可用容量（Java record；category 可空 → JSON null）。
type KnowledgeBaseInterviewCapacityResponse struct {
	KnowledgeBaseID   int64                    `json:"knowledgeBaseId"`
	Category          *string                  `json:"category"`
	Difficulty        string                   `json:"difficulty"`
	MainQuestionCount int                      `json:"mainQuestionCount"`
	Categories        []CapacityCategoryOption `json:"categories"`
	FollowUpOptions   []CapacityFollowUpOption `json:"followUpOptions"`
}

// CapacityCategoryOption 各分类可用题数（Java CategoryOption record）。
type CapacityCategoryOption struct {
	Category               string `json:"category"`
	AvailableQuestionCount int    `json:"availableQuestionCount"`
}

// CapacityFollowUpOption 各追问档位可用量与可选性（Java FollowUpOption record）。
type CapacityFollowUpOption struct {
	FollowUpCount          int  `json:"followUpCount"`
	AvailableQuestionCount int  `json:"availableQuestionCount"`
	Selectable             bool `json:"selectable"`
}
