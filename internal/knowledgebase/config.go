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
