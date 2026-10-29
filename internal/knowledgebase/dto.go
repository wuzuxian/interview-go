// 上传响应与列表 DTO，字段名与 Java 版上传 Map / KnowledgeBaseListItemDTO 逐字对齐。
package knowledgebase

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
