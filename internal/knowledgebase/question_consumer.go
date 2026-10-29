// 知识库出题 Stream 消费者与卡住任务恢复调度（票 04，Java QuestionGenStreamConsumer +
// AbstractStreamConsumer.processMessage + QuestionGenerationRecoveryScheduler 逐字移植）：
// 条件领取（QUEUED→PROCESSING，taskId 幂等）→ 从配置快照读取参数执行生成 →
// 失败按 retryCount 重试重投（≤3，重置失败则 ACK 丢弃）→ 耗尽转 FAILED + 安全文案。
// 恢复调度 60s 巡检：QUEUED 超 2 分钟补投、PROCESSING 超 20 分钟先条件重置再补投。
package knowledgebase

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

// 恢复阈值（Java QuestionGenerationRecoveryScheduler 常量）。
const (
	questionGenQueuedStaleMinutes     = 2
	questionGenProcessingStaleMinutes = 20
	questionGenRecoveryBatchSize      = 10
)

// QuestionGenExecutor 出题执行接缝（生产实现 *QuestionGenerationService，测试注入 fake）。
type QuestionGenExecutor interface {
	ExecuteGeneration(ctx context.Context, kbID int64, taskID string, config QuestionGenerationConfig) error
}

// QuestionGenConsumer 出题消费者。
type QuestionGenConsumer struct {
	stream   QuestionGenStream
	store    Store
	gen      QuestionGenExecutor
	producer *QuestionGenProducer
	logger   *slog.Logger

	consumerName string
	stopCh       chan struct{}
	stopOnce     sync.Once
	doneCh       chan struct{}
}

// NewQuestionGenConsumer 创建消费者。
func NewQuestionGenConsumer(stream QuestionGenStream, store Store, gen QuestionGenExecutor,
	producer *QuestionGenProducer, logger *slog.Logger) *QuestionGenConsumer {
	return &QuestionGenConsumer{
		stream: stream, store: store, gen: gen, producer: producer, logger: logger,
		consumerName: buildQuestionGenConsumerName(),
		stopCh:       make(chan struct{}), doneCh: make(chan struct{}),
	}
}

// Start 启动消费循环（后台 goroutine）。
func (c *QuestionGenConsumer) Start(ctx context.Context) {
	go func() {
		defer close(c.doneCh)
		if err := c.stream.EnsureGroup(ctx); err != nil {
			c.logger.Warn("创建知识库出题 Stream Group 失败", slog.String("error", err.Error()))
		}
		c.logger.Info("知识库出题消费者已启动", slog.String("consumer", c.consumerName))
		c.loop(ctx)
		c.logger.Info("知识库出题消费者已停止", slog.String("consumer", c.consumerName))
	}()
}

// Stop 停止消费循环（等待退出）。
func (c *QuestionGenConsumer) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	select {
	case <-c.doneCh:
	case <-time.After(5 * time.Second):
	}
}

func (c *QuestionGenConsumer) loop(ctx context.Context) {
	for {
		select {
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}
		c.consumeOnce(ctx)
		time.Sleep(PollInterval)
	}
}

// consumeOnce 一轮消费：新消息 → pending 认领（宕机恢复语义）。
func (c *QuestionGenConsumer) consumeOnce(ctx context.Context) {
	msgs, err := c.stream.XReadNew(ctx, c.consumerName, ConsumeBatch)
	if err != nil {
		c.logger.Error("消费知识库出题消息失败", slog.String("error", err.Error()))
	} else if len(msgs) > 0 {
		for _, msg := range msgs {
			c.HandleMessage(ctx, msg.ID, msg.Values)
		}
		return
	}
	claimed, err := c.stream.XClaimPending(ctx, c.consumerName, PendingClaimSize)
	if err != nil {
		return
	}
	for _, msg := range claimed {
		c.logger.Info("回收超时 pending 出题消息", slog.String("messageId", msg.ID))
		c.HandleMessage(ctx, msg.ID, msg.Values)
	}
}

