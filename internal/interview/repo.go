// Repo 面试场次数据访问，模式对齐 resume 模块 repo：WithContext 贯穿、
// 事务句柄透传（tx 为 nil 时退化为独立连接）、按需 FOR UPDATE。
package interview

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// Repo interview_sessions / interview_answers 数据访问。
type Repo struct {
	db *gorm.DB
}

// NewRepo 创建仓库。
func NewRepo(db *gorm.DB) *Repo { return &Repo{db: db} }

// DB 暴露底层连接供事务使用。
func (r *Repo) DB() *gorm.DB { return r.db }

// FindBySessionID 按业务 sessionId（uuid 前 16 位）查询。
func (r *Repo) FindBySessionID(ctx context.Context, sessionID string) (*SessionEntity, error) {
	var e SessionEntity
	err := r.db.WithContext(ctx).Where("session_id = ?", sessionID).First(&e).Error
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// FindByRequestID 按幂等键查询（DB 幂等第二层兜底）。
func (r *Repo) FindByRequestID(ctx context.Context, requestID string) (*SessionEntity, error) {
	var e SessionEntity
	err := r.db.WithContext(ctx).Where("request_id = ?", requestID).First(&e).Error
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// FindUnfinishedByResumeID 未完成场次（CREATED/IN_PROGRESS），按 created_at desc 取第一条
// （对齐 Java findUnfinishedSession 查询语义）。
func (r *Repo) FindUnfinishedByResumeID(ctx context.Context, resumeID int64) (*SessionEntity, error) {
	var e SessionEntity
	err := r.db.WithContext(ctx).
		Where("resume_id = ? AND status IN ?", resumeID, []SessionStatus{StatusCreated, StatusInProgress}).
		Order("created_at DESC").First(&e).Error
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// FindAll 全部场次（created_at desc，对齐 Java findAllByOrderByCreatedAtDesc）。
func (r *Repo) FindAll(ctx context.Context) ([]SessionEntity, error) {
	var list []SessionEntity
	err := r.db.WithContext(ctx).Order("created_at DESC").Find(&list).Error
	return list, err
}

// AnswersBySessionID 场次全部作答（按题号升序），DB 恢复路径用 userAnswer 覆盖题目。
func (r *Repo) AnswersBySessionID(ctx context.Context, sessionRowID int64) ([]AnswerEntity, error) {
	var list []AnswerEntity
	err := r.db.WithContext(ctx).Where("session_id = ?", sessionRowID).
		Order("question_index ASC").Find(&list).Error
	return list, err
}

// CreateWithTx 插入场次；tx 非空时在事务内执行（唯一约束兜底依赖与后续查询同库可见）。
func (r *Repo) CreateWithTx(ctx context.Context, tx *gorm.DB, e *SessionEntity) error {
	db := tx
	if db == nil {
		db = r.db
	}
	return db.WithContext(ctx).Create(e).Error
}

// DeleteBySessionID 删除场次并级联删作答（对齐 Java JPA cascade = ALL,
// orphanRemoval = true 的删除语义），同一事务内先删 answers 再删 session。
// 记录不存在返回 gorm.ErrRecordNotFound（对齐 Java 抛 3001）。
func (r *Repo) DeleteBySessionID(ctx context.Context, sessionID string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var e SessionEntity
		if err := tx.Where("session_id = ?", sessionID).First(&e).Error; err != nil {
			return err
		}
		if err := tx.Where("session_id = ?", e.ID).Delete(&AnswerEntity{}).Error; err != nil {
			return err
		}
		return tx.Delete(&SessionEntity{}, e.ID).Error
	})
}

// UpdateStatus 更新场次状态（对齐 Java updateSessionStatus：转入 COMPLETED/EVALUATED
// 时同步写 completed_at；行不存在静默跳过，对齐 Java isPresent 守卫）。
func (r *Repo) UpdateStatus(ctx context.Context, sessionID string, status SessionStatus) error {
	updates := map[string]any{"status": status}
	if status == StatusCompleted || status == StatusEvaluated {
		updates["completed_at"] = time.Now()
	}
	return r.db.WithContext(ctx).Model(&SessionEntity{}).
		Where("session_id = ?", sessionID).Updates(updates).Error
}

// UpdateCurrentQuestionIndex 更新作答进度，并像 Java 一样一并置状态 IN_PROGRESS
// （submitAnswer 随后以 updateSessionStatus 覆盖末题的 COMPLETED；行不存在静默跳过）。
func (r *Repo) UpdateCurrentQuestionIndex(ctx context.Context, sessionID string, index int) error {
	return r.db.WithContext(ctx).Model(&SessionEntity{}).
		Where("session_id = ?", sessionID).
		Updates(map[string]any{"current_question_index": index, "status": StatusInProgress}).Error
}

// UpsertAnswer 按（场次内部ID, 题号）upsert 作答：已有行 → 更新（不新增行，
// answered_at 刷新——票面验收要求，Java 仅 PrePersist 首次写入，此为有意覆盖）；
// 无行 → 新建。score/feedback 由调用方语义决定（暂存/提交恒 0/空，评估时才回填）。
func (r *Repo) UpsertAnswer(ctx context.Context, sessionRowID int64, index int, question string, category, userAnswer *string) error {
	var existing AnswerEntity
	err := r.db.WithContext(ctx).
		Where("session_id = ? AND question_index = ?", sessionRowID, index).
		First(&existing).Error
	now := time.Now()
	if IsNotFound(err) {
		row := answerRowForInsert(sessionRowID, index, question, category, userAnswer, now)
		return r.db.WithContext(ctx).Create(&row).Error
	}
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Model(&AnswerEntity{}).
		Where("id = ?", existing.ID).
		Updates(answerUpdateColumns(question, category, userAnswer, now)).Error
}

// answerRowForInsert 新建作答行的字段：score 恒 0、feedback 空（对齐 Java
// saveAnswer(..., 0, null)——分数由评估回填），answered_at 取当前时间。
func answerRowForInsert(sessionRowID int64, index int, question string, category, userAnswer *string, now time.Time) AnswerEntity {
	return AnswerEntity{
		SessionID:     sessionRowID,
		QuestionIndex: intPtr(index),
		Question:      stringPtr(question),
		Category:      category,
		UserAnswer:    userAnswer,
		Score:         intPtr(0),
		Feedback:      nil,
		AnsweredAt:    now,
	}
}

// answerUpdateColumns 更新既有作答行的列：仅作答内容与 answered_at，
// 不触碰 id/session_id/question_index（upsert 不产生重复行的关键）。
func answerUpdateColumns(question string, category, userAnswer *string, now time.Time) map[string]any {
	return map[string]any{
		"question":    stringPtr(question),
		"category":    category,
		"user_answer": userAnswer,
		"score":       intPtr(0),
		"feedback":    nil,
		"answered_at": now,
	}
}

// ResumeTextByID 取关联简历正文（对齐 Java entity.getResume().getResumeText() 的 JPA join 语义）；
// 简历不存在或正文为空返回 (nil, nil)。
func (r *Repo) ResumeTextByID(ctx context.Context, resumeID int64) (*string, error) {
	var text *string
	err := r.db.WithContext(ctx).Raw("SELECT resume_text FROM resumes WHERE id = ?", resumeID).
		Scan(&text).Error
	if err != nil {
		return nil, err
	}
	return text, nil
}

// ResumeExists 简历是否存在（创建时对齐 Java resumeOpt.ifPresent 的静默关联前置判断）。
func (r *Repo) ResumeExists(ctx context.Context, resumeID int64) (bool, error) {
	var n int64
	err := r.db.WithContext(ctx).Raw("SELECT COUNT(*) FROM resumes WHERE id = ?", resumeID).Scan(&n).Error
	return n > 0, err
}

// IsNotFound 归一 gorm 未找到错误。
func IsNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
