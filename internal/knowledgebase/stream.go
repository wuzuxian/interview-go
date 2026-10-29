// 知识库向量化 Redis Stream：常量、消息字段、消费组与生产者，与 Java
// AsyncTaskStreamConstants 知识库向量化段 + VectorizeStreamProducer/Consumer +
// AbstractStreamProducer/Consumer 逐字对齐（key/group 与 Java 共库共存，ADR 0005）。
package knowledgebase

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
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

// VectorizeStream 向量化流最小能力集（生产实现为本文件 redis 适配器，
// 测试注入内存 fake，先例 internal/voiceinterview/evaluation_consumer.go）。
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

// redisStream *redis.Client 适配器；XADD 按 MAXLEN 1000 近似裁剪
// （Java RedisService.streamAdd trimNonStrict）。
type redisStream struct {
	rdb *redis.Client
}

// NewRedisStream 创建 Stream 适配器。
func NewRedisStream(rdb *redis.Client) VectorizeStream {
	return &redisStream{rdb: rdb}
}

func (s *redisStream) EnsureGroup(ctx context.Context) error {
	err := s.rdb.XGroupCreateMkStream(ctx, StreamKey, GroupName, "0").Err()
	if err != nil && !isBusyGroup(err) {
		return err
	}
	return nil
}

func isBusyGroup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "BUSYGROUP")
}

func (s *redisStream) XAddTask(ctx context.Context, values map[string]any) (string, error) {
	return s.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamKey,
		MaxLen: StreamMaxLen,
		Approx: true,
		Values: values,
	}).Result()
}

func (s *redisStream) XAck(ctx context.Context, messageID string) error {
	return s.rdb.XAck(ctx, StreamKey, GroupName, messageID).Err()
}

func (s *redisStream) XReadNew(ctx context.Context, consumer string, count int64) ([]VectorizeMessage, error) {
	res, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    GroupName,
		Consumer: consumer,
		Streams:  []string{StreamKey, ">"},
		Count:    count,
		Block:    500 * time.Millisecond,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	msgs := []VectorizeMessage{}
	for _, stream := range res {
		for _, msg := range stream.Messages {
			msgs = append(msgs, VectorizeMessage{ID: msg.ID, Values: msg.Values})
		}
	}
	return msgs, nil
}

func (s *redisStream) XClaimPending(ctx context.Context, consumer string, count int64) ([]VectorizeMessage, error) {
	claimed, _, err := s.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   StreamKey,
		Group:    GroupName,
		Consumer: consumer,
		MinIdle:  PendingIdleTime,
		Start:    "0-0",
		Count:    count,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	msgs := make([]VectorizeMessage, 0, len(claimed))
	for _, msg := range claimed {
		msgs = append(msgs, VectorizeMessage{ID: msg.ID, Values: msg.Values})
	}
	return msgs, nil
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
