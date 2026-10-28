// Repo 语音面试三张表的数据访问，模式对齐 interview 模块 repo：WithContext 贯穿、
// 事务内联（级联删除）、查询语义按 Java Repository 派生方法逐字对齐
// （findByUserIdOrderByUpdatedAtDesc / countBySessionIdAndMessageTypeNot /
// deleteBySessionId 等）。gorm.ErrRecordNotFound 归一为 ErrSessionNotFound，
// 由 service 按各端点的 Java 异常语义映射业务码。
package voiceinterview

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// ErrSessionNotFound 场次不存在（FindSessionByID / DeleteSessionCascade 返回）。
var ErrSessionNotFound = errors.New("voiceinterview: 会话不存在")

// Repo 语音面试数据访问。
type Repo struct {
	db *gorm.DB
}

// NewRepo 创建仓库。
func NewRepo(db *gorm.DB) *Repo { return &Repo{db: db} }

// CreateSession 插入会话；created_at/updated_at/start_time 为空时补当前时间
// （对齐 Java @PrePersist：三者都在首次保存时写入实体）。
func (r *Repo) CreateSession(ctx context.Context, e *SessionEntity) error {
	now := time.Now()
	if e.CreatedAt == nil {
		e.CreatedAt = &now
	}
	if e.UpdatedAt == nil {
		e.UpdatedAt = &now
	}
	if e.StartTime == nil {
		e.StartTime = &now
	}
	return r.db.WithContext(ctx).Create(e).Error
}

// FindSessionByID 按内部自增主键查询；不存在返回 ErrSessionNotFound。
func (r *Repo) FindSessionByID(ctx context.Context, sessionID int64) (*SessionEntity, error) {
	var e SessionEntity
	err := r.db.WithContext(ctx).First(&e, sessionID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// UpdatePause 暂停落库：status=PAUSED + paused_at（updated_at 由 GORM 按字段名
// 自动刷新，对齐 Java @PreUpdate）。
func (r *Repo) UpdatePause(ctx context.Context, sessionID int64, pausedAt time.Time) error {
	return r.db.WithContext(ctx).Model(&SessionEntity{}).
		Where("id = ?", sessionID).
		Updates(map[string]any{"status": VoiceStatusPaused, "paused_at": pausedAt}).Error
}

// UpdateResume 恢复落库：status=IN_PROGRESS + resumed_at。
func (r *Repo) UpdateResume(ctx context.Context, sessionID int64, resumedAt time.Time) error {
	return r.db.WithContext(ctx).Model(&SessionEntity{}).
		Where("id = ?", sessionID).
		Updates(map[string]any{"status": VoiceStatusInProgress, "resumed_at": resumedAt}).Error
}

// UpdateEnd 结束落库：endTime/COMPLETED/currentPhase/actualDuration/evaluateStatus
// （列值取自 service 已改写的实体，updated_at 自动刷新）。
func (r *Repo) UpdateEnd(ctx context.Context, e *SessionEntity) error {
	return r.db.WithContext(ctx).Model(&SessionEntity{}).
		Where("id = ?", e.ID).
		Updates(map[string]any{
			"end_time":        e.EndTime,
			"current_phase":   e.CurrentPhase,
			"status":          e.Status,
			"actual_duration": e.ActualDuration,
			"evaluate_status": e.EvaluateStatus,
		}).Error
}

// UpdatePhase 切换阶段（WS start_phase 路径，票 04 消费）。
func (r *Repo) UpdatePhase(ctx context.Context, sessionID int64, phase VoicePhase) error {
	return r.db.WithContext(ctx).Model(&SessionEntity{}).
		Where("id = ?", sessionID).
		Updates(map[string]any{"current_phase": phase}).Error
}

// UpdateEvaluateStatus 评估状态写入（Java updateEvaluateStatus，Producer/Consumer/
// Controller 共用）；evaluate_error 非空截断 500（列宽上限，与上游截断双保险）、
// nil 清空；行不存在静默跳过（对齐 Java ifPresent 由调用方守卫）。
func (r *Repo) UpdateEvaluateStatus(ctx context.Context, sessionID int64, status VoiceEvaluateStatus, evalErr *string) error {
	return r.db.WithContext(ctx).Model(&SessionEntity{}).
		Where("id = ?", sessionID).
		Updates(map[string]any{
			"evaluate_status": status,
			"evaluate_error":  truncateVoiceError(evalErr),
		}).Error
}

// ListByUser 用户会话列表（updated_at desc；status 非空时过滤），对齐 Java
// findByUserIdOrderByUpdatedAtDesc / findByUserIdAndStatusOrderByUpdatedAtDesc。
func (r *Repo) ListByUser(ctx context.Context, userID string, status *VoiceSessionStatus) ([]SessionEntity, error) {
	q := r.db.WithContext(ctx).Where("user_id = ?", userID).Order("updated_at DESC")
	if status != nil {
		q = q.Where("status = ?", *status)
	}
	var list []SessionEntity
	err := q.Find(&list).Error
	return list, err
}

// CountDialogueMessages 非 SUMMARY 消息计数（对齐 Java
// countBySessionIdAndMessageTypeNot(sessionId, MESSAGE_TYPE_SUMMARY)）。
func (r *Repo) CountDialogueMessages(ctx context.Context, sessionID int64) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&MessageEntity{}).
		Where("session_id = ? AND message_type <> ?", sessionID, MessageSummary).
		Count(&n).Error
	return n, err
}

// FindDialogueMessages 非 SUMMARY 消息按 sequence_num 升序（对齐 Java
// findBySessionIdAndMessageTypeNotOrderBySequenceNumAsc；SUMMARY 行在 SQL 侧排除）。
func (r *Repo) FindDialogueMessages(ctx context.Context, sessionID int64) ([]MessageEntity, error) {
	var list []MessageEntity
	err := r.db.WithContext(ctx).
		Where("session_id = ? AND message_type <> ?", sessionID, MessageSummary).
		Order("sequence_num ASC").
		Find(&list).Error
	return list, err
}

// DeleteSessionCascade 级联删除：同一事务内先删评估、再删消息、最后删会话
// （对齐 Java deleteSession L681-690 的语句顺序）。会话不存在返回 ErrSessionNotFound。
func (r *Repo) DeleteSessionCascade(ctx context.Context, sessionID int64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var e SessionEntity
		err := tx.Where("id = ?", sessionID).First(&e).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrSessionNotFound
		}
		if err != nil {
			return err
		}
		if err := tx.Where("session_id = ?", sessionID).Delete(&EvaluationEntity{}).Error; err != nil {
			return err
		}
		if err := tx.Where("session_id = ?", sessionID).Delete(&MessageEntity{}).Error; err != nil {
			return err
		}
		return tx.Delete(&SessionEntity{}, sessionID).Error
	})
}

// truncateVoiceError evaluate_error 截断 500（按字符，对齐 Java substring 语义）。
func truncateVoiceError(s *string) *string {
	if s == nil {
		return nil
	}
	runes := []rune(*s)
	if len(runes) > 500 {
		t := string(runes[:500])
		return &t
	}
	return s
}
