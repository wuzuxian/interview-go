// 知识库上传服务：并发闸门 → 校验 → 内容去重 → 对象存储 → 落库（PENDING）→
// 投递向量化任务。对应 Java KnowledgeBaseUploadService.doUploadKnowledgeBase
// + FileValidationService + KnowledgeBaseUploadLimiter 逐字对齐：
// 落库失败补偿删除 S3 孤儿对象；投递失败如实返回 FAILED 并提示手动重试。
package knowledgebase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/gabriel-vasile/mimetype"

	"github.com/wuzuxian/interview/internal/apperr"
)

// UploadService 知识库上传服务。
type UploadService struct {
	store       Store
	storage     ObjectStorage
	producer    *VectorizeProducer
	logger      *slog.Logger
	maxFileSize int64
	// gate 并发闸门（Java Semaphore(4) 不排队直接拒绝）。
	gate chan struct{}
}

// NewUploadService 创建上传服务（maxConcurrent ≤ 0 视为 1）。
func NewUploadService(store Store, storage ObjectStorage, producer *VectorizeProducer, maxFileSize int64, maxConcurrent int, logger *slog.Logger) *UploadService {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return &UploadService{
		store: store, storage: storage, producer: producer, logger: logger,
		maxFileSize: maxFileSize, gate: make(chan struct{}, maxConcurrent),
	}
}

// Upload 上传知识库文件（闸门内执行完整上传，Java uploadLimiter.execute）。
func (s *UploadService) Upload(ctx context.Context, fileName, headerContentType string, data []byte, name, category string) (*UploadResponse, error) {
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	default:
		return nil, apperr.New(apperr.CodeRateLimitExceeded, "知识库上传繁忙，请稍后重试")
	}
	return s.doUpload(ctx, fileName, headerContentType, data, name, category)
}

func (s *UploadService) doUpload(ctx context.Context, fileName, headerContentType string, data []byte, name, category string) (*UploadResponse, error) {
	// 1. 验证文件（Java validateFile：空文件、大小上限）。
	if len(data) == 0 {
		return nil, apperr.BadRequest("请选择要上传的知识库文件")
	}
	if int64(len(data)) > s.maxFileSize {
		return nil, apperr.BadRequest("文件大小超过限制")
	}
	s.logger.Info("收到知识库上传请求",
		slog.String("filename", fileName), slog.Int("size", len(data)), slog.String("category", category))

	// 2. 验证文件类型（内容嗅探 + md 扩展名兜底）。
	contentType := DetectKnowledgeBaseContentType(data, fileName)
	if !IsKnowledgeBaseMimeType(contentType) && !IsMarkdownExtension(fileName) {
		return nil, apperr.BadRequest("不支持的文件类型: " + contentType + "，支持的类型：PDF、DOCX、DOC、TXT、MD等")
	}

	// 3. 内容去重（SHA-256，file_hash 唯一索引）。
	fileHash := sha256Hex(data)
	existing, err := s.store.FindByFileHash(ctx, fileHash)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		s.logger.Info("检测到重复知识库", slog.String("hash", fileHash))
		return s.duplicateResponse(ctx, existing)
	}

	// 4. 保存文件到对象存储。
	fileKey, err := s.storage.UploadKnowledgeBase(ctx, fileName, headerContentType, data)
	if err != nil {
		return nil, err
	}
	fileURL := s.storage.FileURL(fileKey)
	s.logger.Info("知识库已存储到对象存储", slog.String("fileKey", fileKey))

	// 5. 保存元数据（PENDING）；失败时补偿删除孤儿对象（按 fileKey，不删实体）。
	now := time.Now()
	kb := &KnowledgeBaseEntity{
		FileHash:         fileHash,
		Name:             resolveKnowledgeBaseName(name, fileName),
		Category:         trimToNull(category),
		OriginalFilename: fileName,
		FileSize:         int64Ptr(int64(len(data))),
		ContentType:      stringPtr(headerContentType),
		StorageKey:       stringPtr(fileKey),
		StorageURL:       stringPtr(fileURL),
		UploadedAt:       now,
		LastAccessedAt:   timePtr(now),
		AccessCount:      intPtr(1),
		QuestionCount:    intPtr(0),
		VectorStatus:     VectorPending,
		ChunkCount:       intPtr(0),
		// 进展时间初始化：NULL 会被 stale 扫描的阈值比较排除，导致 PENDING 永远不被恢复。
		VectorUpdatedAt: timePtr(now),
	}
	if err := s.store.InsertKB(ctx, kb); err != nil {
		s.compensateOrphanObject(ctx, fileKey, err)
		return nil, apperr.New(apperr.CodeInternalError, "保存知识库失败")
	}

	// 6. 发送向量化任务；投递失败文件与实体已保存（手动重试与自动恢复的依据），
	//    只返回真实 FAILED 状态，不伪装成 PENDING。
	enqueueAccepted := s.producer.SendVectorizeTask(ctx, kb.ID)
	s.logger.Info("知识库上传完成",
		slog.String("filename", fileName), slog.Int64("kbId", kb.ID), slog.Bool("enqueueAccepted", enqueueAccepted))

	// 7. 响应：状态与数据库真实状态一致。
	vectorStatus := VectorPending
	if !enqueueAccepted {
		vectorStatus = VectorFailed
	}
	return &UploadResponse{
		KnowledgeBase: UploadKnowledgeBaseDTO{
			ID:           kb.ID,
			Name:         kb.Name,
			Category:     deref(kb.Category),
			FileSize:     int64(len(data)),
			VectorStatus: string(vectorStatus),
		},
		Storage:         UploadStorageDTO{FileKey: fileKey, FileURL: fileURL},
		EnqueueAccepted: enqueueAccepted,
		Message:         uploadMessage(enqueueAccepted),
		Duplicate:       false,
	}, nil
}

