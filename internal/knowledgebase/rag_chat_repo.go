// RAG 会话数据访问（Java RagChatSessionRepository / RagChatMessageRepository
// 派生查询语义对齐）；列表排序、置顶翻转、流式消息两段写入等 SQL 由票 05
// smoke 真库兜底，单测以内存 fake 测编排决策。
package knowledgebase

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/wuzuxian/interview/internal/apperr"
)

// ErrRagSessionNotFound RAG 会话不存在（Java NOT_FOUND + 「会话不存在」）。
var ErrRagSessionNotFound = apperr.New(apperr.CodeNotFound, "会话不存在")

// ErrRagMessageNotFound RAG 消息不存在（Java NOT_FOUND + 「消息不存在」）。
var ErrRagMessageNotFound = apperr.New(apperr.CodeNotFound, "消息不存在")

// RagChatSessionRow 会话行 + 关联知识库 ID（列表名称/详情实体按需二次加载，
// Java findByIdWithKnowledgeBases 两段查询避免笛卡尔积的对应物）。
type RagChatSessionRow struct {
	Session RagChatSessionEntity
	KBIDs   []int64
}

// ChatStore RAG 会话数据面接缝（生产实现 *ChatRepo，测试注入内存 fake）。
type ChatStore interface {
	InsertSession(ctx context.Context, session *RagChatSessionEntity, kbIDs []int64) error
	FindSessionRow(ctx context.Context, id int64) (*RagChatSessionRow, error)
	FindSessionRowsOrderByPinnedUpdatedAtDesc(ctx context.Context) ([]RagChatSessionRow, error)
	FindKBNamesBySessionIDs(ctx context.Context, sessionIDs []int64) (map[int64][]string, error)
	UpdateSessionTitle(ctx context.Context, id int64, title string, now time.Time) error
	ToggleSessionPin(ctx context.Context, id int64, now time.Time) (bool, error)
	UpdateSessionKnowledgeBases(ctx context.Context, id int64, kbIDs []int64, now time.Time) error
	DeleteSession(ctx context.Context, id int64) error

	// InsertStreamMessages 同步落库用户消息 + AI 占位（事务），返回 AI 消息 ID
	// （Java prepareStreamMessage）。
	InsertStreamMessages(ctx context.Context, sessionID int64, question string, startOrder int, now time.Time) (int64, error)
	FindMessagesBySessionID(ctx context.Context, sessionID int64) ([]RagChatMessageEntity, error)
	// FindRecentCompletedMessages 最近 N 条完成消息（message_order DESC，
	// Java findRecentCompletedBySessionId）。
	FindRecentCompletedMessages(ctx context.Context, sessionID int64, limit int) ([]RagChatMessageEntity, error)
	// CompleteMessage 流末回写全文并置完成（Java completeStreamMessage）。
	CompleteMessage(ctx context.Context, messageID int64, content string, now time.Time) error
}

// ChatRepo GORM 实现。
type ChatRepo struct {
	db     *gorm.DB
	logger *slog.Logger
}

// NewChatRepo 创建 RAG 会话仓储。
func NewChatRepo(db *gorm.DB, logger *slog.Logger) *ChatRepo {
	return &ChatRepo{db: db, logger: logger}
}

