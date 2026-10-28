package resume

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/docparse"
	"github.com/wuzuxian/interview/internal/storage"
)

// StreamProducer Redis Stream 生产者接口（stream.go 实现）。
type StreamProducer interface {
	SendAnalyzeTask(ctx context.Context, resumeID int64) bool
}

// AnalysisLookup 历史分析查询能力（HistoryService 实现），重复上传时回放结果。
type AnalysisLookup interface {
	LatestAnalysisAsDTO(ctx context.Context, resumeID int64) (*AnalysisResponse, error)
}

// Uploader 上传服务，对应 Java 版 ResumeUploadService。
type Uploader struct {
	repo         *Repo
	storage      *storage.Service
	producer     StreamProducer
	analyses     AnalysisLookup
	logger       *slog.Logger
	AllowedTypes []string
	MaxFileSize  int64
}

// NewUploader 创建上传服务。
func NewUploader(repo *Repo, st *storage.Service, producer StreamProducer, analyses AnalysisLookup, logger *slog.Logger, allowedTypes []string, maxFileSize int64) *Uploader {
	return &Uploader{repo: repo, storage: st, producer: producer, analyses: analyses, logger: logger, AllowedTypes: allowedTypes, MaxFileSize: maxFileSize}
}

// ValidateFile 校验文件非空且大小未超限。
func (u *Uploader) ValidateFile(size int64, empty bool) error {
	if empty {
		return apperr.BadRequest("请选择要上传的简历文件")
	}
	if size > u.MaxFileSize {
		return apperr.BadRequest("文件大小超过限制")
	}
	return nil
}

// ValidateContentType 按 MIME 列表校验（部分匹配语义与 Java 一致）。
func (u *Uploader) ValidateContentType(contentType string) error {
	if contentType == "" || len(u.AllowedTypes) == 0 {
		return apperr.BadRequest("不支持的文件类型: " + contentType)
	}
	lower := strings.ToLower(contentType)
	for _, allowed := range u.AllowedTypes {
		la := strings.ToLower(allowed)
		if strings.Contains(lower, la) || strings.Contains(la, lower) {
			return nil
		}
	}
	return apperr.BadRequest("不支持的文件类型: " + contentType)
}

