// 知识库管理查询服务：列表（状态过滤 + 内存排序）、详情、分类、搜索、统计、下载。
// 对应 Java KnowledgeBaseListService 逐字对齐：
//   - 详情不存在 → Result.error("知识库不存在")（Java Result.error(String) 固定 500）；
//   - 下载不存在 → BusinessException(6001)；storageKey 缺失 → 4002「文件存储信息不存在」；
//   - 统计总提问次数取 RAG 会话 USER 消息数（多知识库提问只算一次）。
package knowledgebase

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/wuzuxian/interview/internal/apperr"
)

// ListService 知识库列表/分类/搜索/统计/下载服务。
type ListService struct {
	store   Store
	storage ObjectStorage
	logger  *slog.Logger
}

// NewListService 创建管理查询服务。
func NewListService(store Store, storage ObjectStorage, logger *slog.Logger) *ListService {
	return &ListService{store: store, storage: storage, logger: logger}
}

// List 知识库列表：vectorStatus 过滤 + sortBy 排序（Java listKnowledgeBases）。
// sortBy null/空白/"time"（忽略大小写）为时间序（数据库 uploaded_at DESC），
// 其余取值在内存中排序（Java sortEntities：size/access/question，其余原样返回）。
func (s *ListService) List(ctx context.Context, vectorStatus, sortBy string) ([]KnowledgeBaseListItemDTO, error) {
	var status VectorStatus
	if !isBlank(vectorStatus) {
		parsed := VectorStatus(strings.ToUpper(vectorStatus))
		switch parsed {
		case VectorPending, VectorProcessing, VectorCompleted, VectorFailed:
			status = parsed
		default:
			// Java Result.error("无效的向量化状态: " + vectorStatus)（固定 500）。
			return nil, apperr.New(apperr.CodeInternalError, "无效的向量化状态: "+vectorStatus)
		}
	}
	var entities []KnowledgeBaseEntity
	var err error
	if status != "" {
		entities, err = s.store.FindByVectorStatusOrderByUploadedAtDesc(ctx, status)
	} else {
		entities, err = s.store.FindAllOrderByUploadedAtDesc(ctx)
	}
	if err != nil {
		return nil, err
	}
	entities = sortEntities(entities, sortBy)
	out := make([]KnowledgeBaseListItemDTO, 0, len(entities))
	for i := range entities {
		out = append(out, toListItemDTO(&entities[i]))
	}
	return out, nil
}

// sortEntities 内存排序（Java sortEntities switch：size/access/question 降序，
// 稳定排序保持同值内的 uploaded_at DESC；default 原样返回）。
func sortEntities(entities []KnowledgeBaseEntity, sortBy string) []KnowledgeBaseEntity {
	if isBlank(sortBy) || strings.EqualFold(sortBy, "time") {
		return entities
	}
	switch strings.ToLower(sortBy) {
	case "size":
		sort.SliceStable(entities, func(i, j int) bool {
			return derefI64(entities[i].FileSize) > derefI64(entities[j].FileSize)
		})
	case "access":
		sort.SliceStable(entities, func(i, j int) bool {
			return derefIntKb(entities[i].AccessCount) > derefIntKb(entities[j].AccessCount)
		})
	case "question":
		sort.SliceStable(entities, func(i, j int) bool {
			return derefIntKb(entities[i].QuestionCount) > derefIntKb(entities[j].QuestionCount)
		})
	}
	return entities
}

// Get 知识库详情（Java getKnowledgeBase：不存在 → Result.error("知识库不存在")，
// 即 Result.error(String) 的 SERVER_ERROR 500——契约照抄，勿"修正"为 6001）。
func (s *ListService) Get(ctx context.Context, id int64) (*KnowledgeBaseListItemDTO, error) {
	kb, err := s.store.FindByID(ctx, id)
	if err != nil {
		if apperr.Is(err, apperr.CodeKnowledgeBaseNotFound) {
			return nil, apperr.New(apperr.CodeInternalError, "知识库不存在")
		}
		return nil, err
	}
	dto := toListItemDTO(kb)
	return &dto, nil
}

// Categories 全部分类（去重非空升序，Java getAllCategories）；空库输出 [] 非 null。
func (s *ListService) Categories(ctx context.Context) ([]string, error) {
	cats, err := s.store.FindAllCategories(ctx)
	if err != nil {
		return nil, err
	}
	if cats == nil {
		cats = []string{}
	}
	return cats, nil
}

// ListByCategory 按分类浏览；空白按未分类处理（Java listByCategory 的
// category == null || isBlank 分支）。
func (s *ListService) ListByCategory(ctx context.Context, category string) ([]KnowledgeBaseListItemDTO, error) {
	entities, err := s.listByCategoryEntities(ctx, category)
	if err != nil {
		return nil, err
	}
	return toListItemDTOList(entities), nil
}

func (s *ListService) listByCategoryEntities(ctx context.Context, category string) ([]KnowledgeBaseEntity, error) {
	if isBlank(category) {
		return s.store.FindByCategoryIsNullOrderByUploadedAtDesc(ctx)
	}
	return s.store.FindByCategoryOrderByUploadedAtDesc(ctx, category)
}

// Uncategorized 未分类列表（Java getUncategorized → listByCategory(null)）。
func (s *ListService) Uncategorized(ctx context.Context) ([]KnowledgeBaseListItemDTO, error) {
	entities, err := s.store.FindByCategoryIsNullOrderByUploadedAtDesc(ctx)
	if err != nil {
		return nil, err
	}
	return toListItemDTOList(entities), nil
}

