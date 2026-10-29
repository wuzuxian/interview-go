// 知识库向量化 Redis Stream：常量、消息字段、消费组与生产者，与 Java
// AsyncTaskStreamConstants 知识库向量化段 + VectorizeStreamProducer +
// AbstractStreamProducer 逐字对齐（key/group 与 Java 共库共存，ADR 0005）。
// Redis 适配器与消费循环骨架在 stream_skeleton.go 共享（出题流同形复用）。
package knowledgebase

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Stream 常量，与 Java AsyncTaskStreamConstants 逐字对齐。
const (
	StreamKey        = "knowledgebase:vectorize:stream"
	GroupName        = "vectorize-group"
	ConsumerPrefix   = "vectorize-consumer-"
	FieldKBID        = "kbId"
	FieldRetryCount  = "retryCount"
	MaxRetryCount    = 3
	ConsumeBatch     = 10
	PollInterval     = 1 * time.Second
	PendingIdleTime  = 5 * time.Minute
	PendingClaimSize = 10
	StreamMaxLen     = 1000
)

// taskDisplayName 任务显示名（Java taskDisplayName()，FAILED 文案前缀逐字）。
const taskDisplayName = "向量化"

// VectorizeStream 向量化流最小能力集（生产实现为 stream_skeleton.go 的共用
// redis 适配器，测试注入内存 fake，先例 internal/voiceinterview/evaluation_consumer.go）。
type VectorizeStream interface {
	EnsureGroup(ctx context.Context) error
	XAddTask(ctx context.Context, values map[string]any) (string, error)
	XAck(ctx context.Context, messageID string) error
	XReadNew(ctx context.Context, consumer string, count int64) ([]VectorizeMessage, error)
	XClaimPending(ctx context.Context, consumer string, count int64) ([]VectorizeMessage, error)
}

// VectorizeMessage Stream 消息（ID + 字段表）。
type VectorizeMessage struct {
	ID     string
	Values map[string]any
}

// NewRedisStream 创建向量化 Stream 适配器。
func NewRedisStream(rdb *redis.Client) VectorizeStream {
	return newRedisStreamAdapter(rdb, StreamKey, GroupName)
}

// VectorizeProducer 向量化任务生产者（Java VectorizeStreamProducer + AbstractStreamProducer）。
type VectorizeProducer struct {
	stream VectorizeStream
	store  Store
	logger *slog.Logger
}

// NewVectorizeProducer 创建生产者。
func NewVectorizeProducer(stream VectorizeStream, store Store, logger *slog.Logger) *VectorizeProducer {
	return &VectorizeProducer{stream: stream, store: store, logger: logger}
}

// SendVectorizeTask 投递向量化任务（消息只传 kbId + retryCount=0）；
// 失败时 PENDING → FAILED（条件更新，不覆盖终态），返回是否投递成功。
func (p *VectorizeProducer) SendVectorizeTask(ctx context.Context, kbID int64) bool {
	_, err := p.stream.XAddTask(ctx, map[string]any{
		FieldKBID:       strconv.FormatInt(kbID, 10),
		FieldRetryCount: "0",
	})
	if err == nil {
		p.logger.Info("向量化任务已发送到Stream", slog.Int64("kbId", kbID))
		return true
	}
	p.logger.Error("发送向量化任务失败", slog.Int64("kbId", kbID), slog.String("error", err.Error()))
	// Java onSendFailed：任务入队失败（常量文案，条件更新仅清 PENDING）。
	if _, ferr := p.store.FailVectorIfPending(ctx, kbID, "任务入队失败", time.Now()); ferr != nil {
		p.logger.Error("投递失败标记 FAILED 失败", slog.Int64("kbId", kbID), slog.String("error", ferr.Error()))
	}
	return false
}

// buildVectorizeConsumerName 消费者名：vectorize-consumer- + 随机 8 位。
func buildVectorizeConsumerName() string {
	return ConsumerPrefix + uuid.NewString()[:8]
}
