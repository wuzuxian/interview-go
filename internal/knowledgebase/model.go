// Package knowledgebase 知识库管理模块（票 01：上传 → 切分 → 向量入库 → 恢复），
// 接口契约与 Java 版 KnowledgeBaseController / upload / vectorization 链路逐字对齐。
package knowledgebase

import "time"

// VectorStatus 知识库向量化状态（Java VectorStatus 枚举）。
type VectorStatus string

const (
	VectorPending    VectorStatus = "PENDING"
	VectorProcessing VectorStatus = "PROCESSING"
	VectorCompleted  VectorStatus = "COMPLETED"
	VectorFailed     VectorStatus = "FAILED"
)

// KnowledgeBaseEntity 对应表 knowledge_bases（Java schema，应用不做迁移，ADR 0001；
// question_gen_* 列随票 03 出题状态机接入）。
type KnowledgeBaseEntity struct {
	ID                  int64        `gorm:"column:id;primaryKey;autoIncrement"`
	FileHash            string       `gorm:"column:file_hash;size:64;not null;uniqueIndex:idx_kb_hash"`
	Name                string       `gorm:"column:name;size:255;not null"`
	Category            *string      `gorm:"column:category;size:100"`
	OriginalFilename    string       `gorm:"column:original_filename;size:255;not null"`
	FileSize            *int64       `gorm:"column:file_size"`
	ContentType         *string      `gorm:"column:content_type;size:255"`
	StorageKey          *string      `gorm:"column:storage_key;size:500"`
	StorageURL          *string      `gorm:"column:storage_url;size:1000"`
	UploadedAt          time.Time    `gorm:"column:uploaded_at;not null"`
	LastAccessedAt      *time.Time   `gorm:"column:last_accessed_at"`
	AccessCount         *int         `gorm:"column:access_count"`
	QuestionCount       *int         `gorm:"column:question_count"`
	VectorStatus        VectorStatus `gorm:"column:vector_status;size:20"`
	VectorError         *string      `gorm:"column:vector_error;size:500"`
	ChunkCount          *int         `gorm:"column:chunk_count"`
	VectorConfig        *string      `gorm:"column:vector_config;type:text"`
	VectorizedAt        *time.Time   `gorm:"column:vectorized_at"`
	VectorUpdatedAt     *time.Time   `gorm:"column:vector_updated_at"`
	VectorRecoveryCount int          `gorm:"column:vector_recovery_count;not null;default:0"`
	VectorAttemptID     *string      `gorm:"column:vector_attempt_id;size:36"`
}

func (KnowledgeBaseEntity) TableName() string { return "knowledge_bases" }
