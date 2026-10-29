// 知识库向量化服务：分块 → metadata 组装 → 分批向量化写入 → 两阶段 promote。
// 对应 Java KnowledgeBaseVectorService.vectorizeAndStore（ADR 0005 协议逐字对齐）：
// 临时 kb_id = pending:<kbId>:<jobId>，成功在数据库锁内先删旧正式向量再 promote
// 并写 chunk_count / vector_config 快照；失败按 jobId 清理临时向量，检索永不命中半成品。
package knowledgebase

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"log/slog"
	"strconv"

	"github.com/google/uuid"

	"github.com/cloudwego/eino/schema"
	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/knowledgebase/textsplit"
)

// 向量 metadata 键与常量，与 Java KnowledgeBaseVectorService 逐字对齐。
const (
	tempKbIDPrefix        = "pending:"
	metadataKBID          = "kb_id"
	metadataTarget        = "kb_target_id"
	metadataJobID         = "kb_vector_job_id"
	metadataChunkStrategy = "chunk_strategy"
)

// SourceInfo 向量化来源信息（Java KnowledgeBaseVectorService.SourceInfo）。
type SourceInfo struct {
	Name     string
	Filename string
	FileHash string
}

// VectorWriter 向量写入接缝（生产实现 *vector.Store，测试注入 fake）。
type VectorWriter interface {
	// AddDocuments 批量将内容向量化并按 metadata 写入 vector_store（批 ≤10）。
	AddDocuments(ctx context.Context, contents []string, metadatas []map[string]any) error
}

// VectorizeService 向量化服务。
type VectorizeService struct {
	splitter  *textsplit.TokenTextSplitter
	cfg       textsplit.Config
	batchSize int
	store     Store
	vectors   VectorWriter
	logger    *slog.Logger
}

// NewVectorizeService 创建向量化服务；cfg 为切分策略配置（vector_config 快照来源）。
func NewVectorizeService(splitter *textsplit.TokenTextSplitter, cfg textsplit.Config, batchSize int, store Store, vectors VectorWriter, logger *slog.Logger) *VectorizeService {
	if batchSize <= 0 {
		batchSize = 10
	}
	return &VectorizeService{splitter: splitter, cfg: cfg, batchSize: batchSize, store: store, vectors: vectors, logger: logger}
}

// VectorizeAndStore 将知识库文本向量化并存储；heartbeat 在分块完成与每个
// Embedding 批次后回调（由消费方节流，返回错误表示执行权失效，立即中断）。
// attemptID 为执行代次，promote 仅在数据库锁内校验代次仍有效后执行。
func (s *VectorizeService) VectorizeAndStore(ctx context.Context, kbID int64, content string, attemptID string, heartbeat func() error, source SourceInfo) error {
	jobID := uuid.NewString()
	s.logger.Info("开始向量化知识库",
		slog.Int64("kbId", kbID), slog.String("jobId", jobID),
		slog.Int("contentLength", len([]rune(content))))

	if err := s.vectorize(ctx, kbID, content, attemptID, jobID, heartbeat, source); err != nil {
		// 失败按 jobId 清理临时向量（清理失败仅记录，不覆盖原始错误）。
		if cerr := s.store.DeleteVectorsByJobID(ctx, jobID); cerr != nil {
			s.logger.Warn("清理临时向量数据失败，可后续按 jobId 补偿",
				slog.Int64("kbId", kbID), slog.String("jobId", jobID), slog.String("error", cerr.Error()))
		}
		s.logger.Error("向量化知识库失败",
			slog.Int64("kbId", kbID), slog.String("jobId", jobID), slog.String("error", err.Error()))
		return apperr.New(apperr.CodeKnowledgeBaseVectorizationFailed, "向量化知识库失败")
	}
	return nil
}

func (s *VectorizeService) vectorize(ctx context.Context, kbID int64, content string, attemptID, jobID string, heartbeat func() error, source SourceInfo) error {
	configJSON, err := s.cfg.ConfigJSON()
	if err != nil {
		return apperr.New(apperr.CodeInternalError, "向量化配置快照序列化失败")
	}
	// 文件哈希只用于可复现标识；缺失时以内容 md5 兜底（Java 同语义）。
	fileHash := source.FileHash
	if fileHash == "" {
		fileHash = md5Hex([]byte(content))
	}

	// 1. 分块：metadata 预置 source_*，切块继承后再补 chunk_index/total_chunks/
	//    source_id 与临时 kb_id（Java 顺序逐字对齐）。
	metadata := map[string]any{
		"source_kb_id":        strconv.FormatInt(kbID, 10),
		"source_name":         source.Name,
		"source_filename":     source.Filename,
		metadataChunkStrategy: s.cfg.StrategyVersion,
	}
	chunks, err := s.splitter.Transform(ctx, einoDoc(content, metadata))
	if err != nil {
		return err
	}
	pendingKbID := tempKbIDPrefix + strconv.FormatInt(kbID, 10) + ":" + jobID
	contents := make([]string, 0, len(chunks))
	metadatas := make([]map[string]any, 0, len(chunks))
	for i, chunk := range chunks {
		meta := chunk.MetaData
		meta["chunk_index"] = i
		meta["total_chunks"] = len(chunks)
		meta["source_id"] = md5Hex([]byte(fileHash + "\n" + configJSON + "\n" + strconv.Itoa(i)))
		// add 前写入临时键；promote 只改 kb_id，不会回填。
		meta[metadataKBID] = pendingKbID
		meta[metadataTarget] = strconv.FormatInt(kbID, 10)
		meta[metadataJobID] = jobID
		meta[metadataChunkStrategy] = s.cfg.StrategyVersion
		contents = append(contents, chunk.Content)
		metadatas = append(metadatas, meta)
	}
	s.logger.Info("文本分块完成", slog.Int("chunks", len(chunks)))

	// 分块完成心跳。
	if err := heartbeat(); err != nil {
		return err
	}

	// 2. 分批向量化并存储（DashScope batch ≤ 10）。
	total := len(chunks)
	batchCount := (total + s.batchSize - 1) / s.batchSize
	s.logger.Info("开始分批向量化", slog.Int("chunks", total), slog.Int("batches", batchCount), slog.Int("batchSize", s.batchSize))
	for i := 0; i < batchCount; i++ {
		start := i * s.batchSize
		end := start + s.batchSize
		if end > total {
			end = total
		}
		if err := s.vectors.AddDocuments(ctx, contents[start:end], metadatas[start:end]); err != nil {
			return err
		}
		// Embedding 批次心跳（调用方节流）。
		if err := heartbeat(); err != nil {
			return err
		}
	}

	// 3. 成功：数据库锁内先删旧正式向量再 promote，并写配置快照。
	if err := s.store.ActivateVectorJobAndUpdateSnapshot(ctx, kbID, attemptID, jobID, total, configJSON); err != nil {
		return err
	}
	s.logger.Info("知识库向量化完成",
		slog.Int64("kbId", kbID), slog.String("jobId", jobID),
		slog.Int("chunks", total), slog.Int("batches", batchCount))
	return nil
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// einoDoc 组装切分输入（Java new Document(content, metadata)）。
func einoDoc(content string, metadata map[string]any) []*schema.Document {
	return []*schema.Document{{Content: content, MetaData: metadata}}
}
