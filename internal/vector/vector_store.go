// Package vector pgvector 向量存储：将简历分析完成后的文本向量化并存入 vector_store，
// 与 Java 版 Spring AI VectorStore 表结构一致（1024 维 cosine HNSW）。
package vector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"

	einoembedding "github.com/cloudwego/eino-ext/components/embedding/openai"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/strutil"
)

// Store pgvector 存储访问。
type Store struct {
	db         *gorm.DB
	embedder   func(ctx context.Context) (*einoembedding.Embedder, error)
	dimensions int
	logger     *slog.Logger
}

// NewStore 创建向量存储；embedder 为延迟构建器（Provider 可能尚未配置）。
func NewStore(db *gorm.DB, embedder func(ctx context.Context) (*einoembedding.Embedder, error), dimensions int, logger *slog.Logger) *Store {
	return &Store{db: db, embedder: embedder, dimensions: dimensions, logger: logger}
}

// vectorStoreRow 对应表 vector_store。
type vectorStoreRow struct {
	ID        uuid.UUID       `gorm:"column:id;primaryKey"`
	Content   *string         `gorm:"column:content;type:text"`
	Metadata  json.RawMessage `gorm:"column:metadata;type:json"`
	Embedding pgvector.Vector `gorm:"column:embedding;type:vector(1024)"`
}

func (vectorStoreRow) TableName() string { return "vector_store" }

