// 语音评估 Redis Stream 消费者（票 08）：常量、消息字段、消费组与重试机制对齐
// Java AsyncTaskStreamConstants（L118-138）+ VoiceEvaluateStreamConsumer +
// AbstractStreamConsumer.processMessage（L96-144）。消费基座模式沿
// internal/interview/evaluation_stream.go（同仓先例）。
// 顺序契约：跳过（会话缺失/已完成）→ 置 PROCESSING → 评估 → 置 COMPLETED；
// 失败重投 retryCount+1，上限 3 次后置 FAILED + evaluate_error（截断 500）。
package voiceinterview

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// 消费者常量，与 Java AsyncTaskStreamConstants 语音段逐字对齐。
const (
	VoiceEvalGroupName       = "voice-evaluate-group"
	VoiceEvalConsumerPrefix  = "voice-evaluate-consumer-"
	VoiceEvalMaxRetryCount   = 3
	VoiceEvalConsumeBatch    = 10
	VoiceEvalPollInterval    = 1 * time.Second
	VoiceEvalPendingIdleTime = 5 * time.Minute
	VoiceEvalClaimBatchSize  = 10
)

// voiceEvalTaskName 任务显示名（Java taskDisplayName()，FAILED 文案前缀逐字）。
const voiceEvalTaskName = "语音面试评估"

// VoiceEvalMessage Stream 消息（ID + 字段表）。
type VoiceEvalMessage struct {
	ID     string
	Values map[string]any
}

// VoiceEvalStreamClient 语音评估流的最小 Stream 能力（生产实现为本文件
// *redisEvalStream 扩展，测试注入内存实现）。
type VoiceEvalStreamClient interface {
	EnsureGroup(ctx context.Context) error
	XAddTask(ctx context.Context, values map[string]any) (string, error)
	XAck(ctx context.Context, messageID string) error
	XReadNew(ctx context.Context, consumer string, count int64) ([]VoiceEvalMessage, error)
	XClaimPending(ctx context.Context, consumer string, count int64) ([]VoiceEvalMessage, error)
}

// ===== redis 适配扩展（redisEvalStream 定义于 evaluate_stream.go）=====

func (s *redisEvalStream) EnsureGroup(ctx context.Context) error {
	err := s.rdb.XGroupCreateMkStream(ctx, VoiceEvalStreamKey, VoiceEvalGroupName, "0").Err()
	if err != nil && !isVoiceBusyGroup(err) {
		return err
	}
	return nil
}

func isVoiceBusyGroup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "BUSYGROUP")
}

func (s *redisEvalStream) XAck(ctx context.Context, messageID string) error {
	return s.rdb.XAck(ctx, VoiceEvalStreamKey, VoiceEvalGroupName, messageID).Err()
}

func (s *redisEvalStream) XReadNew(ctx context.Context, consumer string, count int64) ([]VoiceEvalMessage, error) {
	res, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    VoiceEvalGroupName,
		Consumer: consumer,
		Streams:  []string{VoiceEvalStreamKey, ">"},
		Count:    count,
		Block:    500 * time.Millisecond,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	return toVoiceEvalMessages(res), nil
}

func (s *redisEvalStream) XClaimPending(ctx context.Context, consumer string, count int64) ([]VoiceEvalMessage, error) {
	claimed, _, err := s.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   VoiceEvalStreamKey,
		Group:    VoiceEvalGroupName,
		Consumer: consumer,
		MinIdle:  VoiceEvalPendingIdleTime,
		Start:    "0-0",
		Count:    count,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	msgs := make([]VoiceEvalMessage, 0, len(claimed))
	for _, msg := range claimed {
		msgs = append(msgs, VoiceEvalMessage{ID: msg.ID, Values: msg.Values})
	}
	return msgs, nil
}

func toVoiceEvalMessages(res []redis.XStream) []VoiceEvalMessage {
	msgs := []VoiceEvalMessage{}
	for _, stream := range res {
		for _, msg := range stream.Messages {
			msgs = append(msgs, VoiceEvalMessage{ID: msg.ID, Values: msg.Values})
		}
	}
	return msgs
}

// ===== 消费者 =====

// voiceEvalPayload 评估消息载荷（Java VoiceEvaluatePayload(sessionId)）。
type voiceEvalPayload struct {
	SessionID int64
}

// VoiceEvaluateConsumer 语音评估 Stream 消费者：单线程循环 XREADGROUP，
// pending 消息空闲超时 XAUTOCLAIM 认领（宕机恢复），失败重新 XADD（retryCount+1）
// 后 ACK 原消息、上限 3 次，耗尽置 FAILED + evaluate_error。
type VoiceEvaluateConsumer struct {
	stream VoiceEvalStreamClient
	store  EvalStore
	eval   VoiceEvaluator
	logger *slog.Logger

	consumerName string
	stopCh       chan struct{}
	stopOnce     sync.Once
	doneCh       chan struct{}
}

