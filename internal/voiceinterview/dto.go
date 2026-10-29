package voiceinterview

import "time"

// REST 请求/响应 DTO。字段名与 Java 版 voiceinterview dto/ 包（Lombok @Data 驼峰
// 序列化）逐字一致，键集由 contract_test.go 黄金清单锁死。
// Java 侧包装类型（Long/Integer/Boolean）可空语义在 Go 用指针表达：null 原样输出。

// CreateSessionRequest POST /api/voice-interview/sessions 请求体，对应 Java
// dto/CreateSessionRequest。默认值（@Builder.Default）由服务层补齐：
// introEnabled=false、tech/project/hrEnabled=true、plannedDuration=30；
// roleType 为向后兼容入参，服务层落库恒取 skillId。
type CreateSessionRequest struct {
	RoleType        *string `json:"roleType"`
	SkillID         *string `json:"skillId"`
	Difficulty      *string `json:"difficulty"`
	CustomJdText    *string `json:"customJdText"`
	ResumeID        *int64  `json:"resumeId"`
	IntroEnabled    *bool   `json:"introEnabled"`
	TechEnabled     *bool   `json:"techEnabled"`
	ProjectEnabled  *bool   `json:"projectEnabled"`
	HrEnabled       *bool   `json:"hrEnabled"`
	PlannedDuration *int    `json:"plannedDuration"`
	LLMProvider     *string `json:"llmProvider"`
}

// SessionResponseDTO 创建/查询/恢复的响应体，对应 Java dto/SessionResponseDTO。
// webSocketUrl 为相对路径 /ws/voice-interview/{id}（Java Service L599-609），
// 客户端按部署地址自选 ws/wss。
type SessionResponseDTO struct {
	SessionID       int64      `json:"sessionId"`
	RoleType        string     `json:"roleType"`
	CurrentPhase    string     `json:"currentPhase"`
	Status          string     `json:"status"`
	StartTime       *time.Time `json:"startTime"`
	PlannedDuration *int       `json:"plannedDuration"`
	WebSocketURL    string     `json:"webSocketUrl"`
}

// SessionMetaDTO 列表项，对应 Java dto/SessionMetaDTO：十字段一次建全
// （messageCount 为非 SUMMARY 消息计数）。
type SessionMetaDTO struct {
	SessionID      int64      `json:"sessionId"`
	RoleType       string     `json:"roleType"`
	Status         string     `json:"status"`
	CurrentPhase   string     `json:"currentPhase"`
	CreatedAt      *time.Time `json:"createdAt"`
	UpdatedAt      *time.Time `json:"updatedAt"`
	ActualDuration *int       `json:"actualDuration"`
	MessageCount   int64      `json:"messageCount"`
	EvaluateStatus *string    `json:"evaluateStatus"`
	EvaluateError  *string    `json:"evaluateError"`
}

// VoiceInterviewMessageDTO 消息项，对应 Java dto/VoiceInterviewMessageDTO：八字段。
// phase 为枚举名或 null（Java msg.getPhase() != null ? name() : null）。
type VoiceInterviewMessageDTO struct {
	ID                 int64      `json:"id"`
	SessionID          int64      `json:"sessionId"`
	MessageType        string     `json:"messageType"`
	Phase              *string    `json:"phase"`
	UserRecognizedText *string    `json:"userRecognizedText"`
	AiGeneratedText    *string    `json:"aiGeneratedText"`
	Timestamp          *time.Time `json:"timestamp"`
	SequenceNum        *int       `json:"sequenceNum"`
}

// VoiceEvaluationStatusDTO GET/POST /sessions/{id}/evaluation 响应体，对应 Java
// dto/VoiceEvaluationStatusDTO：评估状态 + 失败原因 + 状态时间 + 完成时的完整报告。
// Java builder 未赋值的字段序列化为 null，Go 用指针 nil 表达（包装类型可空语义）。
type VoiceEvaluationStatusDTO struct {
	EvaluateStatus          *string                   `json:"evaluateStatus"`
	EvaluateError           *string                   `json:"evaluateError"`
	EvaluateStatusUpdatedAt *time.Time                `json:"evaluateStatusUpdatedAt"`
	Evaluation              *VoiceEvaluationDetailDTO `json:"evaluation"`
}

// VoiceEvaluationDetailDTO 评估报告明细，对应 Java dto/VoiceEvaluationDetailDTO
// （注释原文：aligned with text-based interview InterviewDetailDTO，前端复用同一报告面板）。
// 字段名与文字面试明细逐字对齐；sessionId 为语音场次自增主键（Java Long）。
type VoiceEvaluationDetailDTO struct {
	SessionID       int64                  `json:"sessionId"`
	TotalQuestions  int                    `json:"totalQuestions"`
	OverallScore    *int                   `json:"overallScore"`
	OverallFeedback *string                `json:"overallFeedback"`
	Strengths       []string               `json:"strengths"`
	Improvements    []string               `json:"improvements"`
	Answers         []VoiceAnswerDetailDTO `json:"answers"`
}

// VoiceAnswerDetailDTO 明细中的单题，对应 Java VoiceEvaluationDetailDTO.AnswerDetail：
// questionIndex/score 为 Java 原生 int 恒输出；userAnswer 未答为 null；
// referenceAnswer/keyPoints 仅在参考答案列表命中该题时非空（Java ref != null 判定）。
type VoiceAnswerDetailDTO struct {
	QuestionIndex   int      `json:"questionIndex"`
	Question        string   `json:"question"`
	Category        string   `json:"category"`
	UserAnswer      *string  `json:"userAnswer"`
	Score           int      `json:"score"`
	Feedback        string   `json:"feedback"`
	ReferenceAnswer *string  `json:"referenceAnswer"`
	KeyPoints       []string `json:"keyPoints"`
}
