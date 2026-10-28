// Package resume 简历管理模块，接口契约与 Java 版 ResumeController 一致。
package resume

import "time"

// AsyncTaskStatus 异步任务状态。
type AsyncTaskStatus string

const (
	StatusPending    AsyncTaskStatus = "PENDING"
	StatusProcessing AsyncTaskStatus = "PROCESSING"
	StatusCompleted  AsyncTaskStatus = "COMPLETED"
	StatusFailed     AsyncTaskStatus = "FAILED"
)

// ResumeEntity 对应表 resumes。
type ResumeEntity struct {
	ID                   int64           `gorm:"column:id;primaryKey;autoIncrement"`
	FileHash             string          `gorm:"column:file_hash;size:64;not null;uniqueIndex:idx_resume_hash"`
	OriginalFilename     string          `gorm:"column:original_filename;size:255;not null"`
	FileSize             *int64          `gorm:"column:file_size"`
	ContentType          *string         `gorm:"column:content_type;size:255"`
	StorageKey           *string         `gorm:"column:storage_key;size:500"`
	StorageURL           *string         `gorm:"column:storage_url;size:1000"`
	ResumeText           *string         `gorm:"column:resume_text;type:text"`
	UploadedAt           time.Time       `gorm:"column:uploaded_at;not null"`
	LastAccessedAt       *time.Time      `gorm:"column:last_accessed_at"`
	AccessCount          *int            `gorm:"column:access_count"`
	AnalyzeStatus        AsyncTaskStatus `gorm:"column:analyze_status;size:20"`
	AnalyzeUpdatedAt     *time.Time      `gorm:"column:analyze_updated_at"`
	AnalyzeRecoveryCount int             `gorm:"column:analyze_recovery_count;not null"`
	AnalyzeAttemptID     *string         `gorm:"column:analyze_attempt_id;size:36"`
	AnalyzeError         *string         `gorm:"column:analyze_error;size:500"`
}

func (ResumeEntity) TableName() string { return "resumes" }

// AnalysisEntity 对应表 resume_analyses。
type AnalysisEntity struct {
	ID              int64     `gorm:"column:id;primaryKey;autoIncrement"`
	ResumeID        int64     `gorm:"column:resume_id;not null"`
	OverallScore    *int      `gorm:"column:overall_score"`
	ContentScore    *int      `gorm:"column:content_score"`
	StructureScore  *int      `gorm:"column:structure_score"`
	SkillMatchScore *int      `gorm:"column:skill_match_score"`
	ExpressionScore *int      `gorm:"column:expression_score"`
	ProjectScore    *int      `gorm:"column:project_score"`
	Summary         *string   `gorm:"column:summary;type:text"`
	StrengthsJSON   *string   `gorm:"column:strengths_json;type:text"`
	SuggestionsJSON *string   `gorm:"column:suggestions_json;type:text"`
	AnalyzedAt      time.Time `gorm:"column:analyzed_at;not null"`
}

func (AnalysisEntity) TableName() string { return "resume_analyses" }

// ScoreDetail 各维度评分。
type ScoreDetail struct {
	ContentScore    int `json:"contentScore"`
	StructureScore  int `json:"structureScore"`
	SkillMatchScore int `json:"skillMatchScore"`
	ExpressionScore int `json:"expressionScore"`
	ProjectScore    int `json:"projectScore"`
}

// Suggestion 改进建议。
type Suggestion struct {
	Category       string `json:"category"`
	Priority       string `json:"priority"`
	Issue          string `json:"issue"`
	Recommendation string `json:"recommendation"`
}

// AnalysisResponse 上传/查询返回的分析结果。
type AnalysisResponse struct {
	OverallScore int          `json:"overallScore"`
	ScoreDetail  ScoreDetail  `json:"scoreDetail"`
	Summary      string       `json:"summary"`
	Strengths    []string     `json:"strengths"`
	Suggestions  []Suggestion `json:"suggestions"`
	OriginalText string       `json:"originalText"`
}

// UploadResumeInfo 上传响应中的 resume 字段（异步模式下返回任务状态）。
type UploadResumeInfo struct {
	ID            int64           `json:"id"`
	Filename      string          `json:"filename"`
	AnalyzeStatus AsyncTaskStatus `json:"analyzeStatus"`
}

// StorageInfo 上传响应中的 storage 字段。
type StorageInfo struct {
	FileKey  string `json:"fileKey"`
	FileURL  string `json:"fileUrl"`
	ResumeID int64  `json:"resumeId"`
}