// EnsureSchema 确保 pgvector 扩展与 vector_store 表存在（幂等）。
func (s *Store) EnsureSchema(ctx context.Context) error {
	statements := []string{
		`CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public`,
		`CREATE EXTENSION IF NOT EXISTS "uuid-ossp" WITH SCHEMA public`,
	}
	for _, stmt := range statements {
		if err := s.db.WithContext(ctx).Exec(stmt).Error; err != nil {
			// 已存在时忽略（部分云环境不允许非超级用户建扩展）。
			s.logger.Warn("pgvector 扩展创建跳过", slog.String("error", err.Error()))
		}
	}
	createTable := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS vector_store (
		id UUID DEFAULT public.uuid_generate_v4() NOT NULL PRIMARY KEY,
		content TEXT,
		metadata JSON,
		embedding public.vector(%d)
	)`, s.dimensions)
	if err := s.db.WithContext(ctx).Exec(createTable).Error; err != nil {
		return err
	}
	idx := "CREATE INDEX IF NOT EXISTS spring_ai_vector_index ON vector_store USING hnsw (embedding public.vector_cosine_ops)"
	return s.db.WithContext(ctx).Exec(idx).Error
}

// maxEmbeddingChars 单条向量化文本的字符上限：
// text-embedding-v3 单文本上限 8192 token，超长输入既容易超时也可能被服务端拒绝；
// 向量仅用于语义检索，截断不影响召回主体内容。
const maxEmbeddingChars = 6000

// runeCount 返回字符串的字符（rune）数。
func runeCount(s string) int { return len([]rune(s)) }

// UpsertResumeEmbedding 将简历文本向量化并写入 vector_store：
// 先删除该简历的旧向量，再写入新向量（幂等）。
func (s *Store) UpsertResumeEmbedding(ctx context.Context, resumeID int64, content string) error {
	emb, err := s.embedder(ctx)
	if err != nil {
		return err
	}
	if runeCount(content) > maxEmbeddingChars {
		s.logger.Warn("向量化的简历文本过长，已截断",
			slog.Int64("resumeId", resumeID), slog.Int("originLen", runeCount(content)))
		content = strutil.TruncateRunes(content, maxEmbeddingChars)
	}
	vectors, err := emb.EmbedStrings(ctx, []string{content})
	if err != nil {
		return fmt.Errorf("vector: 向量化失败: %w", err)
	}
	if len(vectors) == 0 {
		return fmt.Errorf("vector: 向量化返回空结果")
	}

	f32 := make([]float32, len(vectors[0]))
	for i, v := range vectors[0] {
		f32[i] = float32(v)
	}
	vec := pgvector.NewVector(f32)
	meta, _ := json.Marshal(map[string]any{"resume_id": resumeID, "source": "resume"})

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`DELETE FROM vector_store WHERE metadata->>'resume_id' = ? AND metadata->>'source' = 'resume'`,
			fmt.Sprintf("%d", resumeID)).Error; err != nil {
			return err
		}
		row := vectorStoreRow{
			ID:        uuid.New(),
			Content:   &content,
			Metadata:  meta,
			Embedding: vec,
		}
		return tx.Create(&row).Error
	})
}

// DeleteResumeEmbedding 删除指定简历的全部向量。
func (s *Store) DeleteResumeEmbedding(ctx context.Context, resumeID int64) error {
	return s.db.WithContext(ctx).Exec(
		`DELETE FROM vector_store WHERE metadata->>'resume_id' = ? AND metadata->>'source' = 'resume'`,
		fmt.Sprintf("%d", resumeID)).Error
}

// AddDocuments 批量将文档内容向量化并按 metadata 写入 vector_store
// （对应 Java VectorStore.add 的通用能力；调用方控制批大小，DashScope 上限 10）。
// metadata 为每行独立的 JSON 键值对（键名协议见 ADR 0005）；
// 实现 knowledgebase.VectorWriter 接缝。
func (s *Store) AddDocuments(ctx context.Context, contents []string, metadatas []map[string]any) error {
	if len(contents) != len(metadatas) {
		return fmt.Errorf("vector: 内容与 metadata 数量不一致: %d != %d", len(contents), len(metadatas))
	}
	if len(contents) == 0 {
		return nil
	}
	emb, err := s.embedder(ctx)
	if err != nil {
		return err
	}
	vectors, err := emb.EmbedStrings(ctx, contents)
	if err != nil {
		return fmt.Errorf("vector: 向量化失败: %w", err)
	}
	if len(vectors) != len(contents) {
		return fmt.Errorf("vector: 向量化返回数量不一致: %d != %d", len(vectors), len(contents))
	}
	rows := make([]vectorStoreRow, 0, len(contents))
	for i, content := range contents {
		f32 := make([]float32, len(vectors[i]))
		for j, v := range vectors[i] {
			f32[j] = float32(v)
		}
		meta, err := json.Marshal(metadatas[i])
		if err != nil {
			return fmt.Errorf("vector: metadata 序列化失败: %w", err)
		}
		rows = append(rows, vectorStoreRow{
			ID:        uuid.New(),
			Content:   &content,
			Metadata:  meta,
			Embedding: pgvector.NewVector(f32),
		})
	}
	return s.db.WithContext(ctx).Create(&rows).Error
}

// SearchSimilarResumes 以向量相似度检索简历（预留能力，供后续语义去重/推荐）。
func (s *Store) SearchSimilarResumes(ctx context.Context, query string, topK int) ([]SearchHit, error) {
	emb, err := s.embedder(ctx)
	if err != nil {
		return nil, err
	}
	vectors, err := emb.EmbedStrings(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	f32 := make([]float32, len(vectors[0]))
	for i, v := range vectors[0] {
		f32[i] = float32(v)
	}
	vec := pgvector.NewVector(f32)

	var rows []struct {
		ID       uuid.UUID       `gorm:"column:id"`
		Content  *string         `gorm:"column:content"`
		Metadata json.RawMessage `gorm:"column:metadata"`
		Distance float64         `gorm:"column:distance"`
	}
	err = s.db.WithContext(ctx).Raw(`
		SELECT id, content, metadata, embedding <=> ? AS distance
		FROM vector_store
		WHERE metadata->>'source' = 'resume'
		ORDER BY embedding <=> ?
		LIMIT ?`, vec, vec, topK).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	hits := make([]SearchHit, 0, len(rows))
	for _, r := range rows {
		hits = append(hits, SearchHit{ID: r.ID, Content: derefStr(r.Content), Distance: r.Distance})
	}
	return hits, nil
}

// SearchHit 相似度检索结果。
type SearchHit struct {
	ID       uuid.UUID
	Content  string
	Distance float64
}

// KBDoc 知识库相似度检索结果（Java Spring AI Document 视图：id/content/metadata/score）。
type KBDoc struct {
	ID       string
	Content  string
	Metadata map[string]any
	Score    *float64
}

// SimilaritySearch 知识库向量检索（Java KnowledgeBaseVectorService.similaritySearch）：
// 查询文本向量化后按 metadata kb_id IN 过滤优先下推 SQL（minScore>0 时附加相似度
// 阈值，score = 1 - cosine distance）；前置过滤失败回退本地过滤（topK×3 放大内存
// 过滤，阈值保留），回退再失败按 Java 语义抛 6004「向量搜索失败」。
func (s *Store) SimilaritySearch(ctx context.Context, query string, kbIDs []int64, topK int, minScore float64) ([]KBDoc, error) {
	if topK < 1 {
		topK = 1
	}
	docs, err := s.searchKBWithFilter(ctx, query, kbIDs, topK, minScore)
	if err != nil {
		s.logger.Warn("向量搜索前置过滤失败，回退到本地过滤", slog.String("error", err.Error()))
		return s.searchKBFallback(ctx, query, kbIDs, topK, minScore)
	}
	return docs, nil
}

// searchKBWithFilter 前置过滤路径：kb_id IN 下推 SQL（kb_id metadata 为字符串键，
// 与 Java Filter "kb_id in ['1', '2']" 语义一致）。
func (s *Store) searchKBWithFilter(ctx context.Context, query string, kbIDs []int64, topK int, minScore float64) ([]KBDoc, error) {
	vectors, err := s.embedQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	sql, args := kbSearchSQL(vectors, kbIDs, topK, minScore)
	return s.scanKBDocs(ctx, sql, args...)
}

// searchKBFallback 回退路径：放大 3 倍 topK 不带 kb 过滤检索，再按 metadata
// kb_id 内存过滤、截断 topK（Java similaritySearchFallback；失败抛 6004）。
func (s *Store) searchKBFallback(ctx context.Context, query string, kbIDs []int64, topK int, minScore float64) ([]KBDoc, error) {
	amplified := topK * 3
	if amplified < topK {
		amplified = topK
	}
	vectors, err := s.embedQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	sql, args := kbSearchSQL(vectors, nil, amplified, minScore)
	docs, err := s.scanKBDocs(ctx, sql, args...)
	if err != nil {
		return nil, apperr.New(apperr.CodeKnowledgeBaseQueryFailed, "向量搜索失败")
	}
	docs = filterKBDocsLocal(docs, kbIDs, topK)
	return docs, nil
}

// filterKBDocsLocal 回退路径的内存 kb 过滤 + topK 截断
// （Java similaritySearchFallback 的 stream filter + limit）。
func filterKBDocsLocal(docs []KBDoc, kbIDs []int64, topK int) []KBDoc {
	if len(kbIDs) > 0 {
		allowed := make(map[int64]bool, len(kbIDs))
		for _, id := range kbIDs {
			allowed[id] = true
		}
		filtered := docs[:0]
		for _, doc := range docs {
			if kbIDInMetadata(doc.Metadata["kb_id"], allowed) {
				filtered = append(filtered, doc)
			}
		}
		docs = filtered
	}
	if len(docs) > topK {
		docs = docs[:topK]
	}
	return docs
}

// kbSearchSQL 组装向量检索 SQL 与完整参数表（参数序与占位符文本序严格一致——
// 向量按值传入，杜绝调用方前置参数导致的位置错位）；minScore>0 时附加阈值条件，
// kbIDs 为空不加 kb 过滤（Java 仅在 minScore>0 时设置 similarityThreshold）。
func kbSearchSQL(vec pgvector.Vector, kbIDs []int64, topK int, minScore float64) (string, []any) {
	if topK < 1 {
		topK = 1
	}
	var b strings.Builder
	b.WriteString("SELECT id, content, metadata, 1 - (embedding <=> ?) AS score ")
	b.WriteString("FROM vector_store WHERE 1=1")
	args := []any{vec}
	if len(kbIDs) > 0 {
		quoted := make([]string, 0, len(kbIDs))
		for _, id := range kbIDs {
			quoted = append(quoted, "'"+strconv.FormatInt(id, 10)+"'")
		}
		b.WriteString(" AND metadata->>'kb_id' IN (" + strings.Join(quoted, ", ") + ")")
	}
	if minScore > 0 {
		b.WriteString(" AND 1 - (embedding <=> ?) >= ?")
		args = append(args, vec, minScore)
	}
	b.WriteString(" ORDER BY embedding <=> ? LIMIT ?")
	args = append(args, vec, topK)
	return b.String(), args
}

// embedQuery 查询文本向量化（每次检索独立 embed，回退路径重embed 与 Java
// 二次调用 vectorStore.similaritySearch 一致）。
func (s *Store) embedQuery(ctx context.Context, query string) (pgvector.Vector, error) {
	emb, err := s.embedder(ctx)
	if err != nil {
		return pgvector.Vector{}, err
	}
	vectors, err := emb.EmbedStrings(ctx, []string{query})
	if err != nil {
		return pgvector.Vector{}, fmt.Errorf("vector: 查询向量化失败: %w", err)
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return pgvector.Vector{}, fmt.Errorf("vector: 查询向量化返回空结果")
	}
	f32 := make([]float32, len(vectors[0]))
	for i, v := range vectors[0] {
		f32[i] = float32(v)
	}
	return pgvector.NewVector(f32), nil
}

// scanKBDocs 执行检索 SQL 并映射结果。
func (s *Store) scanKBDocs(ctx context.Context, sql string, args ...any) ([]KBDoc, error) {
	var rows []struct {
		ID       uuid.UUID       `gorm:"column:id"`
		Content  *string         `gorm:"column:content"`
		Metadata json.RawMessage `gorm:"column:metadata"`
		Score    *float64        `gorm:"column:score"`
	}
	if err := s.db.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	docs := make([]KBDoc, 0, len(rows))
	for _, r := range rows {
		meta := map[string]any{}
		if len(r.Metadata) > 0 {
			if err := json.Unmarshal(r.Metadata, &meta); err != nil {
				return nil, fmt.Errorf("vector: metadata 解析失败: %w", err)
			}
		}
		docs = append(docs, KBDoc{ID: r.ID.String(), Content: derefStr(r.Content), Metadata: meta, Score: r.Score})
	}
	return docs, nil
}

// kbIDInMetadata metadata kb_id 归一为 int64 后判断（Java isDocInKnowledgeBases：
// Long 或可解析字符串，解析失败视为不在集合内）。
func kbIDInMetadata(value any, allowed map[int64]bool) bool {
	switch v := value.(type) {
	case string:
		id, err := strconv.ParseInt(v, 10, 64)
		return err == nil && allowed[id]
	case float64:
		return allowed[int64(v)]
	case int:
		return allowed[int64(v)]
	case int32:
		return allowed[int64(v)]
	case int64:
		return allowed[v]
	default:
		return false
	}
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
