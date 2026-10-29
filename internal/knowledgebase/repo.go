// 知识库数据访问：知识库行 + vector_store 行的知识库向量操作。
// 条件更新 SQL 逐条对齐 Java KnowledgeBaseRepository / VectorRepository；
// SQL 语义由票 05 smoke 真库兜底，单测以内存 fake 测编排决策。
package knowledgebase

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/wuzuxian/interview/internal/apperr"
)

// ErrKnowledgeBaseNotFound 知识库不存在（Java 6001 + findById Optional.empty 分支）。
var ErrKnowledgeBaseNotFound = apperr.New(apperr.CodeKnowledgeBaseNotFound, "知识库不存在")

// Store 知识库模块数据面接缝（生产实现 *Repo，测试注入内存 fake）。
type Store interface {
	FindByID(ctx context.Context, id int64) (*KnowledgeBaseEntity, error)
	FindByFileHash(ctx context.Context, hash string) (*KnowledgeBaseEntity, error)
	InsertKB(ctx context.Context, kb *KnowledgeBaseEntity) error
	IncrementAccessCount(ctx context.Context, id int64, at time.Time) error

	// P1-07 条件状态更新（终态不被覆盖，多实例安全）。
	TryMarkVectorProcessing(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error)
	HeartbeatVectorProcessing(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error)
	CompleteVectorIfProcessing(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error)
	FailVectorIfProcessing(ctx context.Context, id int64, attemptID string, errMsg string, now time.Time) (int64, error)
	FailVectorIfPending(ctx context.Context, id int64, errMsg string, now time.Time) (int64, error)
	ResetVectorToPending(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error)
	UpdateVectorStatusToPending(ctx context.Context, id int64, now time.Time) error
	ResetVectorRecoveryCount(ctx context.Context, id int64) error
	TouchQueuedVectorForRecovery(ctx context.Context, id int64, threshold, now time.Time) (int64, error)
	ResetStaleVectorProcessing(ctx context.Context, id int64, threshold, now time.Time) (int64, error)
	IncrementVectorRecoveryCount(ctx context.Context, id int64) error
	FindStaleByVectorStatus(ctx context.Context, status VectorStatus, threshold time.Time, limit int) ([]KnowledgeBaseEntity, error)

	// 两阶段向量落库（Java KnowledgeBasePersistenceService.activateVectorJobAndUpdateSnapshot
	// + VectorRepository 的 delete/promote/deleteByJobId）。
	ActivateVectorJobAndUpdateSnapshot(ctx context.Context, kbID int64, attemptID, jobID string, chunkCount int, vectorConfig string) error
	DeleteVectorsByJobID(ctx context.Context, jobID string) error

	// ===== 管理接口（票 02，查询语义逐条对齐 Java KnowledgeBaseRepository 派生查询）=====

	FindAllByIDs(ctx context.Context, ids []int64) ([]KnowledgeBaseEntity, error)
	FindAllOrderByUploadedAtDesc(ctx context.Context) ([]KnowledgeBaseEntity, error)
	FindByVectorStatusOrderByUploadedAtDesc(ctx context.Context, status VectorStatus) ([]KnowledgeBaseEntity, error)
	FindAllCategories(ctx context.Context) ([]string, error)
	FindByCategoryOrderByUploadedAtDesc(ctx context.Context, category string) ([]KnowledgeBaseEntity, error)
	FindByCategoryIsNullOrderByUploadedAtDesc(ctx context.Context) ([]KnowledgeBaseEntity, error)
	SearchByKeyword(ctx context.Context, keyword string) ([]KnowledgeBaseEntity, error)
	UpdateCategory(ctx context.Context, id int64, category *string) error
	CountKnowledgeBases(ctx context.Context) (int64, error)
	CountByVectorStatus(ctx context.Context, status VectorStatus) (int64, error)
	SumAccessCount(ctx context.Context) (int64, error)
	// CountUserRagMessages 统计 RAG 会话 USER 消息数（Java stats 的总提问次数：
	// ragChatMessageRepository.countByType(MessageType.USER)，多知识库提问只算一次）。
	CountUserRagMessages(ctx context.Context) (int64, error)

	// DeleteKnowledgeBaseRecords 事务内删除：先摘 RAG 会话关联（防 FK 失败）再删记录，
	// 返回 storageKey 供事务外补偿删除对象存储文件（Java KnowledgeBaseDeleteService）。
	DeleteKnowledgeBaseRecords(ctx context.Context, id int64) (*string, error)
	// DeleteVectorsByKBID 事务外删除正式向量（按 metadata kb_id，兼容 kb_id_long 旧键；
	// Java VectorRepository.deleteByKnowledgeBaseId）。
	DeleteVectorsByKBID(ctx context.Context, id int64) error

	// ===== RAG 查询计数（票 03，Java KnowledgeBaseRepository.incrementQuestionCountBatch）=====

	// IncrementQuestionCountBatch 提问计数单条 SQL 批量 +1。
	IncrementQuestionCountBatch(ctx context.Context, ids []int64) error

	// ===== 题库与出题状态机（票 04，Java KnowledgeBaseQuestionRepository +
	// QuestionGenerationStateService 的事务边界）=====

	FindQuestionsByKBOrderByUpdatedAtDesc(ctx context.Context, kbID int64) ([]KnowledgeBaseQuestionEntity, error)
	FindQuestionsByKBAndStatusOrderByUpdatedAtDesc(ctx context.Context, kbID int64, status QuestionBankStatus) ([]KnowledgeBaseQuestionEntity, error)
	// 知识库面试选题查询（票 05，Java KnowledgeBaseInterviewService.selectActiveQuestions
	// 的两条派生查询：按难度取 ACTIVE，方向可空；updated_at 倒序）。
	FindQuestionsByKBAndDifficultyAndStatusOrderByUpdatedAtDesc(ctx context.Context, kbID int64, difficulty string, status QuestionBankStatus) ([]KnowledgeBaseQuestionEntity, error)
	FindQuestionsByKBAndDifficultyAndCategoryAndStatusOrderByUpdatedAtDesc(ctx context.Context, kbID int64, difficulty, category string, status QuestionBankStatus) ([]KnowledgeBaseQuestionEntity, error)
	// FindCategoryCounts 方向统计：非空方向计数，count 降序、category 升序（Java JPQL 逐字）。
	FindCategoryCounts(ctx context.Context, kbID int64) ([]CategoryCount, error)
	// FindTop20QuestionsByKBAndDifficultyOrderByUpdatedAtDesc 出题防重复段（Java findTop20...）。
	FindTop20QuestionsByKBAndDifficultyOrderByUpdatedAtDesc(ctx context.Context, kbID int64, difficulty string) ([]KnowledgeBaseQuestionEntity, error)
	FindQuestionByID(ctx context.Context, id int64) (*KnowledgeBaseQuestionEntity, error)
	InsertQuestion(ctx context.Context, q *KnowledgeBaseQuestionEntity) error
	UpdateQuestion(ctx context.Context, q *KnowledgeBaseQuestionEntity) error
	DeleteQuestionByID(ctx context.Context, id int64) error

	// CreateQuestionGenTask 创建出题任务：锁内校验向量完成 + 状态空闲 →
	// QUEUED + taskId + 配置快照（Java QuestionGenerationStateService.createTask）。
	CreateQuestionGenTask(ctx context.Context, kbID int64, taskID, configJSON string, now time.Time) (*KnowledgeBaseEntity, error)
	// TryMarkQuestionProcessing QUEUED → PROCESSING 条件领取（taskId 仍匹配）。
	TryMarkQuestionProcessing(ctx context.Context, kbID int64, taskID string, now time.Time) (bool, error)
	// ResetQuestionForRetry PROCESSING → QUEUED 条件重置（重试路径）。
	ResetQuestionForRetry(ctx context.Context, kbID int64, taskID string, now time.Time) (bool, error)
	// MarkQuestionGenFailed 任务未被替换且未完成 → FAILED + 安全文案。
	MarkQuestionGenFailed(ctx context.Context, kbID int64, taskID string, now time.Time) (bool, error)
	// ReplaceQuestionsAndComplete replace 小事务：taskId 仍匹配且 PROCESSING →
	// 删旧题 → 存新题 → COMPLETED + saved/skipped/message；被替换返回 false。
	ReplaceQuestionsAndComplete(ctx context.Context, kbID int64, taskID string, questions []KnowledgeBaseQuestionEntity, skippedCount int, now time.Time) (bool, error)
	// TouchQueuedQuestionForRecovery 恢复补投前原子推进时间（QUEUED 且早于阈值）。
	TouchQueuedQuestionForRecovery(ctx context.Context, kbID int64, taskID string, threshold, now time.Time) (bool, error)
	// ResetStaleQuestionProcessing PROCESSING 超阈值无进展 → QUEUED。
	ResetStaleQuestionProcessing(ctx context.Context, kbID int64, taskID string, threshold, now time.Time) (bool, error)
	// FindStaleQuestionGenTasks 卡住出题任务扫描（Java findStaleQuestionGenerationTasks）。
	FindStaleQuestionGenTasks(ctx context.Context, status QuestionGenStatus, threshold time.Time, limit int) ([]KnowledgeBaseEntity, error)
}