// NewVoiceEvaluateConsumer 创建语音评估消费者。
func NewVoiceEvaluateConsumer(stream VoiceEvalStreamClient, store EvalStore, eval VoiceEvaluator, logger *slog.Logger) *VoiceEvaluateConsumer {
	return &VoiceEvaluateConsumer{
		stream: stream, store: store, eval: eval, logger: logger,
		consumerName: VoiceEvalConsumerPrefix + uuid.NewString()[:8],
		stopCh:       make(chan struct{}), doneCh: make(chan struct{}),
	}
}

// Start 启动消费循环（后台 goroutine）。
func (c *VoiceEvaluateConsumer) Start(ctx context.Context) {
	go func() {
		defer close(c.doneCh)
		if err := c.stream.EnsureGroup(ctx); err != nil {
			c.logger.Warn("创建语音评估 Stream Group 失败", slog.String("error", err.Error()))
		}
		c.logger.Info("语音评估消费者已启动", slog.String("consumer", c.consumerName))
		c.loop(ctx)
		c.logger.Info("语音评估消费者已停止", slog.String("consumer", c.consumerName))
	}()
}

// Stop 停止消费循环（等待退出）。
func (c *VoiceEvaluateConsumer) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	select {
	case <-c.doneCh:
	case <-time.After(5 * time.Second):
	}
}

func (c *VoiceEvaluateConsumer) loop(ctx context.Context) {
	for {
		select {
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}
		c.consumeOnce(ctx)
		time.Sleep(VoiceEvalPollInterval)
	}
}

// consumeOnce 一轮消费：新消息 → pending 认领（宕机恢复语义）。
func (c *VoiceEvaluateConsumer) consumeOnce(ctx context.Context) {
	msgs, err := c.stream.XReadNew(ctx, c.consumerName, VoiceEvalConsumeBatch)
	if err != nil {
		c.logger.Error("消费语音评估消息失败", slog.String("error", err.Error()))
	} else if len(msgs) > 0 {
		for _, msg := range msgs {
			c.handleMessage(ctx, msg.ID, msg.Values)
		}
		return
	}

	claimed, err := c.stream.XClaimPending(ctx, c.consumerName, VoiceEvalClaimBatchSize)
	if err != nil {
		return
	}
	for _, msg := range claimed {
		c.logger.Info("回收超时 pending 消息", slog.String("messageId", msg.ID))
		c.handleMessage(ctx, msg.ID, msg.Values)
	}
}

// handleMessage 消费骨架（对齐 Java AbstractStreamConsumer.processMessage）：
// shouldSkip → markProcessing → processBusiness → markCompleted；异常按 retryCount
// 重新 XADD 或置 FAILED，最后 ACK。
func (c *VoiceEvaluateConsumer) handleMessage(ctx context.Context, messageID string, values map[string]any) {
	payload, ok := parseVoiceEvalPayload(values)
	if !ok {
		c.logger.Warn("消息格式错误，跳过", slog.String("messageId", messageID))
		c.ack(ctx, messageID)
		return
	}
	retryCount := parseVoiceEvalRetryCount(values)
	c.logger.Info("Processing 语音面试评估 task",
		slog.Int64("voiceSessionId", payload.SessionID),
		slog.String("messageId", messageID), slog.Int("retryCount", retryCount))

	if c.shouldSkip(ctx, payload) {
		c.ack(ctx, messageID)
		c.logger.Info("语音面试评估 task skipped",
			slog.String("voiceSessionId", strconv.FormatInt(payload.SessionID, 10)))
		return
	}

	// markProcessing（Java updateEvaluateStatus(PROCESSING, null)）。
	c.updateEvaluateStatus(ctx, payload.SessionID, VoiceEvaluateProcess, nil)

	if err := c.processBusiness(ctx, payload); err != nil {
		c.logger.Error("语音面试评估 task failed",
			slog.Int64("voiceSessionId", payload.SessionID), slog.String("error", err.Error()))
		if retryCount < VoiceEvalMaxRetryCount {
			c.retryMessage(ctx, payload, retryCount+1)
		} else {
			c.updateEvaluateStatus(ctx, payload.SessionID, VoiceEvaluateFailed,
				stringPtr(truncateVoiceErrMsg(voiceEvalTaskName+" failed after retry "+strconv.Itoa(retryCount))))
		}
		c.ack(ctx, messageID)
		return
	}

	c.updateEvaluateStatus(ctx, payload.SessionID, VoiceEvaluateCompleted, nil)
	c.ack(ctx, messageID)
	c.logger.Info("语音面试评估 task completed", slog.Int64("voiceSessionId", payload.SessionID))
}

