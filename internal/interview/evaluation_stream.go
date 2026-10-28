// 面试评估 Redis Stream 队列（票 06）：生产者（SessionService 的 EvaluateEnqueuer
// 扩展点）+ 消费者。常量、消息字段、MAXLEN、消费组与重试机制对齐 Java
// AsyncTaskStreamConstants / EvaluateStreamProducer / EvaluateStreamConsumer +
// AbstractStreamConsumer（笔记 06 逐字）；消费基座模式复用 resume 模块 stream.go。
package interview

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// 评估流常量，与 Java 版 AsyncTaskStreamConstants 逐字对齐。
const (
	EvalStreamKey        = "interview:evaluate:stream"
	EvalGroupName        = "evaluate-group"
	EvalConsumerPrefix   = "evaluate-consumer-"
	EvalFieldSessionID   = "sessionId"
	EvalFieldRetryCount  = "retryCount"
	EvalMaxRetryCount    = 3
	EvalConsumeBatch     = 10
	EvalPollInterval     = 1 * time.Second
	EvalPendingIdleTime  = 5 * time.Minute
	EvalPendingClaimSize = 10
	EvalStreamMaxLen     = 1000
)

// evalTaskName 任务显示名（Java taskDisplayName："评估"，失败文案前缀逐字）。
const evalTaskName = "评估"

// EvalStreamMessage Stream 消息（ID + 字段表）。
type EvalStreamMessage struct {
	ID     string
	Values map[string]any
}

// EvalStreamClient 评估流的最小 Stream 能力（生产实现为 *redis.Client 适配器，
// 测试注入内存实现）。consumer 参数为消费组内的消费者名。
type EvalStreamClient interface {
	EnsureGroup(ctx context.Context) error
	XAddTask(ctx context.Context, values map[string]any) (string, error)
	XAck(ctx context.Context, messageID string) error
	XReadNew(ctx context.Context, consumer string, count int64) ([]EvalStreamMessage, error)
	XClaimPending(ctx context.Context, consumer string, count int64) ([]EvalStreamMessage, error)
}

// redisEvalStream *redis.Client 适配器。
type redisEvalStream struct {
	rdb *redis.Client
}

// NewRedisEvalStream 创建 Stream 适配器（rdb 为 redisx.Client 内嵌的原生客户端）。
func NewRedisEvalStream(rdb *redis.Client) EvalStreamClient {
	return &redisEvalStream{rdb: rdb}
}

func (s *redisEvalStream) EnsureGroup(ctx context.Context) error {
	err := s.rdb.XGroupCreateMkStream(ctx, EvalStreamKey, EvalGroupName, "0").Err()
	if err != nil && !isEvalBusyGroup(err) {
		return err
	}
	return nil
}

func isEvalBusyGroup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "BUSYGROUP")
}

func (s *redisEvalStream) XAddTask(ctx context.Context, values map[string]any) (string, error) {
	return s.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: EvalStreamKey,
		MaxLen: EvalStreamMaxLen,
		Approx: true,
		Values: values,
	}).Result()
}

func (s *redisEvalStream) XAck(ctx context.Context, messageID string) error {
	return s.rdb.XAck(ctx, EvalStreamKey, EvalGroupName, messageID).Err()
}

func (s *redisEvalStream) XReadNew(ctx context.Context, consumer string, count int64) ([]EvalStreamMessage, error) {
	res, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    EvalGroupName,
		Consumer: consumer,
		Streams:  []string{EvalStreamKey, ">"},
		Count:    count,
		Block:    500 * time.Millisecond,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	return toEvalMessages(res), nil
}

func (s *redisEvalStream) XClaimPending(ctx context.Context, consumer string, count int64) ([]EvalStreamMessage, error) {
	claimed, _, err := s.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   EvalStreamKey,
		Group:    EvalGroupName,
		Consumer: consumer,
		MinIdle:  EvalPendingIdleTime,
		Start:    "0-0",
		Count:    count,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	msgs := make([]EvalStreamMessage, 0, len(claimed))
	for _, msg := range claimed {
		msgs = append(msgs, EvalStreamMessage{ID: msg.ID, Values: msg.Values})
	}
	return msgs, nil
}

func toEvalMessages(res []redis.XStream) []EvalStreamMessage {
	msgs := []EvalStreamMessage{}
	for _, stream := range res {
		for _, msg := range stream.Messages {
			msgs = append(msgs, EvalStreamMessage{ID: msg.ID, Values: msg.Values})
		}
	}
	return msgs
}

// ===== 生产者（Java EvaluateStreamProducer + enqueueEvaluationTask）=====