// Repo GORM 实现。
type Repo struct {
	db     *gorm.DB
	logger *slog.Logger
}

// NewRepo 创建仓储。
func NewRepo(db *gorm.DB, logger *slog.Logger) *Repo {
	return &Repo{db: db, logger: logger}
}

// FindByID 按主键查询；不存在返回 ErrKnowledgeBaseNotFound。
func (r *Repo) FindByID(ctx context.Context, id int64) (*KnowledgeBaseEntity, error) {
	var row KnowledgeBaseEntity
	err := r.db.WithContext(ctx).First(&row, id).Error
	if err == gorm.ErrRecordNotFound {
		return nil, ErrKnowledgeBaseNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// FindByFileHash 按内容哈希查询（去重）；不存在返回 nil（Java Optional.empty）。
func (r *Repo) FindByFileHash(ctx context.Context, hash string) (*KnowledgeBaseEntity, error) {
	var row KnowledgeBaseEntity
	err := r.db.WithContext(ctx).Where("file_hash = ?", hash).First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// InsertKB 新建知识库行（Java saveKnowledgeBase：PENDING + vector_updated_at 初始化）。
func (r *Repo) InsertKB(ctx context.Context, kb *KnowledgeBaseEntity) error {
	return r.db.WithContext(ctx).Create(kb).Error
}

// IncrementAccessCount 访问计数 +1 并推进最近访问（Java incrementAccessCount）。
func (r *Repo) IncrementAccessCount(ctx context.Context, id int64, at time.Time) error {
	return r.db.WithContext(ctx).Model(&KnowledgeBaseEntity{}).Where("id = ?", id).
		Updates(map[string]any{
			"access_count":     gorm.Expr("COALESCE(access_count, 0) + 1"),
			"last_accessed_at": at,
		}).Error
}

// TryMarkVectorProcessing PENDING → PROCESSING 条件领取。
func (r *Repo) TryMarkVectorProcessing(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET vector_status = 'PROCESSING', vector_attempt_id = ?, vector_updated_at = ? WHERE id = ? AND vector_status = 'PENDING'",
		attemptID, now, id)
	return res.RowsAffected, res.Error
}

// HeartbeatVectorProcessing 心跳：仍为 PROCESSING 且代次匹配才推进进展时间。
func (r *Repo) HeartbeatVectorProcessing(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET vector_updated_at = ? WHERE id = ? AND vector_status = 'PROCESSING' AND vector_attempt_id = ?",
		now, id, attemptID)
	return res.RowsAffected, res.Error
}

// CompleteVectorIfProcessing PROCESSING → COMPLETED（非本代次 no-op）。
func (r *Repo) CompleteVectorIfProcessing(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET vector_status = 'COMPLETED', vector_error = NULL, vector_attempt_id = NULL, vector_updated_at = ? WHERE id = ? AND vector_status = 'PROCESSING' AND vector_attempt_id = ?",
		now, id, attemptID)
	return res.RowsAffected, res.Error
}