// UploadResponse POST /api/resumes/upload 响应体。
type UploadResponse struct {
	Analysis        *AnalysisResponse `json:"analysis,omitempty"`
	Resume          *UploadResumeInfo `json:"resume,omitempty"`
	Storage         StorageInfo       `json:"storage"`
	EnqueueAccepted bool              `json:"enqueueAccepted"`
	Duplicate       bool              `json:"duplicate"`
	Message         string            `json:"message"`
}

// ResumeListItemDTO GET /api/resumes 列表项。
type ResumeListItemDTO struct {
	ID             int64           `json:"id"`
	Filename       string          `json:"filename"`
	FileSize       *int64          `json:"fileSize"`
	UploadedAt     time.Time       `json:"uploadedAt"`
	AccessCount    *int            `json:"accessCount"`
	LatestScore    *int            `json:"latestScore"`
	LastAnalyzedAt *time.Time      `json:"lastAnalyzedAt"`
	InterviewCount int             `json:"interviewCount"`
	AnalyzeStatus  AsyncTaskStatus `json:"analyzeStatus"`
	AnalyzeError   *string         `json:"analyzeError"`
}

// ResumeStats GET /api/resumes/statistics 统计（前端 HistoryList 使用）。
type ResumeStats struct {
	TotalCount          int64 `json:"totalCount"`
	TotalInterviewCount int64 `json:"totalInterviewCount"`
	TotalAccessCount    int64 `json:"totalAccessCount"`
}

// AnalysisHistoryDTO 简历详情中的分析历史项。
type AnalysisHistoryDTO struct {
	ID              int64        `json:"id"`
	OverallScore    *int         `json:"overallScore"`
	ContentScore    *int         `json:"contentScore"`
	StructureScore  *int         `json:"structureScore"`
	SkillMatchScore *int         `json:"skillMatchScore"`
	ExpressionScore *int         `json:"expressionScore"`
	ProjectScore    *int         `json:"projectScore"`
	Summary         *string      `json:"summary"`
	AnalyzedAt      time.Time    `json:"analyzedAt"`
	Strengths       []string     `json:"strengths"`
	Suggestions     []Suggestion `json:"suggestions"`
}

// InterviewHistoryItemDTO 简历详情中的面试记录项，字段与 Java 版一致。
type InterviewHistoryItemDTO struct {
	ID             int64      `json:"id"`
	SessionID      string     `json:"sessionId"`
	TotalQuestions *int       `json:"totalQuestions"`
	Status         *string    `json:"status"`
	EvaluateStatus *string    `json:"evaluateStatus"`
	EvaluateError  *string    `json:"evaluateError"`
	OverallScore   *int       `json:"overallScore"`
	CreatedAt      time.Time  `json:"createdAt"`
	CompletedAt    *time.Time `json:"completedAt"`
}

// ResumeDetailDTO GET /api/resumes/{id}/detail 响应体。
type ResumeDetailDTO struct {
	ID            int64                     `json:"id"`
	Filename      string                    `json:"filename"`
	FileSize      *int64                    `json:"fileSize"`
	ContentType   *string                   `json:"contentType"`
	StorageURL    *string                   `json:"storageUrl"`
	UploadedAt    time.Time                 `json:"uploadedAt"`
	AccessCount   *int                      `json:"accessCount"`
	ResumeText    *string                   `json:"resumeText"`
	AnalyzeStatus AsyncTaskStatus           `json:"analyzeStatus"`
	AnalyzeError  *string                   `json:"analyzeError"`
	Analyses      []AnalysisHistoryDTO      `json:"analyses"`
	Interviews    []InterviewHistoryItemDTO `json:"interviews"`
}

// InterviewSession 面试会话（仅用于详情页关联展示与级联删除）。
type InterviewSession struct {
	ID             int64      `gorm:"column:id;primaryKey;autoIncrement"`
	SessionID      string     `gorm:"column:session_id;size:36;not null"`
	ResumeID       *int64     `gorm:"column:resume_id"`
	TotalQuestions *int       `gorm:"column:total_questions"`
	Status         *string    `gorm:"column:status;size:20"`
	EvaluateStatus *string    `gorm:"column:evaluate_status;size:20"`
	EvaluateError  *string    `gorm:"column:evaluate_error;size:500"`
	OverallScore   *int       `gorm:"column:overall_score"`
	CreatedAt      time.Time  `gorm:"column:created_at;not null"`
	CompletedAt    *time.Time `gorm:"column:completed_at"`
}

func (InterviewSession) TableName() string { return "interview_sessions" }
