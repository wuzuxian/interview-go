// RAG 会话实体（Java RagChatSessionEntity / RagChatMessageEntity），
// 对应表 rag_chat_sessions / rag_chat_messages / rag_session_knowledge_bases
// （Java schema，应用不做迁移，ADR 0001）。
package knowledgebase

import "time"

// RagChatStatus 会话状态（Java SessionStatus 枚举）。
type RagChatStatus string

const (
	RagChatActive    RagChatStatus = "ACTIVE"
	RagChatArchived  RagChatStatus = "ARCHIVED"
	RagChatUser      string        = "USER"
	RagChatAssistant string        = "ASSISTANT"
)

// RagChatSessionEntity 对应表 rag_chat_sessions。
type RagChatSessionEntity struct {
	ID           int64      `gorm:"column:id;primaryKey;autoIncrement"`
	Title        string     `gorm:"column:title;size:255;not null"`
	Status       string     `gorm:"column:status;size:20"`
	MessageCount *int       `gorm:"column:message_count"`
	IsPinned     *bool      `gorm:"column:is_pinned"`
	CreatedAt    time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt    *time.Time `gorm:"column:updated_at"`
}

func (RagChatSessionEntity) TableName() string { return "rag_chat_sessions" }

// RagChatMessageEntity 对应表 rag_chat_messages。
type RagChatMessageEntity struct {
	ID           int64      `gorm:"column:id;primaryKey;autoIncrement"`
	SessionID    int64      `gorm:"column:session_id;not null"`
	Type         string     `gorm:"column:type;size:20;not null"`
	Content      string     `gorm:"column:content;type:text;not null"`
	MessageOrder int        `gorm:"column:message_order;not null"`
	Completed    *bool      `gorm:"column:completed"`
	CreatedAt    time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt    *time.Time `gorm:"column:updated_at"`
}

func (RagChatMessageEntity) TableName() string { return "rag_chat_messages" }
