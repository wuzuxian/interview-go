// 知识库出题 Redis Stream：常量、消息字段与生产者（票 04），与 Java
// AsyncTaskStreamConstants 知识库问题生成段 + QuestionGenStreamProducer 对齐
// （key/group 与 Java 共库共存，ADR 0005）。消息只传 kbId/taskId/retryCount，
// 出题参数经 question_gen_config 快照在 DB 中传递（Java getConfig 语义）。
// Redis 适配器与消费循环骨架在 stream_skeleton.go 共享（向量化流同形复用）。
package knowledgebase

import (
	"context"
	"log/slog"
	"strconv"
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

// QuestionGenStream 出题流最小能力集（生产实现为 stream_skeleton.go 的共用
// redis 适配器，测试注入内存 fake——fakeStream 方法集同时满足两个流接口）。
type QuestionGenStream interface {
	EnsureGroup(ctx context.Context) error
	XAddTask(ctx context.Context, values map[string]any) (string, error)
	XAck(ctx context.Context, messageID string) error
	XReadNew(ctx context.Context, consumer string, count int64) ([]QuestionGenMessage, error)
	XClaimPending(ctx context.Context, consumer string, count int64) ([]QuestionGenMessage, error)
}

// NewQuestionGenStream 创建出题 Stream 适配器。
func NewQuestionGenStream(rdb *redis.Client) QuestionGenStream {
	return newRedisStreamAdapter(rdb, QuestionGenStreamKey, QuestionGenGroupName)
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
