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
	// 历史去重窗口（app.interview.history_* 配置，<1 回落 Java 常量默认值）。
	historyMaxSessions  int
	historyMaxQuestions int
}

// NewRepo 创建仓库。
func NewRepo(db *gorm.DB, cfg Config) *Repo {
	if cfg.HistoryMaxSessions < 1 {
		cfg.HistoryMaxSessions = maxHistoricalSessions
	}
	if cfg.HistoryMaxQuestions < 1 {
		cfg.HistoryMaxQuestions = maxHistoricalQuestions
	}
	return &Repo{db: db, historyMaxSessions: cfg.HistoryMaxSessions, historyMaxQuestions: cfg.HistoryMaxQuestions}
}

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

// FindByID 按内部自增主键查询（评估入队按行 ID 反查业务 sessionId）。
func (r *Repo) FindByID(ctx context.Context, id int64) (*SessionEntity, error) {
	var e SessionEntity
	err := r.db.WithContext(ctx).First(&e, id).Error
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

// UpdateEvaluateStatus 评估状态更新（对齐 Java InterviewPersistenceService.updateEvaluateStatus）：
// evaluate_error 非空截断 500 写入、为空清空；行不存在静默跳过（Java ifPresent）。
func (r *Repo) UpdateEvaluateStatus(ctx context.Context, sessionID string, status EvaluateStatus, evalError *string) error {
	return r.db.WithContext(ctx).Model(&SessionEntity{}).
		Where("session_id = ?", sessionID).
		Updates(map[string]any{
			"evaluate_status": status,
			"evaluate_error":  truncateEvaluateErrorPtr(evalError),
		}).Error
}

// UpdateReportColumns saveReport 的会话列写入（列映射由 reportSessionColumns 构建，
// 含 status=EVALUATED 与 completed_at）。
func (r *Repo) UpdateReportColumns(ctx context.Context, sessionID string, columns map[string]any) error {
	return r.db.WithContext(ctx).Model(&SessionEntity{}).
		Where("session_id = ?", sessionID).Updates(columns).Error
}

// UpsertAnswerEval 评估回填 upsert（saveReport 语义，对齐 Java answersToSave）：
// 已有行仅更新评分列（不动 user_answer/question/category/answered_at）；
// 无行新建（user_answer 保持 NULL 标注未回答，answered_at 取当前时间）。
func (r *Repo) UpsertAnswerEval(ctx context.Context, w answerEvalWrite) error {
	if w.Create {
		row := answerEvalInsertRow(w, time.Now())
		return r.db.WithContext(ctx).Create(&row).Error
	}
	updates := map[string]any{
		"score":    w.Score,
		"feedback": w.Feedback,
	}
	if w.ReferenceAnswer != nil {
		updates["reference_answer"] = w.ReferenceAnswer
	}
	if w.KeyPointsJSON != nil {
		updates["key_points_json"] = w.KeyPointsJSON
	}
	return r.db.WithContext(ctx).Model(&AnswerEntity{}).
		Where("session_id = ? AND question_index = ?", w.SessionRowID, w.QuestionIndex).
		Updates(updates).Error
}

// answerEvalInsertRow 新建回填行的字段（saveReport 语义：userAnswer 保持 NULL
// 标注未回答，score/feedback/参考答案/要点来自评估）。
func answerEvalInsertRow(w answerEvalWrite, now time.Time) AnswerEntity {
	return AnswerEntity{
		SessionID:       w.SessionRowID,
		QuestionIndex:   intPtr(w.QuestionIndex),
		Question:        stringPtr(w.Question),
		Category:        w.Category,
		UserAnswer:      nil,
		Score:           intPtr(w.Score),
		Feedback:        stringPtr(w.Feedback),
		ReferenceAnswer: w.ReferenceAnswer,
		KeyPointsJSON:   w.KeyPointsJSON,
		AnsweredAt:      now,
	}
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

// ===== 历史去重（出题输入，对齐 Java InterviewPersistenceService.getHistoricalQuestions）=====

// 历史查询窗口默认值（对齐 Java findTop10… 与 MAX_HISTORICAL_QUESTIONS；
// 可经 app.interview.history_* 配置覆盖）。
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
// 均取最近 N 场（created_at desc，默认 10），解析 questions_json 取主问题，
// 按题面去重（保持首见顺序）、最多 M 条（默认 60）；单场 JSON 解析失败跳过该场（对齐 Java log-and-empty）。
func (r *Repo) GetHistoricalQuestions(ctx context.Context, skillID string, resumeID *int64) ([]HistoricalQuestion, error) {
	var rows []SessionEntity
	q := r.db.WithContext(ctx).Model(&SessionEntity{}).
		Select("session_id", "questions_json").
		Order("created_at DESC").Limit(r.historyMaxSessions)
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
	return collectHistoricalQuestions(jsons, r.historyMaxQuestions), nil
}

// collectHistoricalQuestions 纯逻辑（可单测）：逐场解析 → 过滤追问 → 题面去重 → 截 limit 条。
func collectHistoricalQuestions(sessionJSONs []string, limit int) []HistoricalQuestion {
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
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

// IsNotFound 归一 gorm 未找到错误。
func IsNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