// FailVectorIfProcessing 当前执行代次 PROCESSING → FAILED。
func (r *Repo) FailVectorIfProcessing(ctx context.Context, id int64, attemptID string, errMsg string, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET vector_status = 'FAILED', vector_error = ?, vector_attempt_id = NULL, vector_updated_at = ? WHERE id = ? AND vector_status = 'PROCESSING' AND vector_attempt_id = ?",
		truncateError(errMsg, 500), now, id, attemptID)
	return res.RowsAffected, res.Error
}

// FailVectorIfPending 尚未领取的 PENDING → FAILED。
func (r *Repo) FailVectorIfPending(ctx context.Context, id int64, errMsg string, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET vector_status = 'FAILED', vector_error = ?, vector_attempt_id = NULL, vector_updated_at = ? WHERE id = ? AND vector_status = 'PENDING'",
		truncateError(errMsg, 500), now, id)
	return res.RowsAffected, res.Error
}

// ResetVectorToPending PROCESSING → PENDING（重试重置，按执行代次）。
func (r *Repo) ResetVectorToPending(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET vector_status = 'PENDING', vector_attempt_id = NULL, vector_updated_at = ? WHERE id = ? AND vector_status = 'PROCESSING' AND vector_attempt_id = ?",
		now, id, attemptID)
	return res.RowsAffected, res.Error
}