// shouldSkip 幂等守卫（Java VoiceEvaluateStreamConsumer.shouldSkip L82-86）：
// 会话不存在（Optional 为空 → orElse(true)）或评估已完成时跳过；PROCESSING/FAILED/
// PENDING 不跳过（FAILED 的重试消息依赖此语义再次进入评估）。其余查询错误（DB
// 故障等）不跳过：Java findById 抛出的异常会传出入 processMessage 的 catch 走
// 重投路径，这里放行交给 processBusiness 以同一错误收尾重投，语义一致。
func (c *VoiceEvaluateConsumer) shouldSkip(ctx context.Context, payload voiceEvalPayload) bool {
	entity, err := c.store.FindSessionByID(ctx, payload.SessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return true
		}
		return false
	}
	return entity.EvaluateStatus != nil && *entity.EvaluateStatus == VoiceEvaluateCompleted
}

// processBusiness 会话存在性复查后执行评估（Java processBusiness L95-103）：
// 会话已被删除时告警跳过（视为成功，状态更新对已删行无效果）。
func (c *VoiceEvaluateConsumer) processBusiness(ctx context.Context, payload voiceEvalPayload) error {
	if _, err := c.store.FindSessionByID(ctx, payload.SessionID); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			c.logger.Warn("语音面试会话已被删除，跳过评估任务",
				slog.Int64("sessionId", payload.SessionID))
			return nil
		}
		return err
	}
	return c.eval.GenerateEvaluation(ctx, payload.SessionID)
}

// retryMessage 失败重投：重新 XADD 一条新消息（retryCount+1），原消息由调用方 ACK；
// 重投失败置 FAILED + "重试入队失败: ..."（Java retryMessage L118-137 逐字）。
func (c *VoiceEvaluateConsumer) retryMessage(ctx context.Context, payload voiceEvalPayload, retryCount int) {
	_, err := c.stream.XAddTask(ctx, map[string]any{
		VoiceEvalFieldSessionID:  strconv.FormatInt(payload.SessionID, 10),
		VoiceEvalFieldRetryCount: strconv.Itoa(retryCount),
	})
	if err != nil {
		c.logger.Error("重试入队失败",
			slog.Int64("sessionId", payload.SessionID), slog.String("error", err.Error()))
		c.updateEvaluateStatus(ctx, payload.SessionID, VoiceEvaluateFailed,
			stringPtr(truncateVoiceErrMsg("重试入队失败: "+err.Error())))
		return
	}
	c.logger.Info("语音面试评估任务已重新入队",
		slog.Int64("sessionId", payload.SessionID), slog.Int("retryCount", retryCount))
}

// updateEvaluateStatus 评估状态更新：任何失败仅记日志（对齐 Java
// updateEvaluateStatus 的 try/catch，不中断消费流程）。
func (c *VoiceEvaluateConsumer) updateEvaluateStatus(ctx context.Context, sessionID int64, status VoiceEvaluateStatus, evalError *string) {
	if err := c.store.UpdateEvaluateStatus(ctx, sessionID, status, evalError); err != nil {
		c.logger.Error("更新语音面试评估状态失败",
			slog.Int64("sessionId", sessionID),
			slog.String("status", string(status)), slog.String("error", err.Error()))
	}
}

func (c *VoiceEvaluateConsumer) ack(ctx context.Context, messageID string) {
	if err := c.stream.XAck(ctx, messageID); err != nil {
		c.logger.Error("ACK 失败", slog.String("messageId", messageID), slog.String("error", err.Error()))
	}
}

// parseVoiceEvalPayload 消息字段解析（Java parsePayload）：voiceSessionId 缺失或
// 非数字视为格式错误（parseLong 异常 → ACK 丢弃）。
func parseVoiceEvalPayload(values map[string]any) (voiceEvalPayload, bool) {
	raw, exists := values[VoiceEvalFieldSessionID]
	if !exists {
		return voiceEvalPayload{}, false
	}
	str := voiceValueString(raw)
	if str == "" {
		return voiceEvalPayload{}, false
	}
	id, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		return voiceEvalPayload{}, false
	}
	return voiceEvalPayload{SessionID: id}, true
}

// parseVoiceEvalRetryCount retryCount 字段解析（缺失/非法 → 0）。
func parseVoiceEvalRetryCount(values map[string]any) int {
	raw, exists := values[VoiceEvalFieldRetryCount]
	if !exists {
		return 0
	}
	n, err := strconv.Atoi(voiceValueString(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// voiceValueString Stream 值转字符串（go-redis 返回 string 或 []byte）。
func voiceValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return ""
	}
}
