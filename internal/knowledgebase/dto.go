// 上传响应与列表 DTO，字段名与 Java 版上传 Map / KnowledgeBaseListItemDTO 逐字对齐。
package knowledgebase

import "time"

// UploadKnowledgeBaseDTO 上传响应 knowledgeBase 字段（Java Map.of 键序）。
type UploadKnowledgeBaseDTO struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Category     string `json:"category"`
	FileSize     int64  `json:"fileSize"`
	VectorStatus string `json:"vectorStatus"`
}

// UploadStorageDTO 上传响应 storage 字段。
type UploadStorageDTO struct {
	FileKey string `json:"fileKey"`
	FileURL string `json:"fileUrl"`
}

// UploadResponse POST /api/knowledgebase/upload 响应体（Result.data）。
type UploadResponse struct {
	KnowledgeBase   UploadKnowledgeBaseDTO `json:"knowledgeBase"`
	Storage         UploadStorageDTO       `json:"storage"`
	EnqueueAccepted bool                   `json:"enqueueAccepted"`
	Message         string                 `json:"message"`
	Duplicate       bool                   `json:"duplicate"`
}

// KnowledgeBaseListItemDTO 列表/详情 DTO（Java KnowledgeBaseListItemDTO record，
// MapStruct 同名映射；可空列输出 JSON null 与 Java Jackson 一致）。
type KnowledgeBaseListItemDTO struct {
	ID                int64      `json:"id"`
	Name              string     `json:"name"`
	Category          *string    `json:"category"`
	OriginalFilename  string     `json:"originalFilename"`
	FileSize          *int64     `json:"fileSize"`
	ContentType       *string    `json:"contentType"`
	UploadedAt        time.Time  `json:"uploadedAt"`
	LastAccessedAt    *time.Time `json:"lastAccessedAt"`
	AccessCount       *int       `json:"accessCount"`
	QuestionCount     *int       `json:"questionCount"`
	VectorStatus      string     `json:"vectorStatus"`
	VectorError       *string    `json:"vectorError"`
	ChunkCount        *int       `json:"chunkCount"`
	QuestionGenStatus string     `json:"questionGenStatus"`
	QuestionGenError  *string    `json:"questionGenError"`
}

// KnowledgeBaseStatsDTO GET /api/knowledgebase/stats 响应
// （Java KnowledgeBaseStatsDTO record；totalQuestionCount 取 RAG 会话 USER 消息数）。
type KnowledgeBaseStatsDTO struct {
	TotalCount         int64 `json:"totalCount"`
	TotalQuestionCount int64 `json:"totalQuestionCount"`
	TotalAccessCount   int64 `json:"totalAccessCount"`
	CompletedCount     int64 `json:"completedCount"`
	ProcessingCount    int64 `json:"processingCount"`
}