// UpdateVectorStatusToPending 手动重试：状态置 PENDING、清错误与代次、推进时间
// （Java persistenceService.updateVectorStatusToPending）。
func (r *Repo) UpdateVectorStatusToPending(ctx context.Context, id int64, now time.Time) error {
	res := r.db.WithContext(ctx).Model(&KnowledgeBaseEntity{}).Where("id = ?", id).
		Updates(map[string]any{
			"vector_status":     VectorPending,
			"vector_error":      nil,
			"vector_attempt_id": nil,
			"vector_updated_at": now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrKnowledgeBaseNotFound
	}
	return nil
}

// ResetVectorRecoveryCount 手动重试清零恢复计数。
func (r *Repo) ResetVectorRecoveryCount(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Model(&KnowledgeBaseEntity{}).Where("id = ?", id).
		Update("vector_recovery_count", 0).Error
}

// TouchQueuedVectorForRecovery 恢复补投前原子推进时间（PENDING 且早于阈值）。
func (r *Repo) TouchQueuedVectorForRecovery(ctx context.Context, id int64, threshold, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET vector_updated_at = ? WHERE id = ? AND vector_status = 'PENDING' AND vector_updated_at < ?",
		now, id, threshold)
	return res.RowsAffected, res.Error
}

// ResetStaleVectorProcessing PROCESSING 无进展超阈值 → PENDING。
func (r *Repo) ResetStaleVectorProcessing(ctx context.Context, id int64, threshold, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE knowledge_bases SET vector_status = 'PENDING', vector_attempt_id = NULL, vector_updated_at = ? WHERE id = ? AND vector_status = 'PROCESSING' AND vector_updated_at < ?",
		now, id, threshold)
	return res.RowsAffected, res.Error
}

// IncrementVectorRecoveryCount 恢复计数 +1。
func (r *Repo) IncrementVectorRecoveryCount(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Model(&KnowledgeBaseEntity{}).Where("id = ?", id).
		Update("vector_recovery_count", gorm.Expr("vector_recovery_count + 1")).Error
}

// FindStaleByVectorStatus 卡住任务扫描（带每轮上限）。
func (r *Repo) FindStaleByVectorStatus(ctx context.Context, status VectorStatus, threshold time.Time, limit int) ([]KnowledgeBaseEntity, error) {
	var list []KnowledgeBaseEntity
	err := r.db.WithContext(ctx).
		Where("vector_status = ? AND vector_updated_at < ?", status, threshold).
		Order("vector_updated_at ASC").Limit(limit).Find(&list).Error
	return list, err
}

// ActivateVectorJobAndUpdateSnapshot 数据库锁内校验执行代次，先删旧正式向量，
// 再把临时向量 promote 为正式向量，并写入 chunk_count / vector_config / 完成时间
// （Java activateVectorJobAndUpdateSnapshot，删除/promote SQL 与 Java VectorRepository 逐字对齐）。
func (r *Repo) ActivateVectorJobAndUpdateSnapshot(ctx context.Context, kbID int64, attemptID, jobID string, chunkCount int, vectorConfig string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row KnowledgeBaseEntity
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, kbID).Error
		if err == gorm.ErrRecordNotFound {
			return ErrKnowledgeBaseNotFound
		}
		if err != nil {
			return err
		}
		if row.VectorStatus != VectorProcessing || row.VectorAttemptID == nil || *row.VectorAttemptID != attemptID {
			return apperr.New(apperr.CodeKnowledgeBaseVectorizationFailed, "向量化任务执行权已失效")
		}
		if err := deleteVectorsByKBID(tx, kbID); err != nil {
			return err
		}
		if err := promoteVectorsByJobID(tx, kbID, jobID); err != nil {
			return err
		}
		now := time.Now()
		return tx.Model(&KnowledgeBaseEntity{}).Where("id = ?", kbID).
			Updates(map[string]any{
				"chunk_count":       chunkCount,
				"vector_config":     vectorConfig,
				"vectorized_at":     now,
				"vector_updated_at": now,
			}).Error
	})
}

// DeleteVectorsByJobID 按 jobId 清理临时向量（Java VectorRepository.deleteByVectorJobId）。
func (r *Repo) DeleteVectorsByJobID(ctx context.Context, jobID string) error {
	return promoteError(r.db.WithContext(ctx).Exec(
		"DELETE FROM vector_store WHERE metadata->>'kb_vector_job_id' = ?", jobID))
}