// EvaluateStreamProducer 评估任务生产者，实现 SessionService 的 EvaluateEnqueuer
// 扩展点：先置 evaluate_status=PENDING 再 XADD（消息字段 {sessionId, retryCount}、
// MAXLEN 1000）。投递失败对齐 Java AbstractStreamProducer.onSendFailed：置
// FAILED + "任务入队失败"，请求不失败（FAILED 状态已落库可查）。
type EvaluateStreamProducer struct {
	stream  EvalStreamClient
	tracker EvalInflightTracker
	store   EvalStore
	logger  *slog.Logger
}

// NewEvaluateStreamProducer 创建生产者。tracker 可为 nil（不记录在队时间戳）。
func NewEvaluateStreamProducer(stream EvalStreamClient, tracker EvalInflightTracker, store EvalStore, logger *slog.Logger) *EvaluateStreamProducer {
	return &EvaluateStreamProducer{stream: stream, tracker: tracker, store: store, logger: logger}
}

// EnqueueEvaluation 评估入队（末题自动交卷与提前交卷两处调用点）。
// sessionRowID 为 interview_sessions 内部自增主键；消息字段 sessionId 为业务
// session_id（对齐 Java 消息体语义）。场次不存在时对齐 Java ifPresent 语义静默跳过。
func (p *EvaluateStreamProducer) EnqueueEvaluation(ctx context.Context, sessionRowID int64) error {
	entity, err := p.store.FindByID(ctx, sessionRowID)
	if err != nil {
		if IsNotFound(err) {
			p.logger.Warn("场次不存在，跳过评估入队", slog.Int64("sessionRowId", sessionRowID))
			return nil
		}
		return err
	}

	// 先置 evaluate_status=PENDING（Java enqueueEvaluationTask：updateEvaluateStatus(PENDING, null)）。
	if err := p.store.UpdateEvaluateStatus(ctx, entity.SessionID, EvaluatePending, nil); err != nil {
		return err
	}

	_, err = p.stream.XAddTask(ctx, map[string]any{
		EvalFieldSessionID:  entity.SessionID,
		EvalFieldRetryCount: "0",
	})
	if err != nil {
		p.logger.Error("评估任务投递失败",
			slog.String("sessionId", entity.SessionID), slog.String("error", err.Error()))
		// Java onSendFailed：置 FAILED + "任务入队失败"（截断 500）。
		if markErr := p.store.UpdateEvaluateStatus(ctx, entity.SessionID, EvaluateFailed, stringPtr("任务入队失败")); markErr != nil {
			// 失败状态也写不进去（Java onSendFailed 抛出 → 500）：向上传播。
			return err
		}
		return nil
	}
	p.markEnqueued(ctx, entity.SessionID, time.Now())
	p.logger.Info("评估任务已发送到Stream",
		slog.String("sessionId", entity.SessionID), slog.String("stream", EvalStreamKey))
	return nil
}

func (p *EvaluateStreamProducer) markEnqueued(ctx context.Context, sessionID string, at time.Time) {
	if p.tracker == nil {
		return
	}
	if err := p.tracker.MarkEnqueued(ctx, sessionID, at); err != nil {
		p.logger.Warn("记录评估在队时间戳失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
	}
}

// ===== 消费者（Java EvaluateStreamConsumer + AbstractStreamConsumer）=====

// evalPayload 评估消息载荷（Java EvaluatePayload）。
type evalPayload struct {
	SessionID string
}

// EvaluateConsumer 面试评估 Stream 消费者：单线程循环 XREADGROUP，pending 消息
// 空闲超时 XAUTOCLAIM 认领（宕机恢复），失败重新 XADD（retryCount+1）后 ACK 原消息、
// 上限 3 次，耗尽置 FAILED + evaluate_error（对齐 Java，裁决 #7）。
type EvaluateConsumer struct {
	stream  EvalStreamClient
	tracker EvalInflightTracker
	store   EvalStore
	eval    EvaluationRunner
	refs    ReferenceSource
	logger  *slog.Logger

	consumerName string
	stopCh       chan struct{}
	stopOnce     sync.Once
	doneCh       chan struct{}
}

// NewEvaluateConsumer 创建消费者。tracker/refs 可为 nil。
func NewEvaluateConsumer(stream EvalStreamClient, tracker EvalInflightTracker, store EvalStore,
	eval EvaluationRunner, refs ReferenceSource, logger *slog.Logger) *EvaluateConsumer {
	return &EvaluateConsumer{
		stream: stream, tracker: tracker, store: store, eval: eval, refs: refs, logger: logger,
		consumerName: EvalConsumerPrefix + uuid.NewString()[:8],
		stopCh:       make(chan struct{}), doneCh: make(chan struct{}),
	}
}

// Start 启动消费循环（后台 goroutine）。
func (c *EvaluateConsumer) Start(ctx context.Context) {
	go func() {
		defer close(c.doneCh)
		if err := c.stream.EnsureGroup(ctx); err != nil {
			c.logger.Warn("创建评估 Stream Group 失败", slog.String("error", err.Error()))
		}
		c.logger.Info("评估消费者已启动", slog.String("consumer", c.consumerName))
		c.loop(ctx)
		c.logger.Info("评估消费者已停止", slog.String("consumer", c.consumerName))
	}()
}

// Stop 停止消费循环（等待退出）。
func (c *EvaluateConsumer) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	select {
	case <-c.doneCh:
	case <-time.After(5 * time.Second):
	}
}

