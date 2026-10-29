// 题库数据访问与出题状态机事务边界（票 04）：条件更新 SQL 逐条对齐 Java
// KnowledgeBaseQuestionRepository / QuestionGenerationStateService；
// SQL 语义由票 05 smoke 真库兜底，单测以内存 fake 测编排决策。
package knowledgebase

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wuzuxian/interview/internal/apperr"
)

// ErrQuestionNotFound 题目不存在（Java ErrorCode.INTERVIEW_QUESTION_NOT_FOUND 默认文案）。
var ErrQuestionNotFound = apperr.New(apperr.CodeInterviewQuestionNotFound, "面试问题不存在")

// safeFailureMessage 出题失败对外安全文案（Java QuestionGenerationStateService.SAFE_FAILURE_MESSAGE）。
const safeFailureMessage = "题目生成失败，请稍后重试"

// ===== 题库查询（Java KnowledgeBaseQuestionRepository 派生查询）=====

// FindQuestionsByKBOrderByUpdatedAtDesc 全量题目（Java findByKnowledgeBase_IdOrderByUpdatedAtDesc）。
func (r *Repo) FindQuestionsByKBOrderByUpdatedAtDesc(ctx context.Context, kbID int64) ([]KnowledgeBaseQuestionEntity, error) {
	var list []KnowledgeBaseQuestionEntity
	err := r.db.WithContext(ctx).
		Where("knowledge_base_id = ?", kbID).Order("updated_at DESC").Find(&list).Error
	return list, err
}

// FindQuestionsByKBAndStatusOrderByUpdatedAtDesc 按状态过滤（Java 同名派生查询）。
func (r *Repo) FindQuestionsByKBAndStatusOrderByUpdatedAtDesc(ctx context.Context, kbID int64, status QuestionBankStatus) ([]KnowledgeBaseQuestionEntity, error) {
	var list []KnowledgeBaseQuestionEntity
	err := r.db.WithContext(ctx).
		Where("knowledge_base_id = ? AND status = ?", kbID, status).
		Order("updated_at DESC").Find(&list).Error
	return list, err
}

// FindQuestionsByKBAndDifficultyAndStatusOrderByUpdatedAtDesc 按难度 + 状态过滤
// （Java KnowledgeBaseInterviewService.selectActiveQuestions 的无方向分支）。
func (r *Repo) FindQuestionsByKBAndDifficultyAndStatusOrderByUpdatedAtDesc(ctx context.Context, kbID int64, difficulty string, status QuestionBankStatus) ([]KnowledgeBaseQuestionEntity, error) {
	var list []KnowledgeBaseQuestionEntity
	err := r.db.WithContext(ctx).
		Where("knowledge_base_id = ? AND difficulty = ? AND status = ?", kbID, difficulty, status).
		Order("updated_at DESC").Find(&list).Error
	return list, err
}

// FindQuestionsByKBAndDifficultyAndCategoryAndStatusOrderByUpdatedAtDesc
// 按难度 + 方向 + 状态过滤（Java selectActiveQuestions 的方向分支）。
func (r *Repo) FindQuestionsByKBAndDifficultyAndCategoryAndStatusOrderByUpdatedAtDesc(ctx context.Context, kbID int64, difficulty, category string, status QuestionBankStatus) ([]KnowledgeBaseQuestionEntity, error) {
	var list []KnowledgeBaseQuestionEntity
	err := r.db.WithContext(ctx).
		Where("knowledge_base_id = ? AND difficulty = ? AND category = ? AND status = ?", kbID, difficulty, category, status).
		Order("updated_at DESC").Find(&list).Error
	return list, err
}

// FindCategoryCounts 方向统计（Java findCategoryCounts JPQL 逐字：
// 非空方向 group by，count 降序、category 升序）。
func (r *Repo) FindCategoryCounts(ctx context.Context, kbID int64) ([]CategoryCount, error) {
	var list []CategoryCount
	err := r.db.WithContext(ctx).Raw(
		"SELECT category, COUNT(*) AS count FROM knowledge_base_questions "+
			"WHERE knowledge_base_id = ? AND category IS NOT NULL AND category <> '' "+
			"GROUP BY category ORDER BY count DESC, category ASC", kbID).Scan(&list).Error
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []CategoryCount{}
	}
	return list, nil
}

// FindTop20QuestionsByKBAndDifficultyOrderByUpdatedAtDesc 同难度最近 20 题
// （Java findTop20ByKnowledgeBase_IdAndDifficultyOrderByUpdatedAtDesc）。
func (r *Repo) FindTop20QuestionsByKBAndDifficultyOrderByUpdatedAtDesc(ctx context.Context, kbID int64, difficulty string) ([]KnowledgeBaseQuestionEntity, error) {
	var list []KnowledgeBaseQuestionEntity
	err := r.db.WithContext(ctx).
		Where("knowledge_base_id = ? AND difficulty = ?", kbID, difficulty).
		Order("updated_at DESC").Limit(20).Find(&list).Error
	return list, err
}