// deleteVectorsByKBID 删除指定知识库的正式向量
// （Java VectorRepository.deleteByKnowledgeBaseId，兼容 kb_id_long 旧键）。
func deleteVectorsByKBID(tx *gorm.DB, kbID int64) error {
	res := tx.Exec(
		"DELETE FROM vector_store WHERE metadata->>'kb_id' = ? OR (metadata->>'kb_id_long' IS NOT NULL AND (metadata->>'kb_id_long')::bigint = ?)",
		fmt.Sprintf("%d", kbID), kbID)
	if res.Error != nil {
		return apperr.Wrap(apperr.CodeKnowledgeBaseDeleteFailed, "删除向量数据失败", res.Error)
	}
	return nil
}

// promoteVectorsByJobID 临时向量提升为正式向量
// （Java VectorRepository.promoteVectorJob：jsonb_set 提升 kb_id 并删除临时字段）。
func promoteVectorsByJobID(tx *gorm.DB, kbID int64, jobID string) error {
	res := tx.Exec(`UPDATE vector_store
		SET metadata = (jsonb_set(
				metadata::jsonb,
				'{kb_id}',
				to_jsonb(?::text),
				true
			) - 'kb_vector_job_id' - 'kb_target_id')::json
		WHERE metadata->>'kb_vector_job_id' = ?`, fmt.Sprintf("%d", kbID), jobID)
	return promoteError(res)
}

func promoteError(res *gorm.DB) error {
	if res.Error != nil {
		return apperr.Wrap(apperr.CodeKnowledgeBaseVectorizationFailed, "向量化数据操作失败", res.Error)
	}
	return nil
}

// truncateError 错误消息截断（Java truncateError，vector_error 列宽 500）。
func truncateError(msg string, max int) string {
	r := []rune(msg)
	if len(r) > max {
		return string(r[:max])
	}
	return msg
}

// ===== 管理接口（票 02）=====

// FindAllByIDs 按主键批量查询（Java findAllById）。
func (r *Repo) FindAllByIDs(ctx context.Context, ids []int64) ([]KnowledgeBaseEntity, error) {
	var list []KnowledgeBaseEntity
	err := r.db.WithContext(ctx).Where("id IN ?", ids).Find(&list).Error
	return list, err
}

// IncrementQuestionCountBatch 提问计数单条 SQL 批量 +1
// （Java incrementQuestionCountBatch：UPDATE knowledge_bases SET question_count =
// COALESCE(question_count,0)+1 WHERE id IN (:ids)）。
func (r *Repo) IncrementQuestionCountBatch(ctx context.Context, ids []int64) error {
	return r.db.WithContext(ctx).Model(&KnowledgeBaseEntity{}).Where("id IN ?", ids).
		Update("question_count", gorm.Expr("COALESCE(question_count, 0) + 1")).Error
}

// FindAllOrderByUploadedAtDesc 全量列表（Java findAllByOrderByUploadedAtDesc）。
func (r *Repo) FindAllOrderByUploadedAtDesc(ctx context.Context) ([]KnowledgeBaseEntity, error) {
	var list []KnowledgeBaseEntity
	err := r.db.WithContext(ctx).Order("uploaded_at DESC").Find(&list).Error
	return list, err
}

// FindByVectorStatusOrderByUploadedAtDesc 按向量状态过滤（Java 同名派生查询）。
func (r *Repo) FindByVectorStatusOrderByUploadedAtDesc(ctx context.Context, status VectorStatus) ([]KnowledgeBaseEntity, error) {
	var list []KnowledgeBaseEntity
	err := r.db.WithContext(ctx).Where("vector_status = ?", status).Order("uploaded_at DESC").Find(&list).Error
	return list, err
}

// FindAllCategories 全部分类：去重、非空、按分类升序（Java findAllCategories JPQL 逐字）。
func (r *Repo) FindAllCategories(ctx context.Context) ([]string, error) {
	var list []string
	err := r.db.WithContext(ctx).Raw(
		"SELECT DISTINCT category FROM knowledge_bases WHERE category IS NOT NULL ORDER BY category",
	).Scan(&list).Error
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []string{}
	}
	return list, nil
}

// FindByCategoryOrderByUploadedAtDesc 按分类浏览。
func (r *Repo) FindByCategoryOrderByUploadedAtDesc(ctx context.Context, category string) ([]KnowledgeBaseEntity, error) {
	var list []KnowledgeBaseEntity
	err := r.db.WithContext(ctx).Where("category = ?", category).Order("uploaded_at DESC").Find(&list).Error
	return list, err
}