// SHA256Hex 计算文件哈希。
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// UploadAndAnalyze 上传简历（异步分析），对应 Java uploadAndAnalyze。
func (u *Uploader) UploadAndAnalyze(ctx context.Context, fileName string, declaredCT string, data []byte) (*UploadResponse, error) {
	// 1. 验证文件
	if err := u.ValidateFile(int64(len(data)), len(data) == 0); err != nil {
		return nil, err
	}
	u.logger.Info("收到简历上传请求",
		slog.String("filename", fileName), slog.Int64("size", int64(len(data))))

	// 2. 检测并校验文件类型（Tika 等价：基于内容嗅探）
	ct := docparse.DetectContentType(data, fileName)
	if err := u.ValidateContentType(ct); err != nil {
		return nil, err
	}

	// 3. 去重
	fileHash := SHA256Hex(data)
	existing, err := u.repo.FindByHash(ctx, fileHash)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		u.logger.Error("检查简历重复时出错", slog.String("error", err.Error()))
		existing = nil
	}
	if existing != nil {
		_ = u.repo.IncrementAccess(ctx, existing.ID)
		return u.duplicateResponse(ctx, existing), nil
	}

	// 4. 解析文本
	parsed, err := docparse.Parse(data, fileName)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(parsed.Text) == "" {
		return nil, apperr.New(apperr.CodeResumeParseFailed, "无法从文件中提取文本内容，请确保文件不是扫描版PDF")
	}
	resumeText := parsed.Text

	// 5. 存储文件
	fileKey, err := u.storage.UploadResume(ctx, fileName, ct, data)
	if err != nil {
		return nil, err
	}
	fileURL := u.storage.FileURL(fileKey)

	// 6. 入库（失败时补偿删除存储对象）
	now := time.Now()
	entity := &ResumeEntity{
		FileHash:             fileHash,
		OriginalFilename:     fileName,
		FileSize:             ptrInt64(int64(len(data))),
		ContentType:          ptrString(declaredCT),
		StorageKey:           ptrString(fileKey),
		StorageURL:           ptrString(fileURL),
		ResumeText:           ptrString(resumeText),
		UploadedAt:           now,
		LastAccessedAt:       ptrTime(now),
		AccessCount:          ptrInt(1),
		AnalyzeStatus:        StatusPending,
		AnalyzeUpdatedAt:     ptrTime(now),
		AnalyzeRecoveryCount: 0,
	}
	if err := u.repo.Create(ctx, entity); err != nil {
		if derr := u.storage.DeleteResume(ctx, fileKey); derr != nil {
			u.logger.Error("补偿删除孤儿对象失败，需人工清理", slog.String("fileKey", fileKey), slog.String("error", derr.Error()))
		} else {
			u.logger.Warn("数据库保存失败已补偿删除孤儿对象", slog.String("fileKey", fileKey))
		}
		return nil, apperr.New(apperr.CodeResumeUploadFailed, "保存简历失败")
	}

	// 7. 投递分析任务；投递失败保留实体（手动重试依据），返回真实状态
	enqueued := u.producer.SendAnalyzeTask(ctx, entity.ID)

	status := StatusPending
	if !enqueued {
		status = StatusFailed
	}
	message := ""
	if !enqueued {
		message = "简历已保存，但分析任务投递失败，请稍后重试"
	}
	return &UploadResponse{
		Resume: &UploadResumeInfo{
			ID:            entity.ID,
			Filename:      entity.OriginalFilename,
			AnalyzeStatus: status,
		},
		Storage: StorageInfo{
			FileKey:  fileKey,
			FileURL:  fileURL,
			ResumeID: entity.ID,
		},
		EnqueueAccepted: enqueued,
		Message:         message,
		Duplicate:       false,
	}, nil
}

// duplicateResponse 重复上传响应：有历史分析时直接返回结果。
func (u *Uploader) duplicateResponse(ctx context.Context, resume *ResumeEntity) *UploadResponse {
	storageInfo := StorageInfo{
		FileKey:  derefStr(resume.StorageKey),
		FileURL:  derefStr(resume.StorageURL),
		ResumeID: resume.ID,
	}
	analysis, err := u.analyses.LatestAnalysisAsDTO(ctx, resume.ID)
	if err == nil && analysis != nil {
		return &UploadResponse{
			Analysis:        analysis,
			Storage:         storageInfo,
			EnqueueAccepted: true,
			Message:         "检测到重复文件，已返回现有分析结果",
			Duplicate:       true,
		}
	}
	status := resume.AnalyzeStatus
	if status == "" {
		status = StatusPending
	}
	return &UploadResponse{
		Resume: &UploadResumeInfo{
			ID:            resume.ID,
			Filename:      resume.OriginalFilename,
			AnalyzeStatus: status,
		},
		Storage:         storageInfo,
		EnqueueAccepted: true,
		Message:         "检测到重复文件，已返回现有简历",
		Duplicate:       true,
	}
}

// Reanalyze 手动重试分析，对应 Java reanalyze。
func (u *Uploader) Reanalyze(ctx context.Context, id int64) error {
	if _, err := u.repo.FindByID(ctx, id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperr.ResumeNotFound()
		}
		return err
	}
	now := time.Now()
	if err := u.repo.MarkReanalyze(ctx, id, now); err != nil {
		return err
	}
	_ = u.repo.ResetAnalyzeRecoveryCount(ctx, id)

	if !u.producer.SendAnalyzeTask(ctx, id) {
		return apperr.Internal("任务投递失败，请稍后重试")
	}
	u.logger.Info("重新分析任务已发送", slog.Int64("resumeId", id))
	return nil
}

// ===== 查询/历史 =====

// HistoryService 简历历史与导出，对应 Java ResumeHistoryService。
type HistoryService struct {
	repo   *Repo
	logger *slog.Logger
}

// NewHistoryService 创建历史服务。
func NewHistoryService(repo *Repo, logger *slog.Logger) *HistoryService {
	return &HistoryService{repo: repo, logger: logger}
}