func (c *EvaluateConsumer) loop(ctx context.Context) {
	for {
		select {
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}
		c.consumeOnce(ctx)
		time.Sleep(EvalPollInterval)
	}
}

// consumeOnce 一轮消费：新消息 → pending 认领（基座的宕机恢复语义）。
func (c *EvaluateConsumer) consumeOnce(ctx context.Context) {
	msgs, err := c.stream.XReadNew(ctx, c.consumerName, EvalConsumeBatch)
	if err != nil {
		c.logger.Error("消费评估消息失败", slog.String("error", err.Error()))
	} else if len(msgs) > 0 {
		for _, msg := range msgs {
			c.handleMessage(ctx, msg.ID, msg.Values)
		}
		return
	}

	// pending 消息空闲超时后由本消费者认领回收（重启后消息不丢失）。
	claimed, err := c.stream.XClaimPending(ctx, c.consumerName, EvalPendingClaimSize)
	if err != nil {
		return
	}
	for _, msg := range claimed {
		c.logger.Info("回收超时 pending 消息", slog.String("messageId", msg.ID))
		c.handleMessage(ctx, msg.ID, msg.Values)
	}
}

// handleMessage 消费骨架（对齐 Java AbstractStreamConsumer.processMessage）：
// shouldSkip → markProcessing（Java 默认 tryMarkProcessing：无条件置 PROCESSING）→
// processBusiness → markCompleted；异常按 retryCount 重新 XADD 或置 FAILED，最后 ACK。
func (c *EvaluateConsumer) handleMessage(ctx context.Context, messageID string, values map[string]any) {
	payload, ok := parseEvalPayload(values)
	if !ok {
		c.logger.Warn("消息格式错误，跳过", slog.String("messageId", messageID))
		c.ack(ctx, messageID)
		return
	}
	retryCount := parseEvalRetryCount(values)
	c.logger.Info("Processing 评估 task",
		slog.String("sessionId", payload.SessionID),
		slog.String("messageId", messageID), slog.Int("retryCount", retryCount))

	if c.shouldSkip(ctx, payload) {
		c.ack(ctx, messageID)
		c.logger.Info("评估 task skipped", slog.String("sessionId", payload.SessionID))
		return
	}

	// 条件领取（Java tryMarkProcessing 默认实现：置 PROCESSING 后恒领取成功）。
	c.updateEvaluateStatus(ctx, payload.SessionID, EvaluateProcess, nil)

	if err := c.processBusiness(ctx, payload); err != nil {
		c.logger.Error("评估 task failed",
			slog.String("sessionId", payload.SessionID), slog.String("error", err.Error()))
		if retryCount < EvalMaxRetryCount {
			c.retryMessage(ctx, payload, retryCount+1)
		} else {
			c.updateEvaluateStatus(ctx, payload.SessionID, EvaluateFailed,
				stringPtr(truncateEvaluateError(evalTaskName+" failed after retry "+strconv.Itoa(retryCount))))
		}
		c.ack(ctx, messageID)
		return
	}

	c.updateEvaluateStatus(ctx, payload.SessionID, EvaluateCompleted, nil)
	c.ack(ctx, messageID)
	c.logger.Info("评估 task completed", slog.String("sessionId", payload.SessionID))
}

// shouldSkip 状态守卫（Java EvaluateStreamConsumer.shouldSkip 逐字）：
// 会话不存在或 evaluate_status 已 COMPLETED 跳过——重试消息依赖此守卫去重，
// PROCESSING/FAILED 不跳过（裁决 #7）。
func (c *EvaluateConsumer) shouldSkip(ctx context.Context, payload evalPayload) bool {
	entity, err := c.store.FindBySessionID(ctx, payload.SessionID)
	if err != nil {
		return true // 已删除
	}
	return entity.EvaluateStatus != nil && *entity.EvaluateStatus == EvaluateCompleted
}