// FindByCategoryIsNullOrderByUploadedAtDesc 未分类浏览。
func (r *Repo) FindByCategoryIsNullOrderByUploadedAtDesc(ctx context.Context) ([]KnowledgeBaseEntity, error) {
	var list []KnowledgeBaseEntity
	err := r.db.WithContext(ctx).Where("category IS NULL").Order("uploaded_at DESC").Find(&list).Error
	return list, err
}

// SearchByKeyword 名称或原文件名模糊搜索，不区分大小写（Java searchByKeyword JPQL：
// LOWER(...) LIKE LOWER(CONCAT('%', :keyword, '%'))，通配符不过转义，行为一致）。
func (r *Repo) SearchByKeyword(ctx context.Context, keyword string) ([]KnowledgeBaseEntity, error) {
	pattern := "%" + strings.ToLower(keyword) + "%"
	var list []KnowledgeBaseEntity
	err := r.db.WithContext(ctx).
		Where("LOWER(name) LIKE ? OR LOWER(original_filename) LIKE ?", pattern, pattern).
		Order("uploaded_at DESC").Find(&list).Error
	return list, err
}

// UpdateCategory 设置分类：先查后改，不存在返回 6001（Java updateCategory）；
// category nil 落 NULL（Java setCategory(blank → null)）。
func (r *Repo) UpdateCategory(ctx context.Context, id int64, category *string) error {
	var row KnowledgeBaseEntity
	err := r.db.WithContext(ctx).First(&row, id).Error
	if err == gorm.ErrRecordNotFound {
		return ErrKnowledgeBaseNotFound
	}
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Model(&KnowledgeBaseEntity{}).Where("id = ?", id).
		Update("category", category).Error
}

// CountKnowledgeBases 知识库总数（Java count()）。
func (r *Repo) CountKnowledgeBases(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&KnowledgeBaseEntity{}).Count(&n).Error
	return n, err
}

// CountByVectorStatus 按向量状态计数（Java countByVectorStatus）。
func (r *Repo) CountByVectorStatus(ctx context.Context, status VectorStatus) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&KnowledgeBaseEntity{}).Where("vector_status = ?", status).Count(&n).Error
	return n, err
}

// SumAccessCount 访问总数（Java sumAccessCount：COALESCE(SUM(access_count), 0)）。
func (r *Repo) SumAccessCount(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Raw(
		"SELECT COALESCE(SUM(access_count), 0) FROM knowledge_bases").Scan(&n).Error
	return n, err
}

// CountUserRagMessages RAG 会话 USER 消息数（Java stats 总提问次数口径：
// ragChatMessageRepository.countByType(MessageType.USER)，多知识库提问只算一次）。
func (r *Repo) CountUserRagMessages(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM rag_chat_messages WHERE type = 'USER'").Scan(&n).Error
	return n, err
}

// DeleteKnowledgeBaseRecords 事务内删除：摘 RAG 会话关联 → 删知识库记录，
// 返回 storageKey（Java deleteKnowledgeBaseRecords；不存在按 Java NOT_FOUND(404) + "知识库不存在"）。
func (r *Repo) DeleteKnowledgeBaseRecords(ctx context.Context, id int64) (*string, error) {
	var storageKey *string
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row KnowledgeBaseEntity
		err := tx.First(&row, id).Error
		if err == gorm.ErrRecordNotFound {
			return apperr.New(apperr.CodeNotFound, "知识库不存在")
		}
		if err != nil {
			return err
		}
		storageKey = row.StorageKey
		// Java：先逐会话移除关联再删记录，防止外键约束阻止删除。
		if err := tx.Exec("DELETE FROM rag_session_knowledge_bases WHERE knowledge_base_id = ?", id).Error; err != nil {
			return err
		}
		return tx.Delete(&KnowledgeBaseEntity{}, id).Error
	})
	if err != nil {
		return nil, err
	}
	return storageKey, nil
}

// DeleteVectorsByKBID 事务外删除正式向量（SQL 与 Java VectorRepository
// .deleteByKnowledgeBaseId 逐字对齐，兼容 kb_id_long 旧键；失败以 6005 抛出，
// 由删除服务吞掉仅记日志）。
func (r *Repo) DeleteVectorsByKBID(ctx context.Context, id int64) error {
	return deleteVectorsByKBID(r.db.WithContext(ctx), id)
}