// GetAllResumes 简历列表（含最新分数与面试次数）。
func (h *HistoryService) GetAllResumes(ctx context.Context) ([]ResumeListItemDTO, error) {
	list, err := h.repo.FindAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ResumeListItemDTO, 0, len(list))
	for i := range list {
		r := &list[i]
		var latestScore *int
		var lastAnalyzedAt *time.Time
		if a, err := h.repo.LatestAnalysis(ctx, r.ID); err == nil {
			latestScore = a.OverallScore
			at := a.AnalyzedAt
			lastAnalyzedAt = &at
		}
		interviewCount, _ := h.repo.InterviewCountByResume(ctx, r.ID)
		out = append(out, ResumeListItemDTO{
			ID:             r.ID,
			Filename:       r.OriginalFilename,
			FileSize:       r.FileSize,
			UploadedAt:     r.UploadedAt,
			AccessCount:    r.AccessCount,
			LatestScore:    latestScore,
			LastAnalyzedAt: lastAnalyzedAt,
			InterviewCount: int(interviewCount),
			AnalyzeStatus:  r.AnalyzeStatus,
			AnalyzeError:   r.AnalyzeError,
		})
	}
	return out, nil
}

// GetResumeDetail 简历详情（含分析历史与面试记录）。
func (h *HistoryService) GetResumeDetail(ctx context.Context, id int64) (*ResumeDetailDTO, error) {
	r, err := h.repo.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apperr.ResumeNotFound()
		}
		return nil, err
	}
	analyses, err := h.repo.AnalysesByResumeID(ctx, id)
	if err != nil {
		return nil, err
	}
	history := make([]AnalysisHistoryDTO, 0, len(analyses))
	for i := range analyses {
		a := &analyses[i]
		strengths := []string{}
		if a.StrengthsJSON != nil {
			_ = json.Unmarshal([]byte(*a.StrengthsJSON), &strengths)
		}
		suggestions := []Suggestion{}
		if a.SuggestionsJSON != nil {
			_ = json.Unmarshal([]byte(*a.SuggestionsJSON), &suggestions)
		}
		// 读取时归一化：修复此前入库的 recommendation 为空的脏数据（前端崩溃源）。
		suggestions = NormalizeSuggestions(suggestions)
		history = append(history, AnalysisHistoryDTO{
			ID:              a.ID,
			OverallScore:    a.OverallScore,
			ContentScore:    a.ContentScore,
			StructureScore:  a.StructureScore,
			SkillMatchScore: a.SkillMatchScore,
			ExpressionScore: a.ExpressionScore,
			ProjectScore:    a.ProjectScore,
			Summary:         a.Summary,
			AnalyzedAt:      a.AnalyzedAt,
			Strengths:       strengths,
			Suggestions:     suggestions,
		})
	}
	sessions, err := h.repo.InterviewsByResume(ctx, id)
	if err != nil {
		return nil, err
	}
	interviews := make([]InterviewHistoryItemDTO, 0, len(sessions))
	for i := range sessions {
		s := &sessions[i]
		interviews = append(interviews, InterviewHistoryItemDTO{
			ID:             s.ID,
			SessionID:      s.SessionID,
			TotalQuestions: s.TotalQuestions,
			Status:         s.Status,
			EvaluateStatus: s.EvaluateStatus,
			EvaluateError:  s.EvaluateError,
			OverallScore:   s.OverallScore,
			CreatedAt:      s.CreatedAt,
			CompletedAt:    s.CompletedAt,
		})
	}
	return &ResumeDetailDTO{
		ID:            r.ID,
		Filename:      r.OriginalFilename,
		FileSize:      r.FileSize,
		ContentType:   r.ContentType,
		StorageURL:    r.StorageURL,
		UploadedAt:    r.UploadedAt,
		AccessCount:   r.AccessCount,
		ResumeText:    r.ResumeText,
		AnalyzeStatus: r.AnalyzeStatus,
		AnalyzeError:  r.AnalyzeError,
		Analyses:      history,
		Interviews:    interviews,
	}, nil
}

