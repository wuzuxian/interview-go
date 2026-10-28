package resume

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// Repo 简历数据访问：常规 CRUD + P1-07 条件状态更新（多实例安全）。
type Repo struct {
	db *gorm.DB
}

// NewRepo 创建仓库。
func NewRepo(db *gorm.DB) *Repo { return &Repo{db: db} }

// DB 暴露底层连接供事务使用。
func (r *Repo) DB() *gorm.DB { return r.db }

// FindByID 按主键查询。
func (r *Repo) FindByID(ctx context.Context, id int64) (*ResumeEntity, error) {
	var e ResumeEntity
	if err := r.db.WithContext(ctx).First(&e, id).Error; err != nil {
		return nil, err
	}
	return &e, nil
}

// FindByHash 按文件哈希查询（去重）。
func (r *Repo) FindByHash(ctx context.Context, hash string) (*ResumeEntity, error) {
	var e ResumeEntity
	err := r.db.WithContext(ctx).First(&e, "file_hash = ?", hash).Error
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// FindAll 全部简历（按上传时间倒序展示更合理，Java findAll 默认无序，此处稳定排序）。
func (r *Repo) FindAll(ctx context.Context) ([]ResumeEntity, error) {
	var list []ResumeEntity
	err := r.db.WithContext(ctx).Order("uploaded_at DESC").Find(&list).Error
	return list, err
}

// IncrementAccess 重复上传时访问次数 +1。
func (r *Repo) IncrementAccess(ctx context.Context, id int64) error {
	now := time.Now()
	return r.db.WithContext(ctx).Model(&ResumeEntity{}).Where("id = ?", id).
		Updates(map[string]any{"access_count": gorm.Expr("COALESCE(access_count, 0) + 1"), "last_accessed_at": now}).Error
}

// Create 新建简历。
func (r *Repo) Create(ctx context.Context, e *ResumeEntity) error {
	return r.db.WithContext(ctx).Create(e).Error
}

// UpdateResumeText 回填正文（独立短事务）。
func (r *Repo) UpdateResumeText(ctx context.Context, id int64, text string) error {
	return r.db.WithContext(ctx).Model(&ResumeEntity{}).Where("id = ?", id).
		Update("resume_text", text).Error
}

// FindForUpdateSELECT SELECT ... FOR UPDATE 行锁读取。
func (r *Repo) FindForUpdate(ctx context.Context, tx *gorm.DB, id int64) (*ResumeEntity, error) {
	var e ResumeEntity
	if err := tx.WithContext(ctx).Raw("SELECT * FROM resumes WHERE id = ? FOR UPDATE", id).Scan(&e).Error; err != nil {
		return nil, err
	}
	if e.ID == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	return &e, nil
}

// LatestAnalysis 最新一条分析记录。
func (r *Repo) LatestAnalysis(ctx context.Context, resumeID int64) (*AnalysisEntity, error) {
	var e AnalysisEntity
	err := r.db.WithContext(ctx).Where("resume_id = ?", resumeID).
		Order("analyzed_at DESC").First(&e).Error
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// AnalysesByResumeID 全部分析记录（时间倒序）。
func (r *Repo) AnalysesByResumeID(ctx context.Context, resumeID int64) ([]AnalysisEntity, error) {
	var list []AnalysisEntity
	err := r.db.WithContext(ctx).Where("resume_id = ?", resumeID).
		Order("analyzed_at DESC").Find(&list).Error
	return list, err
}

// CreateAnalysis 保存分析记录。
func (r *Repo) CreateAnalysis(ctx context.Context, tx *gorm.DB, e *AnalysisEntity) error {
	db := tx
	if db == nil {
		db = r.db
	}
	return db.WithContext(ctx).Create(e).Error
}

// ===== 条件状态更新：全部返回受影响行数 =====

// TryMarkAnalyzeProcessing PENDING → PROCESSING 条件领取。
func (r *Repo) TryMarkAnalyzeProcessing(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE resumes SET analyze_status = 'PROCESSING', analyze_attempt_id = ?, analyze_updated_at = ? WHERE id = ? AND analyze_status = 'PENDING'",
		attemptID, now, id)
	return res.RowsAffected, res.Error
}

// HeartbeatAnalyzeProcessing 心跳推进。
func (r *Repo) HeartbeatAnalyzeProcessing(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE resumes SET analyze_updated_at = ? WHERE id = ? AND analyze_status = 'PROCESSING' AND analyze_attempt_id = ?",
		now, id, attemptID)
	return res.RowsAffected, res.Error
}

// CompleteAnalyzeIfProcessing PROCESSING → COMPLETED（非 PROCESSING 时 no-op）。
func (r *Repo) CompleteAnalyzeIfProcessing(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE resumes SET analyze_status = 'COMPLETED', analyze_error = NULL, analyze_attempt_id = NULL, analyze_updated_at = ? WHERE id = ? AND analyze_status = 'PROCESSING' AND analyze_attempt_id = ?",
		now, id, attemptID)
	return res.RowsAffected, res.Error
}