// FindQuestionByID 按主键查询；不存在返回 ErrQuestionNotFound。
func (r *Repo) FindQuestionByID(ctx context.Context, id int64) (*KnowledgeBaseQuestionEntity, error) {
	var row KnowledgeBaseQuestionEntity
	err := r.db.WithContext(ctx).First(&row, id).Error
	if err == gorm.ErrRecordNotFound {
		return nil, ErrQuestionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// InsertQuestion 新建题目（Java save：created_at/updated_at 由调用方写入）。
func (r *Repo) InsertQuestion(ctx context.Context, q *KnowledgeBaseQuestionEntity) error {
	return r.db.WithContext(ctx).Create(q).Error
}

// UpdateQuestion 全字段保存（Java save 的托管实体更新语义）。
func (r *Repo) UpdateQuestion(ctx context.Context, q *KnowledgeBaseQuestionEntity) error {
	return r.db.WithContext(ctx).Save(q).Error
}

// DeleteQuestionByID 删除题目（Java deleteById）。
func (r *Repo) DeleteQuestionByID(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&KnowledgeBaseQuestionEntity{}, id).Error
}

// ===== 出题状态机（Java QuestionGenerationStateService 事务边界逐条对齐）=====

// CreateQuestionGenTask 创建出题任务：SELECT FOR UPDATE 锁内校验向量完成 + 状态空闲，
// 写 QUEUED + taskId + 配置快照并清空上轮结果（Java createTask）。
func (r *Repo) CreateQuestionGenTask(ctx context.Context, kbID int64, taskID, configJSON string, now time.Time) (*KnowledgeBaseEntity, error) {
	var out *KnowledgeBaseEntity
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row KnowledgeBaseEntity
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, kbID).Error
		if err == gorm.ErrRecordNotFound {
			return ErrKnowledgeBaseNotFound
		}
		if err != nil {
			return err
		}
		if row.VectorStatus != VectorCompleted {
			return apperr.New(apperr.CodeBadRequest, "知识库尚未完成向量化")
		}
		if row.QuestionGenStatus == QuestionGenQueued || row.QuestionGenStatus == QuestionGenProcessing {
			return apperr.New(apperr.CodeBadRequest, "知识库问题正在生成中，请勿重复提交")
		}
		updates := map[string]any{
			"question_gen_task_id":       taskID,
			"question_gen_status":        QuestionGenQueued,
			"question_gen_config":        configJSON,
			"question_gen_error":         nil,
			"question_gen_message":       nil,
			"question_gen_saved_count":   0,
			"question_gen_skipped_count": 0,
			"question_gen_updated_at":    now,
		}
		if err := tx.Model(&KnowledgeBaseEntity{}).Where("id = ?", kbID).Updates(updates).Error; err != nil {
			return err
		}
		if err := tx.First(&row, kbID).Error; err != nil {
			return err
		}
		out = &row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TryMarkQuestionProcessing QUEUED → PROCESSING 条件领取（Java tryMarkProcessing：
// taskId 匹配才允许领取，多实例防重复）。
func (r *Repo) TryMarkQuestionProcessing(ctx context.Context, kbID int64, taskID string, now time.Time) (bool, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET question_gen_status = 'PROCESSING', question_gen_error = NULL, "+
			"question_gen_updated_at = ? WHERE id = ? AND question_gen_task_id = ? AND question_gen_status = 'QUEUED'",
		now, kbID, taskID)
	return res.RowsAffected == 1, res.Error
}

// ResetQuestionForRetry PROCESSING → QUEUED 条件重置（Java resetForRetry）。
func (r *Repo) ResetQuestionForRetry(ctx context.Context, kbID int64, taskID string, now time.Time) (bool, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET question_gen_status = 'QUEUED', question_gen_updated_at = ? "+
			"WHERE id = ? AND question_gen_task_id = ? AND question_gen_status = 'PROCESSING'",
		now, kbID, taskID)
	return res.RowsAffected == 1, res.Error
}

// MarkQuestionGenFailed 任务未被替换且未完成 → FAILED + 安全文案（Java markFailed）。
func (r *Repo) MarkQuestionGenFailed(ctx context.Context, kbID int64, taskID string, now time.Time) (bool, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET question_gen_status = 'FAILED', question_gen_error = ?, "+
			"question_gen_updated_at = ? WHERE id = ? AND question_gen_task_id = ? AND question_gen_status <> 'COMPLETED'",
		safeFailureMessage, now, kbID, taskID)
	return res.RowsAffected == 1, res.Error
}

