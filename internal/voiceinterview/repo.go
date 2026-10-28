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

// SaveDialogueMessage 插入一条 DIALOGUE/SUMMARY 消息（Java messageRepository.save；
// created_at 为空由 GORM 按字段名自动填充）。
func (r *Repo) SaveDialogueMessage(ctx context.Context, e *MessageEntity) error {
	return r.db.WithContext(ctx).Create(e).Error
}

// FillLatestUnansweredQuestion 最近一条「问题已问、答案未填」消息回填用户答案
// （Java findFirstBySessionIdAndUserRecognizedTextIsNullAndAiGeneratedTextIsNotNull
// OrderBySequenceNumDesc → setUserRecognizedText → save）。无未答问题返回 (false, nil)。
func (r *Repo) FillLatestUnansweredQuestion(ctx context.Context, sessionID int64, userText string) (bool, error) {
	var m MessageEntity
	err := r.db.WithContext(ctx).
		Where("session_id = ? AND user_recognized_text IS NULL AND ai_generated_text IS NOT NULL AND message_type <> ?",
			sessionID, MessageSummary).
		Order("sequence_num DESC").
		First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := r.db.WithContext(ctx).Model(&MessageEntity{}).
		Where("id = ?", m.ID).
		Update("user_recognized_text", userText).Error; err != nil {
		return false, err
	}
	return true, nil
}

// FindRecentDialogueMessages 最近 limit 条非 SUMMARY 消息（倒序取窗口后内存恢复
// 升序，对齐 Java VoiceHistoryLoader 的 PageRequest.of(0, windowSize) 读取形状）。
func (r *Repo) FindRecentDialogueMessages(ctx context.Context, sessionID int64, limit int) ([]MessageEntity, error) {
	var recentDesc []MessageEntity
	err := r.db.WithContext(ctx).
		Where("session_id = ? AND message_type <> ?", sessionID, MessageSummary).
		Order("sequence_num DESC").
		Limit(limit).
		Find(&recentDesc).Error
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(recentDesc)-1; i < j; i, j = i+1, j-1 {
		recentDesc[i], recentDesc[j] = recentDesc[j], recentDesc[i]
	}
	return recentDesc, nil
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

// ===== 评估链路（票 08）=====

// FindEvaluationBySessionID 按场次读评估行（session_id UNIQUE）；
// 不存在返回 ErrEvaluationNotFound。
func (r *Repo) FindEvaluationBySessionID(ctx context.Context, sessionID int64) (*EvaluationEntity, error) {
	var e EvaluationEntity
	err := r.db.WithContext(ctx).Where("session_id = ?", sessionID).First(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrEvaluationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// SaveEvaluation 评估报告落库（Java saveEvaluationTransactional 的 repository.save：
// 新实体恒 INSERT；session_id 冲突由 UNIQUE 约束报错并走消费侧重试路径，与 Java 一致）。
func (r *Repo) SaveEvaluation(ctx context.Context, e *EvaluationEntity) error {
	return r.db.WithContext(ctx).Create(e).Error
}

// SaveEmptyEvaluation 占位空评估（Java saveEmptyEvaluationTransactional）：
// 按场次找既有行——存在则整体更新评估列，否则插入新行。
func (r *Repo) SaveEmptyEvaluation(ctx context.Context, e *EvaluationEntity) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing EvaluationEntity
		err := tx.Where("session_id = ?", *e.SessionID).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Create(e).Error
		}
		if err != nil {
			return err
		}
		return tx.Model(&EvaluationEntity{}).Where("id = ?", existing.ID).Updates(map[string]any{
			"overall_score":             e.OverallScore,
			"overall_feedback":          e.OverallFeedback,
			"question_evaluations_json": e.QuestionEvaluationsJSON,
			"strengths_json":            e.StrengthsJSON,
			"improvements_json":         e.ImprovementsJSON,
			"reference_answers_json":    e.ReferenceAnswersJSON,
			"interviewer_role":          e.InterviewerRole,
			"interview_date":            e.InterviewDate,
		}).Error
	})
}

// FindStaleEvaluateSessions 按评估状态与 updated_at 截止时间查滞留场次
// （对齐 Java findByEvaluateStatusAndUpdatedAtBefore）。
func (r *Repo) FindStaleEvaluateSessions(ctx context.Context, status VoiceEvaluateStatus, before time.Time) ([]SessionEntity, error) {
	var list []SessionEntity
	err := r.db.WithContext(ctx).
		Where("evaluate_status = ? AND updated_at < ?", status, before).
		Find(&list).Error
	return list, err
}

// UpdateEvaluateRequeue 滞留 PENDING 补投前置（Java cleanupStaleSessions L759-768）：
// evaluate_error 清空 + updated_at 推进到巡检时钟（GORM 自动填充同为 updated_at）。
func (r *Repo) UpdateEvaluateRequeue(ctx context.Context, sessionID int64, at time.Time) error {
	return r.db.WithContext(ctx).Model(&SessionEntity{}).
		Where("id = ?", sessionID).
		Updates(map[string]any{"evaluate_error": nil, "updated_at": at}).Error
}

// truncateVoiceError evaluate_error 截断 500（指针版：nil 原样清空列；按字符截断，
// 对齐 Java substring 语义，实现见 evaluation.go 的 truncateVoiceErrMsg）。
func truncateVoiceError(s *string) *string {
	if s == nil {
		return nil
	}
	t := truncateVoiceErrMsg(*s)
	return &t
}