// HandleMessage 消费骨架（对齐 Java AbstractStreamConsumer.processMessage 的
// 出题分支：无 shouldSkip，领取失败 ACK 丢弃）。
func (c *QuestionGenConsumer) HandleMessage(ctx context.Context, messageID string, values map[string]any) {
	kbID, taskID, retryCount, ok := parseQuestionGenPayload(values)
	if !ok {
		c.logger.Warn("题目生成消息格式错误，丢弃: messageId=" + messageID)
		c.ack(ctx, messageID)
		return
	}
	identifier := slog.Int64("kbId", kbID)
	c.logger.Info("Processing 题目生成 task",
		identifier, slog.String("messageId", messageID), slog.Int("retryCount", retryCount))

	// 条件领取：QUEUED → PROCESSING（taskId 幂等，多实例防重复）。
	claimed, err := c.store.TryMarkQuestionProcessing(ctx, kbID, taskID, time.Now())
	if err != nil || !claimed {
		c.ack(ctx, messageID)
		c.logger.Info("题目生成 task was not claimed", identifier)
		return
	}

	if err := c.processBusiness(ctx, kbID, taskID); err != nil {
		c.logger.Error("题目生成 task failed", identifier, slog.String("taskId", taskID),
			slog.String("error", err.Error()))
		if retryCount < MaxRetryCount {
			c.retryMessage(ctx, kbID, taskID, retryCount+1)
		} else if _, ferr := c.store.MarkQuestionGenFailed(ctx, kbID, taskID, time.Now()); ferr != nil {
			c.logger.Error("题目生成标记失败态异常", identifier, slog.String("error", ferr.Error()))
		}
		c.ack(ctx, messageID)
		return
	}

	// Java markCompleted：题目替换与 COMPLETED 已在同一事务提交，no-op。
	c.ack(ctx, messageID)
	c.logger.Info("题目生成 task completed", identifier, slog.String("taskId", taskID))
}

// processBusiness 从配置快照读取出题参数后执行生成（Java processBusiness：
// stateService.getConfig → generationService.executeGeneration）。
func (c *QuestionGenConsumer) processBusiness(ctx context.Context, kbID int64, taskID string) error {
	config, err := c.loadConfig(ctx, kbID, taskID)
	if err != nil {
		return err
	}
	return c.gen.ExecuteGeneration(ctx, kbID, taskID, config)
}

// loadConfig 配置快照读取（Java QuestionGenerationStateService.getConfig）：
// 任务不存在 6001、taskId 不匹配 400「题目生成任务已失效」、快照缺失 500。
func (c *QuestionGenConsumer) loadConfig(ctx context.Context, kbID int64, taskID string) (QuestionGenerationConfig, error) {
	kb, err := c.store.FindByID(ctx, kbID)
	if err != nil {
		return QuestionGenerationConfig{}, err
	}
	if kb.QuestionGenTaskID == nil || *kb.QuestionGenTaskID != taskID {
		return QuestionGenerationConfig{}, apperr.New(apperr.CodeBadRequest, "题目生成任务已失效")
	}
	config, err := readQuestionGenConfig(kb.QuestionGenConfig)
	if err != nil {
		return QuestionGenerationConfig{}, err
	}
	if config == nil {
		return QuestionGenerationConfig{}, apperr.New(apperr.CodeInternalError, "题目生成配置不存在")
	}
	return *config, nil
}

// retryMessage 重试重投（Java retryMessage）：PROCESSING → QUEUED 条件重置成功才
// 重新投递，否则 ACK 丢弃；重投失败由生产者 onSendFailed 标记 FAILED。
func (c *QuestionGenConsumer) retryMessage(ctx context.Context, kbID int64, taskID string, nextRetry int) {
	reset, err := c.store.ResetQuestionForRetry(ctx, kbID, taskID, time.Now())
	if err != nil || !reset {
		c.logger.Info("题目生成任务已失效，不再重试",
			slog.Int64("kbId", kbID), slog.String("taskId", taskID), slog.Int("retryCount", nextRetry))
		return
	}
	c.producer.SendGenerateTask(ctx, kbID, taskID, nextRetry)
}

func (c *QuestionGenConsumer) ack(ctx context.Context, messageID string) {
	if err := c.stream.XAck(ctx, messageID); err != nil {
		c.logger.Error("ACK 失败", slog.String("messageId", messageID), slog.String("error", err.Error()))
	}
}

