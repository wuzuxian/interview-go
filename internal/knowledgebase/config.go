// 知识库模块配置（app.knowledgebase 段），键与 Java 版 application.yml 对齐。
package knowledgebase

import (
	"time"

	"github.com/wuzuxian/interview/internal/knowledgebase/textsplit"
)

// Config 模块配置，default 标签值与 Java application.yml 一致。
type Config struct {
	// MaxFileSize 上传文件大小上限（Java MAX_FILE_SIZE = 50MB）。
	MaxFileSize int64 `config:"max_file_size" default:"52428800"`
	// MaxConcurrent 单实例同步处理的上传并发上限（Java knowledge-base-upload.max-concurrent=4）。
	MaxConcurrent int `config:"max_concurrent" default:"4"`
	// ParseTimeout 单文件解析超时（Java document-parse.timeout=2m）。
	ParseTimeout time.Duration `config:"parse_timeout" default:"2m"`
	// MaxTextBytes 提取文本上限（Java BodyContentHandler 5MB）。
	MaxTextBytes int `config:"max_text_bytes" default:"5242880"`
	// EmbeddingBatch Embedding 分批大小（DashScope 限制 10，Java MAX_BATCH_SIZE）。
	EmbeddingBatch int `config:"embedding_batch" default:"10"`
	// HeartbeatThrottle Embedding 阶段心跳节流（Java heartbeat-throttle=30s）。
	HeartbeatThrottle time.Duration `config:"heartbeat_throttle" default:"30s"`
	// Vectorization 切分策略（Java app.ai.rag.vectorization.*）。
	Vectorization VectorizationConfig `config:"vectorization"`
	// Query RAG 查询管线参数（Java app.ai.rag.* 的 rewrite/search/history 段）。
	Query RagQueryConfig `config:"query"`
}

// RagQueryConfig RAG 查询参数（Java KnowledgeBaseQueryProperties）。
type RagQueryConfig struct {
	// RewriteEnabled 问题改写开关（Java app.ai.rag.rewrite.enabled，默认开）。
	RewriteEnabled bool `config:"rewrite_enabled" default:"true"`
	// MergeOriginalQuery 双路召回融合开关（Java search.merge-original-query，默认关）。
	MergeOriginalQuery bool `config:"merge_original_query" default:"false"`
	// ShortQueryLength 短问题长度档（字符数，Java search.short-query-length=4）。
	ShortQueryLength int `config:"short_query_length" default:"4"`
	// TopkShort 短问题检索档 topK（Java search.topk-short=20）。
	TopkShort int `config:"topk_short" default:"20"`
	// TopkMedium 中等问题检索档 topK（Java search.topk-medium=12）。
	TopkMedium int `config:"topk_medium" default:"12"`
	// TopkLong 长问题检索档 topK（Java search.topk-long=8）。
	TopkLong int `config:"topk_long" default:"8"`
	// MinScoreShort 短问题相似度阈值。取 Java application.yml 运行值 0.18
	// （Properties 代码默认 0.25 被 yml 覆盖，票面要求以 yml 核对为准）。
	MinScoreShort float64 `config:"min_score_short" default:"0.18"`
	// MinScoreDefault 中/长问题相似度阈值（Java search.min-score-default=0.28）。
	MinScoreDefault float64 `config:"min_score_default" default:"0.28"`
	// HistoryEnabled 多轮历史开关（Java app.ai.rag.history.enabled，默认开）。
	HistoryEnabled bool `config:"history_enabled" default:"true"`
	// HistoryMaxMessages 历史消息条数上限（Java history.max-messages=10）。
	HistoryMaxMessages int `config:"history_max_messages" default:"10"`
}

// VectorizationConfig TokenTextSplitter 策略参数（Java KnowledgeBaseVectorProperties）。
type VectorizationConfig struct {
	Splitter              string `config:"splitter" default:"token"`
	ChunkSize             int    `config:"chunk_size" default:"800"`
	MinChunkSizeChars     int    `config:"min_chunk_size_chars" default:"350"`
	MinChunkLengthToEmbed int    `config:"min_chunk_length_to_embed" default:"5"`
	MaxNumChunks          int    `config:"max_num_chunks" default:"10000"`
	KeepSeparator         bool   `config:"keep_separator" default:"true"`
	// PunctuationMarks 句末标点集；空时取 textsplit.DefaultConfig 的默认集合。
	PunctuationMarks []string `config:"punctuation_marks"`
	// StrategyVersion 策略版本标识，写入向量 metadata 与 vector_config 快照。
	StrategyVersion string `config:"strategy_version" default:"token-v1"`
}

// RecoveryConfig 卡住任务恢复配置（Java app.async.recovery.vectorize）；
// main 接线取自仓库共享的 app.async.recovery 段（Java 默认值一致）。
type RecoveryConfig struct {
	Enabled             bool          `config:"enabled" default:"true"`
	PendingThreshold    time.Duration `config:"pending_threshold" default:"10m"`
	ProcessingThreshold time.Duration `config:"processing_threshold" default:"15m"`
	Interval            time.Duration `config:"interval" default:"1m"`
	BatchSize           int           `config:"batch_size" default:"10"`
	MaxRecoveryCount    int           `config:"max_recovery_count" default:"3"`
}

// SplitterConfig 转换为切分器配置（textsplit.Config）；标点集为空时取默认集合，
// 标量零值同样回退默认（测试直接构造 Config 时不依赖配置库的 default 标签）。
func (c Config) SplitterConfig() textsplit.Config {
	out := textsplit.DefaultConfig()
	v := c.Vectorization
	if v.Splitter != "" {
		out.Splitter = v.Splitter
	}
	if v.ChunkSize > 0 {
		out.ChunkSize = v.ChunkSize
	}
	if v.MinChunkSizeChars > 0 {
		out.MinChunkSizeChars = v.MinChunkSizeChars
	}
	if v.MinChunkLengthToEmbed > 0 {
		out.MinChunkLengthToEmbed = v.MinChunkLengthToEmbed
	}
	if v.MaxNumChunks > 0 {
		out.MaxNumChunks = v.MaxNumChunks
	}
	out.KeepSeparator = v.KeepSeparator
	if len(v.PunctuationMarks) > 0 {
		out.PunctuationMarks = v.PunctuationMarks
	}
	if v.StrategyVersion != "" {
		out.StrategyVersion = v.StrategyVersion
	}
	return out
}
