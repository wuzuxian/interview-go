// 知识库出题 Redis Stream：常量、消息字段、消费组适配器与生产者（票 04），与 Java
// AsyncTaskStreamConstants 知识库问题生成段 + QuestionGenStreamProducer/Consumer
// 对齐（key/group 与 Java 共库共存，ADR 0005）。消息只传 kbId/taskId/retryCount，
// 出题参数经 question_gen_config 快照在 DB 中传递（Java getConfig 语义）。
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

// 出题 Stream 常量，与 Java AsyncTaskStreamConstants 逐字对齐。
const (
	QuestionGenStreamKey      = "knowledgebase:question-gen:stream"
	QuestionGenGroupName      = "question-gen-group"
	QuestionGenConsumerPrefix = "question-gen-consumer-"
	QuestionFieldTaskID       = "taskId"
)

// QuestionGenMessage Stream 消息（与向量化消息同构，别名复用消费骨架）。
type QuestionGenMessage = VectorizeMessage

// QuestionGenStream 出题流最小能力集（生产实现为本文件 redis 适配器，
// 测试注入内存 fake——fakeStream 方法集同时满足两个流接口）。
type QuestionGenStream interface {
	EnsureGroup(ctx context.Context) error
	XAddTask(ctx context.Context, values map[string]any) (string, error)
	XAck(ctx context.Context, messageID string) error
	XReadNew(ctx context.Context, consumer string, count int64) ([]QuestionGenMessage, error)
	XClaimPending(ctx context.Context, consumer string, count int64) ([]QuestionGenMessage, error)
}

// questionGenRedisStream *redis.Client 适配器；XADD 按 MAXLEN 1000 近似裁剪
// （Java RedisService.streamAdd trimNonStrict）。
type questionGenRedisStream struct {
	rdb *redis.Client
}

// NewQuestionGenStream 创建出题 Stream 适配器。
func NewQuestionGenStream(rdb *redis.Client) QuestionGenStream {
	return &questionGenRedisStream{rdb: rdb}
}

func (s *questionGenRedisStream) EnsureGroup(ctx context.Context) error {
	err := s.rdb.XGroupCreateMkStream(ctx, QuestionGenStreamKey, QuestionGenGroupName, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

func (s *questionGenRedisStream) XAddTask(ctx context.Context, values map[string]any) (string, error) {
	return s.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: QuestionGenStreamKey,
		MaxLen: StreamMaxLen,
		Approx: true,
		Values: values,
	}).Result()
}

func (s *questionGenRedisStream) XAck(ctx context.Context, messageID string) error {
	return s.rdb.XAck(ctx, QuestionGenStreamKey, QuestionGenGroupName, messageID).Err()
}

func (s *questionGenRedisStream) XReadNew(ctx context.Context, consumer string, count int64) ([]QuestionGenMessage, error) {
	res, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    QuestionGenGroupName,
		Consumer: consumer,
		Streams:  []string{QuestionGenStreamKey, ">"},
		Count:    count,
		Block:    500 * time.Millisecond,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	msgs := []QuestionGenMessage{}
	for _, stream := range res {
		for _, msg := range stream.Messages {
			msgs = append(msgs, QuestionGenMessage{ID: msg.ID, Values: msg.Values})
		}
	}
	return msgs, nil
}

func (s *questionGenRedisStream) XClaimPending(ctx context.Context, consumer string, count int64) ([]QuestionGenMessage, error) {
	claimed, _, err := s.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   QuestionGenStreamKey,
		Group:    QuestionGenGroupName,
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
	msgs := make([]QuestionGenMessage, 0, len(claimed))
	for _, msg := range claimed {
		msgs = append(msgs, QuestionGenMessage{ID: msg.ID, Values: msg.Values})
	}
	return msgs, nil
}

// QuestionGenProducer 出题任务生产者（Java QuestionGenStreamProducer +
// AbstractStreamProducer：投递失败 markFailed → FAILED + 安全文案）。
type QuestionGenProducer struct {
	stream QuestionGenStream
	store  Store
	logger *slog.Logger
}

// NewQuestionGenProducer 创建生产者。
func NewQuestionGenProducer(stream QuestionGenStream, store Store, logger *slog.Logger) *QuestionGenProducer {
	return &QuestionGenProducer{stream: stream, store: store, logger: logger}
}

// SendGenerateTask 投递出题任务（消息字段 kbId/taskId/retryCount，Java buildMessage）；
// 失败时条件写 FAILED（不覆盖 COMPLETED），返回是否投递成功。
func (p *QuestionGenProducer) SendGenerateTask(ctx context.Context, kbID int64, taskID string, retryCount int) bool {
	_, err := p.stream.XAddTask(ctx, map[string]any{
		FieldKBID:           strconv.FormatInt(kbID, 10),
		QuestionFieldTaskID: taskID,
		FieldRetryCount:     strconv.Itoa(retryCount),
	})
	if err == nil {
		p.logger.Info("题目生成任务已发送到Stream", slog.Int64("kbId", kbID), slog.String("taskId", taskID))
		return true
	}
	p.logger.Error("发送题目生成任务失败", slog.Int64("kbId", kbID), slog.String("taskId", taskID),
		slog.String("error", err.Error()))
	// Java onSendFailed：stateService.markFailed。
	if _, ferr := p.store.MarkQuestionGenFailed(ctx, kbID, taskID, time.Now()); ferr != nil {
		p.logger.Error("投递失败标记 FAILED 失败", slog.Int64("kbId", kbID), slog.String("error", ferr.Error()))
	}
	return false
}

// buildQuestionGenConsumerName 消费者名：question-gen-consumer- + 随机 8 位
// （Java consumerPrefix() + UUID 前 8 位）。
func buildQuestionGenConsumerName() string {
	return QuestionGenConsumerPrefix + uuid.NewString()[:8]
}