// parseQuestionGenPayload 消息字段解析：kbId/taskId 缺失或非法视为格式错误（ACK 丢弃）。
func parseQuestionGenPayload(values map[string]any) (int64, string, int, bool) {
	raw, exists := values[FieldKBID]
	if !exists {
		return 0, "", 0, false
	}
	kbID, err := strconv.ParseInt(streamValueString(raw), 10, 64)
	if err != nil || kbID <= 0 {
		return 0, "", 0, false
	}
	taskRaw, exists := values[QuestionFieldTaskID]
	if !exists {
		return 0, "", 0, false
	}
	taskID := streamValueString(taskRaw)
	if taskID == "" {
		return 0, "", 0, false
	}
	retryCount := 0
	if rcRaw, exists := values[FieldRetryCount]; exists {
		if n, err := strconv.Atoi(streamValueString(rcRaw)); err == nil && n >= 0 {
			retryCount = n
		}
	}
	return kbID, taskID, retryCount, true
}

// ===== 恢复调度（Java QuestionGenerationRecoveryScheduler 逐字移植）=====

// QuestionGenRecoveryScheduler 出题卡住任务恢复调度器。
type QuestionGenRecoveryScheduler struct {
	store    Store
	producer *QuestionGenProducer
	logger   *slog.Logger
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewQuestionGenRecoveryScheduler 创建恢复调度器。
func NewQuestionGenRecoveryScheduler(store Store, producer *QuestionGenProducer, logger *slog.Logger) *QuestionGenRecoveryScheduler {
	return &QuestionGenRecoveryScheduler{store: store, producer: producer, logger: logger,
		stopCh: make(chan struct{}), doneCh: make(chan struct{})}
}

// Start 启动调度（Java fixedDelay = initialDelay = 60s）。
func (s *QuestionGenRecoveryScheduler) Start(ctx context.Context) {
	go func() {
		defer close(s.doneCh)
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-ctx.Done():
				return
			case <-timer.C:
				s.RecoverStaleTasks(ctx, time.Now())
				timer.Reset(time.Minute)
			}
		}
	}()
}

// Stop 停止调度。
func (s *QuestionGenRecoveryScheduler) Stop() {
	close(s.stopCh)
	select {
	case <-s.doneCh:
	case <-time.After(3 * time.Second):
	}
}

// RecoverStaleTasks 一轮巡检（now 注入时钟，供测试）。
func (s *QuestionGenRecoveryScheduler) RecoverStaleTasks(ctx context.Context, now time.Time) {
	queuedThreshold := now.Add(-questionGenQueuedStaleMinutes * time.Minute)
	staleQueued, err := s.store.FindStaleQuestionGenTasks(ctx, QuestionGenQueued, queuedThreshold, questionGenRecoveryBatchSize)
	if err != nil {
		s.logger.Error("扫描 QUEUED 卡住出题任务失败", slog.String("error", err.Error()))
	}
	for _, kb := range staleQueued {
		if kb.QuestionGenTaskID == nil {
			continue
		}
		// Java recoverQueued：条件推进时间（一个调度周期只补投一次）再补投。
		touched, terr := s.store.TouchQueuedQuestionForRecovery(ctx, kb.ID, *kb.QuestionGenTaskID, queuedThreshold, now)
		if terr != nil || !touched {
			continue
		}
		s.producer.SendGenerateTask(ctx, kb.ID, *kb.QuestionGenTaskID, 0)
		s.logger.Info("重新投递等待中的题目生成任务",
			slog.Int64("kbId", kb.ID), slog.String("taskId", *kb.QuestionGenTaskID))
	}

	processingThreshold := now.Add(-questionGenProcessingStaleMinutes * time.Minute)
	staleProcessing, err := s.store.FindStaleQuestionGenTasks(ctx, QuestionGenProcessing, processingThreshold, questionGenRecoveryBatchSize)
	if err != nil {
		s.logger.Error("扫描 PROCESSING 卡住出题任务失败", slog.String("error", err.Error()))
	}
	for _, kb := range staleProcessing {
		if kb.QuestionGenTaskID == nil {
			continue
		}
		// Java recoverProcessing：先条件重置回 QUEUED 再补投。
		reset, rerr := s.store.ResetStaleQuestionProcessing(ctx, kb.ID, *kb.QuestionGenTaskID, processingThreshold, now)
		if rerr != nil || !reset {
			continue
		}
		s.producer.SendGenerateTask(ctx, kb.ID, *kb.QuestionGenTaskID, 0)
		s.logger.Warn("恢复卡住的题目生成任务",
			slog.Int64("kbId", kb.ID), slog.String("taskId", *kb.QuestionGenTaskID))
	}
}