// FailAnalyzeIfProcessing 当前执行代次 PROCESSING → FAILED。
func (r *Repo) FailAnalyzeIfProcessing(ctx context.Context, id int64, attemptID, errMsg string, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE resumes SET analyze_status = 'FAILED', analyze_error = ?, analyze_attempt_id = NULL, analyze_updated_at = ? WHERE id = ? AND analyze_status = 'PROCESSING' AND analyze_attempt_id = ?",
		truncate(errMsg, 500), now, id, attemptID)
	return res.RowsAffected, res.Error
}

// FailAnalyzeIfPending 尚未领取的 PENDING → FAILED。
func (r *Repo) FailAnalyzeIfPending(ctx context.Context, id int64, errMsg string, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE resumes SET analyze_status = 'FAILED', analyze_error = ?, analyze_attempt_id = NULL, analyze_updated_at = ? WHERE id = ? AND analyze_status = 'PENDING'",
		truncate(errMsg, 500), now, id)
	return res.RowsAffected, res.Error
}

// ResetAnalyzeToPending PROCESSING → PENDING（重试重置，按执行代次）。
func (r *Repo) ResetAnalyzeToPending(ctx context.Context, id int64, attemptID string, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE resumes SET analyze_status = 'PENDING', analyze_attempt_id = NULL, analyze_updated_at = ? WHERE id = ? AND analyze_status = 'PROCESSING' AND analyze_attempt_id = ?",
		now, id, attemptID)
	return res.RowsAffected, res.Error
}

// TouchQueuedAnalyzeForRecovery 恢复补投前原子推进时间（PENDING 且早于阈值）。
func (r *Repo) TouchQueuedAnalyzeForRecovery(ctx context.Context, id int64, threshold, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE resumes SET analyze_updated_at = ? WHERE id = ? AND analyze_status = 'PENDING' AND analyze_updated_at < ?",
		now, id, threshold)
	return res.RowsAffected, res.Error
}

// ResetStaleAnalyzeProcessing PROCESSING 无进展超阈值 → PENDING。
func (r *Repo) ResetStaleAnalyzeProcessing(ctx context.Context, id int64, threshold, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Exec(
		"UPDATE resumes SET analyze_status = 'PENDING', analyze_attempt_id = NULL, analyze_updated_at = ? WHERE id = ? AND analyze_status = 'PROCESSING' AND analyze_updated_at < ?",
		now, id, threshold)
	return res.RowsAffected, res.Error
}

// IncrementAnalyzeRecoveryCount 恢复计数 +1。
func (r *Repo) IncrementAnalyzeRecoveryCount(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Model(&ResumeEntity{}).Where("id = ?", id).
		Update("analyze_recovery_count", gorm.Expr("analyze_recovery_count + 1")).Error
}

// ResetAnalyzeRecoveryCount 手动重试清零恢复计数。
func (r *Repo) ResetAnalyzeRecoveryCount(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Model(&ResumeEntity{}).Where("id = ?", id).
		Update("analyze_recovery_count", 0).Error
}

// FindStaleByAnalyzeStatus 卡住任务扫描（带每轮上限）。
func (r *Repo) FindStaleByAnalyzeStatus(ctx context.Context, status AsyncTaskStatus, threshold time.Time, limit int) ([]ResumeEntity, error) {
	var list []ResumeEntity
	err := r.db.WithContext(ctx).
		Where("analyze_status = ? AND analyze_updated_at < ?", status, threshold).
		Order("analyze_updated_at ASC").Limit(limit).Find(&list).Error
	return list, err
}

// MarkReanalyze 手动重试：状态置 PENDING、清错误与代次。
func (r *Repo) MarkReanalyze(ctx context.Context, id int64, now time.Time) error {
	return r.db.WithContext(ctx).Model(&ResumeEntity{}).Where("id = ?", id).
		Updates(map[string]any{
			"analyze_status":     StatusPending,
			"analyze_error":      nil,
			"analyze_attempt_id": nil,
			"analyze_updated_at": now,
		}).Error
}

// InterviewCountByResume 统计面试会话数。
func (r *Repo) InterviewCountByResume(ctx context.Context, resumeID int64) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&InterviewSession{}).Where("resume_id = ?", resumeID).Count(&n).Error
	return n, err
}

// InterviewsByResume 面试记录（时间倒序）。
func (r *Repo) InterviewsByResume(ctx context.Context, resumeID int64) ([]InterviewSession, error) {
	var list []InterviewSession
	err := r.db.WithContext(ctx).Where("resume_id = ?", resumeID).
		Order("created_at DESC").Find(&list).Error
	return list, err
}

// Stats 统计：简历总数 / 面试总数 / 访问总数。
func (r *Repo) Stats(ctx context.Context) (*ResumeStats, error) {
	var stats ResumeStats
	if err := r.db.WithContext(ctx).Model(&ResumeEntity{}).Count(&stats.TotalCount).Error; err != nil {
		return nil, err
	}
	if err := r.db.WithContext(ctx).Model(&InterviewSession{}).Count(&stats.TotalInterviewCount).Error; err != nil {
		return nil, err
	}
	err := r.db.WithContext(ctx).Model(&ResumeEntity{}).
		Select("COALESCE(SUM(COALESCE(access_count, 0)), 0)").Scan(&stats.TotalAccessCount).Error
	return &stats, err
}

// Delete 删除简历记录本身。
func (r *Repo) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&ResumeEntity{}, id).Error
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