// ExportAnalysis 导出 PDF 报告：返回 pdfexport.ReportInput 所需的规范化数据由控制器组装，
// 这里返回分析 DTO。
func (h *HistoryService) ExportAnalysis(ctx context.Context, id int64) (*ResumeEntity, *AnalysisResponse, error) {
	r, err := h.repo.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, apperr.ResumeNotFound()
		}
		return nil, nil, err
	}
	analysis, err := h.LatestAnalysisAsDTO(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, apperr.New(apperr.CodeAnalysisNotFound, "简历分析结果不存在")
		}
		return nil, nil, err
	}
	if analysis == nil {
		return nil, nil, apperr.New(apperr.CodeAnalysisNotFound, "简历分析结果不存在")
	}
	return r, analysis, nil
}

// LatestAnalysisAsDTO 最新分析转 DTO（originalText 取简历正文）。
func (h *HistoryService) LatestAnalysisAsDTO(ctx context.Context, resumeID int64) (*AnalysisResponse, error) {
	a, err := h.repo.LatestAnalysis(ctx, resumeID)
	if err != nil {
		return nil, err
	}
	strengths := []string{}
	if a.StrengthsJSON != nil {
		_ = json.Unmarshal([]byte(*a.StrengthsJSON), &strengths)
	}
	suggestions := []Suggestion{}
	if a.SuggestionsJSON != nil {
		_ = json.Unmarshal([]byte(*a.SuggestionsJSON), &suggestions)
	}
	// 读取时归一化：重复上传回放与 PDF 导出同样不受脏数据影响。
	suggestions = NormalizeSuggestions(suggestions)
	var text string
	if r, err := h.repo.FindByID(ctx, resumeID); err == nil && r.ResumeText != nil {
		text = *r.ResumeText
	}
	return &AnalysisResponse{
		OverallScore: derefInt(a.OverallScore),
		ScoreDetail: ScoreDetail{
			ContentScore:    derefInt(a.ContentScore),
			StructureScore:  derefInt(a.StructureScore),
			SkillMatchScore: derefInt(a.SkillMatchScore),
			ExpressionScore: derefInt(a.ExpressionScore),
			ProjectScore:    derefInt(a.ProjectScore),
		},
		Summary:      derefStr(a.Summary),
		Strengths:    strengths,
		Suggestions:  suggestions,
		OriginalText: text,
	}, nil
}

// GetStatistics 统计信息。
func (h *HistoryService) GetStatistics(ctx context.Context) (*ResumeStats, error) {
	return h.repo.Stats(ctx)
}

// EmbeddingCleaner 简历向量清理能力（vector.Store 实现），删除简历时同步清理向量。
type EmbeddingCleaner interface {
	DeleteResumeEmbedding(ctx context.Context, resumeID int64) error
}

// DeleteService 简历删除服务，对应 Java ResumeDeleteService。
type DeleteService struct {
	repo    *Repo
	storage *storage.Service
	vector  EmbeddingCleaner
	logger  *slog.Logger
}

// NewDeleteService 创建删除服务。
func NewDeleteService(repo *Repo, st *storage.Service, vector EmbeddingCleaner, logger *slog.Logger) *DeleteService {
	return &DeleteService{repo: repo, storage: st, vector: vector, logger: logger}
}