// duplicateResponse 重复上传：更新访问计数并返回既有条目（Java
// handleDuplicateKnowledgeBase，响应契约与新上传一致）。
func (s *UploadService) duplicateResponse(ctx context.Context, kb *KnowledgeBaseEntity) (*UploadResponse, error) {
	now := time.Now()
	if err := s.store.IncrementAccessCount(ctx, kb.ID, now); err != nil {
		return nil, err
	}
	s.logger.Info("检测到重复知识库，返回已有记录", slog.Int64("kbId", kb.ID))
	status := kb.VectorStatus
	if status == "" {
		status = VectorPending
	}
	return &UploadResponse{
		KnowledgeBase: UploadKnowledgeBaseDTO{
			ID:           kb.ID,
			Name:         kb.Name,
			Category:     deref(kb.Category),
			FileSize:     derefI64(kb.FileSize),
			VectorStatus: string(status),
		},
		Storage:         UploadStorageDTO{FileKey: deref(kb.StorageKey), FileURL: deref(kb.StorageURL)},
		EnqueueAccepted: true,
		Message:         "检测到重复文件，已返回现有知识库",
		Duplicate:       true,
	}, nil
}

// compensateOrphanObject S3 已成功、数据库保存失败的补偿；补偿失败只记录。
func (s *UploadService) compensateOrphanObject(ctx context.Context, fileKey string, originalErr error) {
	if err := s.storage.DeleteKnowledgeBase(ctx, fileKey); err != nil {
		s.logger.Error("补偿删除孤儿对象失败，需人工清理",
			slog.String("fileKey", fileKey), slog.String("error", err.Error()))
		return
	}
	s.logger.Warn("数据库保存失败已补偿删除孤儿对象", slog.String("fileKey", fileKey))
}