// processBusiness 从 DB 重放会话与作答并评估（Java processBusiness 逐字，笔记 06 §4）：
// 简历文本取自 resumes 关联（不用请求参数）；会话已删除时跳过（视为成功）。
func (c *EvaluateConsumer) processBusiness(ctx context.Context, payload evalPayload) error {
	entity, err := c.store.FindBySessionID(ctx, payload.SessionID)
	if err != nil {
		if IsNotFound(err) {
			c.logger.Warn("会话已被删除，跳过评估任务", slog.String("sessionId", payload.SessionID))
			return nil
		}
		return err
	}

	questions, err := replayQuestions(ctx, c.store, entity)
	if err != nil {
		return err
	}
	resumeText := resumeTextOf(ctx, c.store, entity)
	skillReference := ""
	if c.refs != nil {
		skillReference = c.refs.BuildEvaluationReferenceSectionSafe(evalSkillID(entity))
	}

	report, err := c.eval.Evaluate(ctx, EvaluateInput{
		SessionID:      entity.SessionID,
		ResumeText:     resumeText,
		Questions:      questions,
		SkillReference: skillReference,
	}, entity.LLMProvider)
	if err != nil {
		return err
	}
	return saveReport(ctx, c.store, c.logger, payload.SessionID, report)
}

// retryMessage 重试机制（裁决 #7 逐字）：重新 XADD 一条新消息（retryCount+1），
// 原消息 ACK 丢弃；重投失败置 FAILED + "重试入队失败: ..."。
func (c *EvaluateConsumer) retryMessage(ctx context.Context, payload evalPayload, retryCount int) {
	_, err := c.stream.XAddTask(ctx, map[string]any{
		EvalFieldSessionID:  payload.SessionID,
		EvalFieldRetryCount: strconv.Itoa(retryCount),
	})
	if err != nil {
		c.logger.Error("重试入队失败",
			slog.String("sessionId", payload.SessionID), slog.String("error", err.Error()))
		c.updateEvaluateStatus(ctx, payload.SessionID, EvaluateFailed,
			stringPtr(truncateEvaluateError("重试入队失败: "+err.Error())))
		return
	}
	if c.tracker != nil {
		if terr := c.tracker.MarkEnqueued(ctx, payload.SessionID, time.Now()); terr != nil {
			c.logger.Warn("记录评估在队时间戳失败", slog.String("sessionId", payload.SessionID))
		}
	}
	c.logger.Info("评估任务已重新入队",
		slog.String("sessionId", payload.SessionID), slog.Int("retryCount", retryCount))
}

// updateEvaluateStatus 评估状态更新（Java EvaluateStreamConsumer.updateEvaluateStatus）：
// 任何失败仅记日志（含 evaluate_error 截断 500）；COMPLETED/FAILED 时移除在队时间戳。
func (c *EvaluateConsumer) updateEvaluateStatus(ctx context.Context, sessionID string, status EvaluateStatus, evalError *string) {
	if err := c.store.UpdateEvaluateStatus(ctx, sessionID, status, evalError); err != nil {
		c.logger.Error("更新评估状态失败",
			slog.String("sessionId", sessionID), slog.String("status", string(status)), slog.String("error", err.Error()))
		return
	}
	if c.tracker != nil && (status == EvaluateCompleted || status == EvaluateFailed) {
		if err := c.tracker.Forget(ctx, sessionID); err != nil {
			c.logger.Warn("清理评估在队时间戳失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
		}
	}
}

func (c *EvaluateConsumer) ack(ctx context.Context, messageID string) {
	if err := c.stream.XAck(ctx, messageID); err != nil {
		c.logger.Error("ACK 失败", slog.String("messageId", messageID), slog.String("error", err.Error()))
	}
}

// parseEvalPayload 消息字段解析（Java parsePayload）：sessionId 缺失视为格式错误。
func parseEvalPayload(values map[string]any) (evalPayload, bool) {
	raw, exists := values[EvalFieldSessionID]
	if !exists {
		return evalPayload{}, false
	}
	sessionID := evalValueString(raw)
	if sessionID == "" {
		return evalPayload{}, false
	}
	return evalPayload{SessionID: sessionID}, true
}

// parseEvalRetryCount retryCount 字段解析（Java parseRetryCount：缺失/非法 → 0）。
func parseEvalRetryCount(values map[string]any) int {
	raw, exists := values[EvalFieldRetryCount]
	if !exists {
		return 0
	}
	n, err := strconv.Atoi(evalValueString(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// evalValueString Stream 值转字符串（go-redis 返回 string 或 []byte）。
func evalValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return ""
	}
}