// DeleteResume 删除简历及关联数据（分析记录、面试会话与答案、存储文件）。
func (d *DeleteService) DeleteResume(ctx context.Context, id int64) error {
	d.logger.Info("收到删除简历请求", slog.Int64("id", id))
	r, err := d.repo.FindByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperr.ResumeNotFound()
		}
		return err
	}

	// 1. 删除存储文件（失败不阻断数据库删除）
	if err := d.storage.DeleteResume(ctx, derefStr(r.StorageKey)); err != nil {
		d.logger.Warn("删除存储文件失败，继续删除数据库记录", slog.String("error", err.Error()))
	}
	// 2. 清理向量（尽力而为）
	if d.vector != nil {
		if err := d.vector.DeleteResumeEmbedding(ctx, id); err != nil {
			d.logger.Warn("清理简历向量失败", slog.Int64("id", id), slog.String("error", err.Error()))
		}
	}

	// 3. 数据库事务：删除面试答案 → 面试会话 → 分析记录 → 简历
	err = d.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`DELETE FROM interview_answers WHERE session_id IN (SELECT id FROM interview_sessions WHERE resume_id = ?)`, id).Error; err != nil {
			return err
		}
		if err := tx.Exec(`DELETE FROM interview_sessions WHERE resume_id = ?`, id).Error; err != nil {
			return err
		}
		if err := tx.Exec(`DELETE FROM resume_analyses WHERE resume_id = ?`, id).Error; err != nil {
			return err
		}
		return tx.Delete(&ResumeEntity{}, id).Error
	})
	if err != nil {
		return err
	}
	d.logger.Info("简历删除完成", slog.Int64("id", id))
	return nil
}

// ===== 保存分析结果 =====

// SaveAnalysisIfOwned 行锁校验执行代次后保存分析（防超时回收后旧消费者落库）。
func (h *HistoryService) SaveAnalysisIfOwned(ctx context.Context, resumeID int64, attemptID string, a *AnalysisResponse) error {
	return h.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		r, err := h.repo.FindForUpdate(ctx, tx, resumeID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperr.ResumeNotFound()
			}
			return err
		}
		if r.AnalyzeStatus != StatusProcessing || r.AnalyzeAttemptID == nil || *r.AnalyzeAttemptID != attemptID {
			return apperr.New(apperr.CodeResumeAnalysisFailed, "简历分析任务执行权已失效")
		}
		now := time.Now()
		if err := tx.Model(&ResumeEntity{}).Where("id = ?", resumeID).Update("analyze_updated_at", now).Error; err != nil {
			return err
		}
		return h.createAnalysis(ctx, tx, resumeID, a)
	})
}

// SaveAnalysis 直接保存分析结果。
func (h *HistoryService) SaveAnalysis(ctx context.Context, resumeID int64, a *AnalysisResponse) error {
	return h.repo.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return h.createAnalysis(ctx, tx, resumeID, a)
	})
}

func (h *HistoryService) createAnalysis(ctx context.Context, tx *gorm.DB, resumeID int64, a *AnalysisResponse) error {
	strengthsJSON, err := json.Marshal(a.Strengths)
	if err != nil {
		return apperr.New(apperr.CodeResumeAnalysisFailed, "保存评测结果失败")
	}
	suggestionsJSON, err := json.Marshal(a.Suggestions)
	if err != nil {
		return apperr.New(apperr.CodeResumeAnalysisFailed, "保存评测结果失败")
	}
	entity := &AnalysisEntity{
		ResumeID:        resumeID,
		OverallScore:    ptrInt(a.OverallScore),
		ContentScore:    ptrInt(a.ScoreDetail.ContentScore),
		StructureScore:  ptrInt(a.ScoreDetail.StructureScore),
		SkillMatchScore: ptrInt(a.ScoreDetail.SkillMatchScore),
		ExpressionScore: ptrInt(a.ScoreDetail.ExpressionScore),
		ProjectScore:    ptrInt(a.ScoreDetail.ProjectScore),
		Summary:         ptrString(a.Summary),
		StrengthsJSON:   ptrString(string(strengthsJSON)),
		SuggestionsJSON: ptrString(string(suggestionsJSON)),
		AnalyzedAt:      time.Now(),
	}
	if err := h.repo.CreateAnalysis(ctx, tx, entity); err != nil {
		return apperr.New(apperr.CodeResumeAnalysisFailed, "保存评测结果失败")
	}
	h.logger.Info("简历评测结果已保存",
		slog.Int64("analysisId", entity.ID), slog.Int64("resumeId", resumeID), slog.Int("score", a.OverallScore))
	return nil
}

func ptrInt(v int) *int { return &v }

func ptrInt64(v int64) *int64 { return &v }

func ptrString(v string) *string { return &v }

func ptrTime(v time.Time) *time.Time { return &v }

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
