// Package vector pgvector 向量存储：将简历分析完成后的文本向量化并存入 vector_store，
// 与 Java 版 Spring AI VectorStore 表结构一致（1024 维 cosine HNSW）。
package vector

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"

	einoembedding "github.com/cloudwego/eino-ext/components/embedding/openai"

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

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