// Revectorize 重新向量化（手动重试）：清零恢复计数并重新领取
// （Java uploadService.revectorize）。
func (s *UploadService) Revectorize(ctx context.Context, kbID int64) error {
	kb, err := s.store.FindByID(ctx, kbID)
	if err != nil {
		return err
	}
	if isBlank(deref(kb.StorageKey)) || isBlank(kb.OriginalFilename) {
		return apperr.New(apperr.CodeInternalError, "知识库缺少存储信息，无法重新向量化")
	}
	s.logger.Info("开始重新向量化知识库", slog.Int64("kbId", kbID))

	// 1. 状态置 PENDING 并清零自动恢复计数（手动重试重新获得完整恢复额度）。
	if err := s.store.UpdateVectorStatusToPending(ctx, kbID, time.Now()); err != nil {
		return err
	}
	if err := s.store.ResetVectorRecoveryCount(ctx, kbID); err != nil {
		return err
	}

	// 2. 投递；失败必须让调用方感知，不能只写日志。
	if !s.producer.SendVectorizeTask(ctx, kbID) {
		return apperr.New(apperr.CodeInternalError, "任务投递失败，请稍后重试")
	}
	s.logger.Info("重新向量化任务已发送", slog.Int64("kbId", kbID))
	return nil
}

// uploadMessage 投递失败提示（Java Map.of message 键值逐字）。
func uploadMessage(enqueueAccepted bool) string {
	if enqueueAccepted {
		return ""
	}
	return "文件已保存，但任务投递失败，可在管理页重试"
}

// DetectKnowledgeBaseContentType 内容嗅探 MIME（Tika detect 对应能力）：
// mimetype 库内容嗅探，RTF 归一为 application/rtf（Java 白名单按该值匹配），
// 空内容按扩展名兜底。
func DetectKnowledgeBaseContentType(data []byte, fileName string) string {
	if len(data) > 0 {
		if m := mimetype.Detect(data); m != nil {
			ct := m.String()
			if ct == "text/rtf" {
				return "application/rtf"
			}
			return ct
		}
	}
	switch knowledgeBaseFileExt(fileName) {
	case "pdf":
		return "application/pdf"
	case "doc":
		return "application/msword"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "rtf":
		return "application/rtf"
	case "md", "markdown", "mdown":
		return "text/markdown"
	case "txt", "text":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

// IsKnowledgeBaseMimeType 知识库支持的 MIME 白名单
// （Java FileValidationService.isKnowledgeBaseMimeType，contains 语义逐字对齐）。
func IsKnowledgeBaseMimeType(contentType string) bool {
	if contentType == "" {
		return false
	}
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "pdf") ||
		strings.Contains(ct, "msword") ||
		strings.Contains(ct, "wordprocessingml") ||
		strings.Contains(ct, "text/plain") ||
		strings.Contains(ct, "text/markdown") ||
		strings.Contains(ct, "text/x-markdown") ||
		strings.Contains(ct, "text/x-web-markdown") ||
		strings.Contains(ct, "application/rtf")
}

// IsMarkdownExtension md 扩展名兜底（Java isMarkdownExtension）。
func IsMarkdownExtension(fileName string) bool {
	if fileName == "" {
		return false
	}
	lower := strings.ToLower(fileName)
	return strings.HasSuffix(lower, ".md") ||
		strings.HasSuffix(lower, ".markdown") ||
		strings.HasSuffix(lower, ".mdown")
}

// resolveKnowledgeBaseName 名称：用户指定优先，否则从文件名去扩展名提取
// （Java persistenceService.extractNameFromFilename）。
func resolveKnowledgeBaseName(name, fileName string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	if fileName == "" {
		return "未命名知识库"
	}
	lastDot := strings.LastIndexByte(fileName, '.')
	if lastDot > 0 {
		return fileName[:lastDot]
	}
	return fileName
}

func knowledgeBaseFileExt(name string) string {
	idx := strings.LastIndexByte(name, '.')
	if idx < 0 {
		return ""
	}
	return strings.ToLower(name[idx+1:])
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func trimToNull(s string) *string {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil
	}
	return &t
}

func stringPtr(s string) *string     { return &s }
func intPtr(i int) *int              { return &i }
func int64Ptr(i int64) *int64        { return &i }
func timePtr(t time.Time) *time.Time { return &t }

func derefI64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
