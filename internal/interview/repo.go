// Repo 面试场次数据访问，模式对齐 resume 模块 repo：WithContext 贯穿、
// 事务句柄透传（tx 为 nil 时退化为独立连接）、按需 FOR UPDATE。
package interview

import (
	"context"
	"errors"

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

// ===== 历史去重（出题输入，对齐 Java InterviewPersistenceService.getHistoricalQuestions）=====

// 历史查询窗口（对齐 Java findTop10… 与 MAX_HISTORICAL_QUESTIONS）。
const (
	maxHistoricalSessions  = 10
	maxHistoricalQuestions = 60
)

// HistoricalQuestion 历史主问题摘要（对齐 Java model HistoricalQuestion record）：
// Type 为分类 key（如 MYSQL/JAVA）；TopicSummary 为知识点摘要，为空时由段落构造截取题面兜底。
type HistoricalQuestion struct {
	Question     string
	Type         string
	TopicSummary *string
}

// GetHistoricalQuestions 出题去重输入（对齐 Java getHistoricalQuestions，370-401 行）：
// 有 resumeId 时按 resumeId + skillId 精确匹配，无 resumeId 时按 skillId 查全部；
// 均取最近 10 场（created_at desc），解析 questions_json 取主问题，
// 按题面去重（保持首见顺序）、最多 60 条；单场 JSON 解析失败跳过该场（对齐 Java log-and-empty）。
func (r *Repo) GetHistoricalQuestions(ctx context.Context, skillID string, resumeID *int64) ([]HistoricalQuestion, error) {
	var rows []SessionEntity
	q := r.db.WithContext(ctx).Model(&SessionEntity{}).
		Select("session_id", "questions_json").
		Order("created_at DESC").Limit(maxHistoricalSessions)
	if resumeID != nil {
		q = q.Where("resume_id = ? AND skill_id = ?", *resumeID, skillID)
	} else {
		q = q.Where("skill_id = ?", skillID)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}

	jsons := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.QuestionsJSON != nil && *row.QuestionsJSON != "" {
			jsons = append(jsons, *row.QuestionsJSON)
		}
	}
	return collectHistoricalQuestions(jsons), nil
}

// collectHistoricalQuestions 纯逻辑（可单测）：逐场解析 → 过滤追问 → 题面去重 → 截 60 条。
func collectHistoricalQuestions(sessionJSONs []string) []HistoricalQuestion {
	seen := map[string]bool{}
	out := []HistoricalQuestion{}
	for _, js := range sessionJSONs {
		questions, err := DeserializeQuestions(js)
		if err != nil {
			continue // 对齐 Java：解析历史问题 JSON 失败仅记日志，跳过该会话
		}
		for _, q := range questions {
			if q.IsFollowUp || seen[q.Question] {
				continue
			}
			seen[q.Question] = true
			out = append(out, HistoricalQuestion{
				Question:     q.Question,
				Type:         categoryValue(q.Type),
				TopicSummary: q.TopicSummary,
			})
			if len(out) >= maxHistoricalQuestions {
				return out
			}
		}
	}
	return out
}

// IsNotFound 归一 gorm 未找到错误。
func IsNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
