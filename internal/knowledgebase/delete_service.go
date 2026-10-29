// 知识库删除服务：补偿顺序逐字对齐 Java KnowledgeBaseDeleteService.deleteKnowledgeBase：
// 事务内先摘 RAG 会话关联（防外键约束）→ 删 DB 记录 → 事务外删向量（失败不阻断仅记
// 日志）→ 删对象存储文件（失败留 storageKey 供人工补偿，仅记日志）。
package knowledgebase

import (
	"context"
	"log/slog"
)

// DeleteService 知识库删除服务。
type DeleteService struct {
	store   Store
	storage ObjectStorage
	logger  *slog.Logger
}

// NewDeleteService 创建删除服务。
func NewDeleteService(store Store, storage ObjectStorage, logger *slog.Logger) *DeleteService {
	return &DeleteService{store: store, storage: storage, logger: logger}
}

// Delete 删除知识库及其向量、对象存储文件与 RAG 会话关联。
// 不存在 → 404「知识库不存在」（Java BusinessException(ErrorCode.NOT_FOUND, ...)）。
func (s *DeleteService) Delete(ctx context.Context, id int64) error {
	storageKey, err := s.store.DeleteKnowledgeBaseRecords(ctx, id)
	if err != nil {
		return err
	}

	// 向量删除失败不阻断（Java deleteByKnowledgeBaseId 内部吞异常仅记日志）。
	if err := s.store.DeleteVectorsByKBID(ctx, id); err != nil {
		s.logger.Error("删除向量数据失败，不阻断删除流程",
			slog.Int64("kbId", id), slog.String("error", err.Error()))
	}

	// 对象存储删除失败留 storageKey 供人工补偿（Java 同文案逐字）。
	if err := s.storage.DeleteKnowledgeBase(ctx, deref(storageKey)); err != nil {
		s.logger.Warn("知识库数据库记录已删除，但对象存储文件清理失败，可后续按storageKey补偿",
			slog.Int64("kbId", id), slog.String("storageKey", deref(storageKey)),
			slog.String("error", err.Error()))
	}

	s.logger.Info("知识库已删除", slog.Int64("id", id))
	return nil
}