// UpdateCategory 设置分类（Java updateCategory：不存在 → 6001「知识库不存在」；
// blank → NULL，非空白保留原值不做 trim）。
func (s *ListService) UpdateCategory(ctx context.Context, id int64, category string) error {
	var value *string
	if !isBlank(category) {
		value = &category
	}
	if err := s.store.UpdateCategory(ctx, id, value); err != nil {
		return err
	}
	s.logger.Info("更新知识库分类", slog.Int64("id", id), slog.String("category", category))
	return nil
}

// Search 关键词搜索：空白返回全量列表（Java search → listKnowledgeBases()），
// 否则按 trim 后关键词不区分大小写匹配名称/原文件名。
func (s *ListService) Search(ctx context.Context, keyword string) ([]KnowledgeBaseListItemDTO, error) {
	if isBlank(keyword) {
		return s.List(ctx, "", "")
	}
	entities, err := s.store.SearchByKeyword(ctx, strings.TrimSpace(keyword))
	if err != nil {
		return nil, err
	}
	return toListItemDTOList(entities), nil
}

// Stats 统计总览（Java getStatistics：总数、USER 消息数、访问总数、完成数、处理中数）。
func (s *ListService) Stats(ctx context.Context) (*KnowledgeBaseStatsDTO, error) {
	totalCount, err := s.store.CountKnowledgeBases(ctx)
	if err != nil {
		return nil, err
	}
	totalQuestionCount, err := s.store.CountUserRagMessages(ctx)
	if err != nil {
		return nil, err
	}
	totalAccessCount, err := s.store.SumAccessCount(ctx)
	if err != nil {
		return nil, err
	}
	completedCount, err := s.store.CountByVectorStatus(ctx, VectorCompleted)
	if err != nil {
		return nil, err
	}
	processingCount, err := s.store.CountByVectorStatus(ctx, VectorProcessing)
	if err != nil {
		return nil, err
	}
	return &KnowledgeBaseStatsDTO{
		TotalCount:         totalCount,
		TotalQuestionCount: totalQuestionCount,
		TotalAccessCount:   totalAccessCount,
		CompletedCount:     completedCount,
		ProcessingCount:    processingCount,
	}, nil
}

// DownloadResult 下载载荷：原文件名、内容类型与文件字节。
type DownloadResult struct {
	Filename    string
	ContentType string
	Data        []byte
}

// Download 下载原始文件（Java downloadFile + getEntityForDownload）：
// 不存在 → 6001「知识库不存在」；storageKey 缺失 → 4002「文件存储信息不存在」；
// 对象存储失败 → 4002（internal/storage 已产出与 Java 同语义文案）。
func (s *ListService) Download(ctx context.Context, id int64) (*DownloadResult, error) {
	kb, err := s.store.FindByID(ctx, id)
	if err != nil {
		if apperr.Is(err, apperr.CodeKnowledgeBaseNotFound) {
			return nil, apperr.New(apperr.CodeKnowledgeBaseNotFound, "知识库不存在")
		}
		return nil, err
	}
	storageKey := deref(kb.StorageKey)
	if isBlank(storageKey) {
		return nil, apperr.New(apperr.CodeStorageDownloadFail, "文件存储信息不存在")
	}
	s.logger.Info("下载知识库文件",
		slog.Int64("id", id), slog.String("filename", kb.OriginalFilename))
	data, err := s.storage.Download(ctx, storageKey)
	if err != nil {
		return nil, err
	}
	return &DownloadResult{
		Filename:    kb.OriginalFilename,
		ContentType: deref(kb.ContentType),
		Data:        data,
	}, nil
}

// toListItemDTO 实体 → 列表 DTO（Java MapStruct 同名映射；
// questionGenStatus 实体初始化为 NONE，列值为空时同样输出 NONE）。
func toListItemDTO(e *KnowledgeBaseEntity) KnowledgeBaseListItemDTO {
	genStatus := e.QuestionGenStatus
	if genStatus == "" {
		genStatus = QuestionGenNone
	}
	return KnowledgeBaseListItemDTO{
		ID:                e.ID,
		Name:              e.Name,
		Category:          e.Category,
		OriginalFilename:  e.OriginalFilename,
		FileSize:          e.FileSize,
		ContentType:       e.ContentType,
		UploadedAt:        e.UploadedAt,
		LastAccessedAt:    e.LastAccessedAt,
		AccessCount:       e.AccessCount,
		QuestionCount:     e.QuestionCount,
		VectorStatus:      string(e.VectorStatus),
		VectorError:       e.VectorError,
		ChunkCount:        e.ChunkCount,
		QuestionGenStatus: genStatus,
		QuestionGenError:  e.QuestionGenError,
	}
}

// toListItemDTOList 批量映射；空输入输出空数组（与 Java 空列表 JSON 序列化一致）。
func toListItemDTOList(entities []KnowledgeBaseEntity) []KnowledgeBaseListItemDTO {
	out := make([]KnowledgeBaseListItemDTO, 0, len(entities))
	for i := range entities {
		out = append(out, toListItemDTO(&entities[i]))
	}
	return out
}

// derefIntKb 空指针 int 取 0（列表排序对齐 Java 自动拆箱在非空数据上的行为）。
func derefIntKb(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