// InsertSession 事务内新建会话 + 关联行（Java save：@PrePersist 填 created_at/
// updated_at；多对多 Set 去重后落关联表）。
func (r *ChatRepo) InsertSession(ctx context.Context, session *RagChatSessionEntity, kbIDs []int64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(session).Error; err != nil {
			return err
		}
		seen := make(map[int64]bool, len(kbIDs))
		for _, kbID := range kbIDs {
			if seen[kbID] {
				continue
			}
			seen[kbID] = true
			if err := tx.Exec("INSERT INTO rag_session_knowledge_bases (session_id, knowledge_base_id) VALUES (?, ?)",
				session.ID, kbID).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// FindSessionRow 会话 + 关联知识库 ID（Java findByIdWithKnowledgeBases；
// 不存在 → ErrRagSessionNotFound）。
func (r *ChatRepo) FindSessionRow(ctx context.Context, id int64) (*RagChatSessionRow, error) {
	var session RagChatSessionEntity
	err := r.db.WithContext(ctx).First(&session, id).Error
	if err == gorm.ErrRecordNotFound {
		return nil, ErrRagSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	kbIDs, err := r.sessionKBIDs(r.db.WithContext(ctx), id)
	if err != nil {
		return nil, err
	}
	return &RagChatSessionRow{Session: session, KBIDs: kbIDs}, nil
}

// sessionKBIDs 关联知识库 ID（按 knowledge_base_id 升序输出，Java HashSet 顺序
// 不稳定，此处取稳定序）。
func (r *ChatRepo) sessionKBIDs(tx *gorm.DB, sessionID int64) ([]int64, error) {
	var ids []int64
	err := tx.Raw("SELECT knowledge_base_id FROM rag_session_knowledge_bases WHERE session_id = ? ORDER BY knowledge_base_id",
		sessionID).Scan(&ids).Error
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []int64{}
	}
	return ids, nil
}

// FindSessionRowsOrderByPinnedUpdatedAtDesc 会话列表（Java
// findAllOrderByPinnedAndUpdatedAtDesc：ORDER BY is_pinned DESC, updated_at DESC，
// Postgres DESC 下 NULL 靠前与 Java-on-Postgres 行为一致）。关联知识库 ID 一次
// IN 查询取回后内存分组（输出序仍按 knowledge_base_id 升序），消除逐会话 N+1。
func (r *ChatRepo) FindSessionRowsOrderByPinnedUpdatedAtDesc(ctx context.Context) ([]RagChatSessionRow, error) {
	var sessions []RagChatSessionEntity
	if err := r.db.WithContext(ctx).
		Raw("SELECT * FROM rag_chat_sessions ORDER BY is_pinned DESC, updated_at DESC").
		Scan(&sessions).Error; err != nil {
		return nil, err
	}
	rows := make([]RagChatSessionRow, 0, len(sessions))
	if len(sessions) == 0 {
		return rows, nil
	}
	sessionIDs := make([]int64, 0, len(sessions))
	for _, s := range sessions {
		sessionIDs = append(sessionIDs, s.ID)
	}
	var assoc []struct {
		SessionID int64 `gorm:"column:session_id"`
		KBID      int64 `gorm:"column:knowledge_base_id"`
	}
	if err := r.db.WithContext(ctx).Raw(
		"SELECT session_id, knowledge_base_id FROM rag_session_knowledge_bases "+
			"WHERE session_id IN ? ORDER BY session_id, knowledge_base_id",
		sessionIDs).Scan(&assoc).Error; err != nil {
		return nil, err
	}
	kbIDsBySession := make(map[int64][]int64, len(sessions))
	for _, a := range assoc {
		kbIDsBySession[a.SessionID] = append(kbIDsBySession[a.SessionID], a.KBID)
	}
	for _, s := range sessions {
		kbIDs := kbIDsBySession[s.ID]
		if kbIDs == nil {
			kbIDs = []int64{}
		}
		rows = append(rows, RagChatSessionRow{Session: s, KBIDs: kbIDs})
	}
	return rows, nil
}

// FindKBNamesBySessionIDs 批量取会话关联的知识库名称（列表项 knowledgeBaseNames；
// 名称按 knowledge_base_id 升序）。
func (r *ChatRepo) FindKBNamesBySessionIDs(ctx context.Context, sessionIDs []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		SessionID int64  `gorm:"column:session_id"`
		Name      string `gorm:"column:name"`
	}
	err := r.db.WithContext(ctx).Raw(
		"SELECT rskb.session_id AS session_id, kb.name AS name "+
			"FROM rag_session_knowledge_bases rskb "+
			"JOIN knowledge_bases kb ON kb.id = rskb.knowledge_base_id "+
			"WHERE rskb.session_id IN ? ORDER BY rskb.session_id, rskb.knowledge_base_id",
		sessionIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.SessionID] = append(out[row.SessionID], row.Name)
	}
	return out, nil
}

// UpdateSessionTitle 改标题并推进 updated_at（Java updateSessionTitle + @PreUpdate）。
func (r *ChatRepo) UpdateSessionTitle(ctx context.Context, id int64, title string, now time.Time) error {
	res := r.db.WithContext(ctx).Model(&RagChatSessionEntity{}).Where("id = ?", id).
		Updates(map[string]any{"title": title, "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrRagSessionNotFound
	}
	return nil
}

// ToggleSessionPin 置顶翻转：NULL 视为 false（Java togglePin 兼容旧数据），
// 返回翻转后的状态。
func (r *ChatRepo) ToggleSessionPin(ctx context.Context, id int64, now time.Time) (bool, error) {
	var session RagChatSessionEntity
	err := r.db.WithContext(ctx).First(&session, id).Error
	if err == gorm.ErrRecordNotFound {
		return false, ErrRagSessionNotFound
	}
	if err != nil {
		return false, err
	}
	current := session.IsPinned != nil && *session.IsPinned
	next := !current
	res := r.db.WithContext(ctx).Model(&RagChatSessionEntity{}).Where("id = ?", id).
		Updates(map[string]any{"is_pinned": next, "updated_at": now})
	if res.Error != nil {
		return false, res.Error
	}
	return next, nil
}

// UpdateSessionKnowledgeBases 换绑知识库：Java updateSessionKnowledgeBases 用
// findAllById 取回存在的知识库（缺失项静默丢弃），事务内替换关联行并推进 updated_at。
func (r *ChatRepo) UpdateSessionKnowledgeBases(ctx context.Context, id int64, kbIDs []int64, now time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session RagChatSessionEntity
		err := tx.First(&session, id).Error
		if err == gorm.ErrRecordNotFound {
			return ErrRagSessionNotFound
		}
		if err != nil {
			return err
		}
		var existing []int64
		if len(kbIDs) > 0 {
			if err := tx.Raw("SELECT id FROM knowledge_bases WHERE id IN ?", kbIDs).Scan(&existing).Error; err != nil {
				return err
			}
		}
		if err := tx.Exec("DELETE FROM rag_session_knowledge_bases WHERE session_id = ?", id).Error; err != nil {
			return err
		}
		seen := make(map[int64]bool, len(existing))
		for _, kbID := range existing {
			if seen[kbID] {
				continue
			}
			seen[kbID] = true
			if err := tx.Exec("INSERT INTO rag_session_knowledge_bases (session_id, knowledge_base_id) VALUES (?, ?)",
				id, kbID).Error; err != nil {
				return err
			}
		}
		return tx.Model(&RagChatSessionEntity{}).Where("id = ?", id).
			Update("updated_at", now).Error
	})
}