// ReplaceQuestionsAndComplete replace 小事务（Java replaceQuestionsAndComplete）：
// 锁内校验 taskId 仍匹配且状态 PROCESSING → 删旧题 → 存新题 → COMPLETED +
// saved/skipped/message；被新任务替换（条件不命中）返回 false，调用方丢弃结果。
func (r *Repo) ReplaceQuestionsAndComplete(ctx context.Context, kbID int64, taskID string, questions []KnowledgeBaseQuestionEntity, skippedCount int, now time.Time) (bool, error) {
	completed := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row KnowledgeBaseEntity
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, kbID).Error
		if err != nil {
			return err
		}
		if row.QuestionGenTaskID == nil || *row.QuestionGenTaskID != taskID || row.QuestionGenStatus != QuestionGenProcessing {
			return nil // 被替换：小事务空提交，completed=false
		}
		if err := tx.Where("knowledge_base_id = ?", kbID).Delete(&KnowledgeBaseQuestionEntity{}).Error; err != nil {
			return err
		}
		if len(questions) > 0 {
			if err := tx.Create(&questions).Error; err != nil {
				return err
			}
		}
		message := "已生成 " + strconv.Itoa(len(questions)) + " 道题"
		if skippedCount > 0 {
			message = "已生成 " + strconv.Itoa(len(questions)) + " 道题，跳过 " + strconv.Itoa(skippedCount) + " 道重复题"
		}
		updates := map[string]any{
			"question_gen_status":        QuestionGenCompleted,
			"question_gen_error":         nil,
			"question_gen_message":       message,
			"question_gen_saved_count":   len(questions),
			"question_gen_skipped_count": skippedCount,
			"question_gen_updated_at":    now,
		}
		if err := tx.Model(&KnowledgeBaseEntity{}).Where("id = ?", kbID).Updates(updates).Error; err != nil {
			return err
		}
		completed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return completed, nil
}

// TouchQueuedQuestionForRecovery 恢复补投前原子推进时间（Java touchQueuedForRecovery）。
func (r *Repo) TouchQueuedQuestionForRecovery(ctx context.Context, kbID int64, taskID string, threshold, now time.Time) (bool, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET question_gen_updated_at = ? "+
			"WHERE id = ? AND question_gen_task_id = ? AND question_gen_status = 'QUEUED' "+
			"AND (question_gen_updated_at IS NULL OR question_gen_updated_at < ?)",
		now, kbID, taskID, threshold)
	return res.RowsAffected == 1, res.Error
}

// ResetStaleQuestionProcessing PROCESSING 超阈值无进展 → QUEUED（Java resetStaleProcessing）。
func (r *Repo) ResetStaleQuestionProcessing(ctx context.Context, kbID int64, taskID string, threshold, now time.Time) (bool, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET question_gen_status = 'QUEUED', question_gen_updated_at = ? "+
			"WHERE id = ? AND question_gen_task_id = ? AND question_gen_status = 'PROCESSING' "+
			"AND (question_gen_updated_at IS NULL OR question_gen_updated_at < ?)",
		now, kbID, taskID, threshold)
	return res.RowsAffected == 1, res.Error
}

// FindStaleQuestionGenTasks 卡住出题任务扫描（Java findStaleQuestionGenerationTasks JPQL）。
func (r *Repo) FindStaleQuestionGenTasks(ctx context.Context, status QuestionGenStatus, threshold time.Time, limit int) ([]KnowledgeBaseEntity, error) {
	var list []KnowledgeBaseEntity
	err := r.db.WithContext(ctx).
		Where("question_gen_status = ? AND (question_gen_updated_at IS NULL OR question_gen_updated_at < ?)", status, threshold).
		Limit(limit).Find(&list).Error
	return list, err
}

// ===== 出题配置快照读写（Java readConfigOrNull/writeConfig）=====

// readQuestionGenConfig 解析配置快照；空白返回 nil；损坏按 Java 语义抛 500。
func readQuestionGenConfig(raw *string) (*QuestionGenerationConfig, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	var cfg QuestionGenerationConfig
	if err := json.Unmarshal([]byte(*raw), &cfg); err != nil {
		return nil, apperr.New(apperr.CodeInternalError, "解析题目生成配置失败")
	}
	return &cfg, nil
}

// writeQuestionGenConfig 序列化配置快照（Java writeConfig）。
func writeQuestionGenConfig(cfg QuestionGenerationConfig) (string, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", apperr.New(apperr.CodeInternalError, "序列化题目生成配置失败")
	}
	return string(b), nil
}