// DeleteSession 事务内删消息 → 摘关联 → 删会话（Java deleteById 的级联语义；
// 不存在 → ErrRagSessionNotFound）。
func (r *ChatRepo) DeleteSession(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session RagChatSessionEntity
		err := tx.First(&session, id).Error
		if err == gorm.ErrRecordNotFound {
			return ErrRagSessionNotFound
		}
		if err != nil {
			return err
		}
		if err := tx.Where("session_id = ?", id).Delete(&RagChatMessageEntity{}).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM rag_session_knowledge_bases WHERE session_id = ?", id).Error; err != nil {
			return err
		}
		return tx.Delete(&RagChatSessionEntity{}, id).Error
	})
}

// InsertStreamMessages 同步落库用户消息（completed）+ AI 占位（未完成），
// 会话 message_count 推进 +2 并推进 updated_at，返回 AI 消息 ID
// （Java prepareStreamMessage 逐步对齐）。
func (r *ChatRepo) InsertStreamMessages(ctx context.Context, sessionID int64, question string, startOrder int, now time.Time) (int64, error) {
	var assistantID int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		completed := true
		userMessage := RagChatMessageEntity{
			SessionID: sessionID, Type: RagChatUser, Content: question,
			MessageOrder: startOrder, Completed: &completed, CreatedAt: now, UpdatedAt: &now,
		}
		if err := tx.Create(&userMessage).Error; err != nil {
			return err
		}
		notCompleted := false
		assistant := RagChatMessageEntity{
			SessionID: sessionID, Type: RagChatAssistant, Content: "",
			MessageOrder: startOrder + 1, Completed: &notCompleted, CreatedAt: now, UpdatedAt: &now,
		}
		if err := tx.Create(&assistant).Error; err != nil {
			return err
		}
		assistantID = assistant.ID
		nextOrder := startOrder + 2
		return tx.Model(&RagChatSessionEntity{}).Where("id = ?", sessionID).
			Updates(map[string]any{"message_count": nextOrder, "updated_at": now}).Error
	})
	if err != nil {
		return 0, err
	}
	return assistantID, nil
}

// FindMessagesBySessionID 会话全部消息（按 message_order 升序，Java
// findBySessionIdOrderByMessageOrderAsc）。
func (r *ChatRepo) FindMessagesBySessionID(ctx context.Context, sessionID int64) ([]RagChatMessageEntity, error) {
	var messages []RagChatMessageEntity
	err := r.db.WithContext(ctx).
		Where("session_id = ?", sessionID).Order("message_order ASC").Find(&messages).Error
	if err != nil {
		return nil, err
	}
	if messages == nil {
		messages = []RagChatMessageEntity{}
	}
	return messages, nil
}

// FindRecentCompletedMessages 最近 limit 条完成消息（message_order DESC，
// Java findRecentCompletedBySessionId）。
func (r *ChatRepo) FindRecentCompletedMessages(ctx context.Context, sessionID int64, limit int) ([]RagChatMessageEntity, error) {
	var messages []RagChatMessageEntity
	err := r.db.WithContext(ctx).
		Where("session_id = ? AND completed = ?", sessionID, true).
		Order("message_order DESC").Limit(limit).Find(&messages).Error
	return messages, err
}

// CompleteMessage 回写全文并置完成（Java completeStreamMessage；不存在 →
// ErrRagMessageNotFound）。
func (r *ChatRepo) CompleteMessage(ctx context.Context, messageID int64, content string, now time.Time) error {
	res := r.db.WithContext(ctx).Model(&RagChatMessageEntity{}).Where("id = ?", messageID).
		Updates(map[string]any{"content": content, "completed": true, "updated_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrRagMessageNotFound
	}
	return nil
}
