// 知识库向量化 Stream 消费者：条件领取 + attempt_id 代次 fencing + 节流心跳。
// 对应 Java VectorizeStreamConsumer + AbstractStreamConsumer.processMessage：
// 跳过（缺失/COMPLETED）→ PENDING→PROCESSING 原子领取 → 下载解析（后心跳）→
// 分批向量化（每批节流心跳 30s）→ 条件写终态；失败按 retryCount 重投（≤3）或置败。
package knowledgebase

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/wuzuxian/interview/internal/apperr"
)

// DocumentParser 下载解析接缝（生产实现 *ParseService，测试注入 fake）。
type DocumentParser interface {
	DownloadAndParseContent(ctx context.Context, storageKey, originalFilename string) (string, error)
}

// Vectorizer 向量化执行接缝（生产实现 *VectorizeService，测试注入 fake）。
type Vectorizer interface {
	VectorizeAndStore(ctx context.Context, kbID int64, content string, attemptID string, heartbeat func() error, source SourceInfo) error
}

// VectorizeConsumer 向量化消费者。
type VectorizeConsumer struct {
	stream     VectorizeStream
	store      Store
	parser     DocumentParser
	vectorizer Vectorizer
	logger     *slog.Logger
	// heartbeatThrottle Embedding 阶段心跳节流阈值（Java heartbeatThrottle）。
	heartbeatThrottle time.Duration

	consumerName string
	stopCh       chan struct{}
	stopOnce     sync.Once
	doneCh       chan struct{}

	// lastEmbeddingHeartbeat 当前任务的 Embedding 心跳上次写库时间（领取时清零），仅内存态。
	lastEmbeddingHeartbeatMu sync.Mutex
	lastEmbeddingHeartbeat   time.Time
}

// NewVectorizeConsumer 创建消费者。
func NewVectorizeConsumer(stream VectorizeStream, store Store, parser DocumentParser, vectorizer Vectorizer, heartbeatThrottle time.Duration, logger *slog.Logger) *VectorizeConsumer {
	return &VectorizeConsumer{
		stream: stream, store: store, parser: parser, vectorizer: vectorizer,
		logger: logger, heartbeatThrottle: heartbeatThrottle,
		consumerName: buildVectorizeConsumerName(),
		stopCh:       make(chan struct{}), doneCh: make(chan struct{}),
	}
}

// Start 启动消费循环（后台 goroutine）。
func (c *VectorizeConsumer) Start(ctx context.Context) {
	go func() {
		defer close(c.doneCh)
		if err := c.stream.EnsureGroup(ctx); err != nil {
			c.logger.Warn("创建知识库向量化 Stream Group 失败", slog.String("error", err.Error()))
		}
		c.logger.Info("知识库向量化消费者已启动", slog.String("consumer", c.consumerName))
		c.loop(ctx)
		c.logger.Info("知识库向量化消费者已停止", slog.String("consumer", c.consumerName))
	}()
}

// Stop 停止消费循环（等待退出）。
func (c *VectorizeConsumer) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	select {
	case <-c.doneCh:
	case <-time.After(5 * time.Second):
	}
}

func (c *VectorizeConsumer) loop(ctx context.Context) {
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
func (c *VectorizeConsumer) consumeOnce(ctx context.Context) {
	msgs, err := c.stream.XReadNew(ctx, c.consumerName, ConsumeBatch)
	if err != nil {
		c.logger.Error("消费知识库向量化消息失败", slog.String("error", err.Error()))
	} else if len(msgs) > 0 {
		for _, msg := range msgs {
			c.handleMessage(ctx, msg.ID, msg.Values)
		}
		return
	}
	claimed, err := c.stream.XClaimPending(ctx, c.consumerName, PendingClaimSize)
	if err != nil {
		return
	}
	for _, msg := range claimed {
		c.logger.Info("回收超时 pending 消息", slog.String("messageId", msg.ID))
		c.handleMessage(ctx, msg.ID, msg.Values)
	}
}

// handleMessage 消费骨架（对齐 Java AbstractStreamConsumer.processMessage）。
func (c *VectorizeConsumer) handleMessage(ctx context.Context, messageID string, values map[string]any) {
	kbID, retryCount, ok := parseVectorizePayload(values)
	if !ok {
		c.logger.Warn("消息格式错误，跳过", slog.String("messageId", messageID))
		c.ack(ctx, messageID)
		return
	}
	identifier := slog.Int64("kbId", kbID)
	c.logger.Info("Processing 向量化 task",
		identifier, slog.String("messageId", messageID), slog.Int("retryCount", retryCount))

	if c.shouldSkip(ctx, kbID) {
		c.ack(ctx, messageID)
		c.logger.Info("向量化 task skipped", identifier)
		return
	}

	// 条件领取：PENDING → PROCESSING（多实例防重复）。
	attemptID := newAttemptID()
	claimed, err := c.store.TryMarkVectorProcessing(ctx, kbID, attemptID, time.Now())
	if err != nil || claimed == 0 {
		c.ack(ctx, messageID)
		c.logger.Info("向量化 task was not claimed", identifier)
		return
	}
	c.resetEmbeddingHeartbeat()

	if err := c.processBusiness(ctx, kbID, attemptID); err != nil {
		c.logger.Error("向量化 task failed", identifier, slog.String("error", err.Error()))
		if retryCount < MaxRetryCount {
			c.retryMessage(ctx, kbID, attemptID, retryCount+1)
		} else {
			_, _ = c.store.FailVectorIfProcessing(ctx, kbID, attemptID,
				taskDisplayName+" failed after retry "+strconv.Itoa(retryCount), time.Now())
		}
		c.ack(ctx, messageID)
		return
	}

	// 条件写入终态：状态已不是本代次 PROCESSING 时 no-op，防止覆盖他人结果。
	_, _ = c.store.CompleteVectorIfProcessing(ctx, kbID, attemptID, time.Now())
	c.ack(ctx, messageID)
	c.logger.Info("向量化 task completed", identifier)
}

// shouldSkip 幂等守卫（Java shouldSkip：findById 的 Optional 为空 orElse(true)
// 跳过、COMPLETED 跳过；其余查询错误不跳过——Java 异常会进入 catch 走重投路径，
// 这里放行交给 processBusiness 以同一错误收尾，语义一致）。
func (c *VectorizeConsumer) shouldSkip(ctx context.Context, kbID int64) bool {
	kb, err := c.store.FindByID(ctx, kbID)
	if err != nil {
		return apperr.Is(err, apperr.CodeKnowledgeBaseNotFound)
	}
	return kb.VectorStatus == VectorCompleted
}

// processBusiness 实体存在性复查后执行下载解析与向量化（Java processBusiness）：
// 知识库已被删除时告警跳过（视为成功）；其余错误上抛走重试路径。
func (c *VectorizeConsumer) processBusiness(ctx context.Context, kbID int64, attemptID string) error {
	kb, err := c.store.FindByID(ctx, kbID)
	if err != nil {
		if apperr.Is(err, apperr.CodeKnowledgeBaseNotFound) {
			c.logger.Warn("知识库已被删除，跳过向量化任务", slog.Int64("kbId", kbID))
			return nil
		}
		return err
	}
	if isBlank(deref(kb.StorageKey)) || isBlank(kb.OriginalFilename) {
		return apperr.New(apperr.CodeInternalError, "知识库缺少存储信息，无法下载原文件")
	}
	content, err := c.parser.DownloadAndParseContent(ctx, deref(kb.StorageKey), kb.OriginalFilename)
	if err != nil {
		return err
	}
	// 下载 + 解析完成心跳（Java heartbeat：更新数 != 1 视为执行权失效抛异常）。
	updated, herr := c.store.HeartbeatVectorProcessing(ctx, kbID, attemptID, time.Now())
	if herr != nil {
		return herr
	}
	if updated != 1 {
		return apperr.New(apperr.CodeInternalError, "向量化任务执行权已失效")
	}
	if isBlank(content) {
		return apperr.New(apperr.CodeInternalError, "无法从文件中提取文本内容")
	}
	source := SourceInfo{Name: kb.Name, Filename: kb.OriginalFilename, FileHash: kb.FileHash}
	return c.vectorizer.VectorizeAndStore(ctx, kbID, content, attemptID, func() error {
		// 分块完成与每个 Embedding 批次后的节流心跳。
		return c.throttledEmbeddingHeartbeat(ctx, kbID, attemptID)
	}, source)
}

// throttledEmbeddingHeartbeat Embedding 阶段节流心跳：距上次超过阈值才写库一次；
// 心跳失败 = 执行权失效（Java heartbeat 抛 BusinessException 中断向量化）。
func (c *VectorizeConsumer) throttledEmbeddingHeartbeat(ctx context.Context, kbID int64, attemptID string) error {
	now := time.Now()
	c.lastEmbeddingHeartbeatMu.Lock()
	last := c.lastEmbeddingHeartbeat
	if !last.IsZero() && now.Sub(last) < c.heartbeatThrottle {
		c.lastEmbeddingHeartbeatMu.Unlock()
		return nil
	}
	c.lastEmbeddingHeartbeatMu.Unlock()
	updated, err := c.store.HeartbeatVectorProcessing(ctx, kbID, attemptID, now)
	if err != nil {
		c.logger.Error("向量化心跳失败", slog.Int64("kbId", kbID), slog.String("error", err.Error()))
		return err
	}
	if updated != 1 {
		c.logger.Warn("向量化任务执行权已失效", slog.Int64("kbId", kbID))
		return apperr.New(apperr.CodeInternalError, "向量化任务执行权已失效")
	}
	c.lastEmbeddingHeartbeatMu.Lock()
	c.lastEmbeddingHeartbeat = now
	c.lastEmbeddingHeartbeatMu.Unlock()
	return nil
}

// resetEmbeddingHeartbeat 领取时清零心跳计时。
func (c *VectorizeConsumer) resetEmbeddingHeartbeat() {
	c.lastEmbeddingHeartbeatMu.Lock()
	c.lastEmbeddingHeartbeat = time.Time{}
	c.lastEmbeddingHeartbeatMu.Unlock()
}

// retryMessage 重试重投：PROCESSING → PENDING 条件重置成功才重新投递，否则
// ACK 丢弃（Java retryMessage）；重投失败 PENDING → FAILED（"重试入队失败"）。
func (c *VectorizeConsumer) retryMessage(ctx context.Context, kbID int64, attemptID string, nextRetry int) {
	reset, err := c.store.ResetVectorToPending(ctx, kbID, attemptID, time.Now())
	if err != nil || reset != 1 {
		c.logger.Warn("重试重置失败（任务已完成或状态不再匹配），ACK 丢弃",
			slog.Int64("kbId", kbID), slog.Int("retryCount", nextRetry))
		return
	}
	_, xerr := c.stream.XAddTask(ctx, map[string]any{
		FieldKBID:       strconv.FormatInt(kbID, 10),
		FieldRetryCount: strconv.Itoa(nextRetry),
	})
	if xerr == nil {
		c.logger.Info("向量化任务已重新入队", slog.Int64("kbId", kbID), slog.Int("retryCount", nextRetry))
		return
	}
	c.logger.Error("重试入队失败", slog.Int64("kbId", kbID), slog.String("error", xerr.Error()))
	_, _ = c.store.FailVectorIfPending(ctx, kbID, "重试入队失败", time.Now())
}

func (c *VectorizeConsumer) ack(ctx context.Context, messageID string) {
	if err := c.stream.XAck(ctx, messageID); err != nil {
		c.logger.Error("ACK 失败", slog.String("messageId", messageID), slog.String("error", err.Error()))
	}
}

// parseVectorizePayload 消息字段解析：kbId 缺失或非数字视为格式错误（ACK 丢弃）。
func parseVectorizePayload(values map[string]any) (int64, int, bool) {
	raw, exists := values[FieldKBID]
	if !exists {
		return 0, 0, false
	}
	str := streamValueString(raw)
	kbID, err := strconv.ParseInt(str, 10, 64)
	if err != nil || kbID <= 0 {
		return 0, 0, false
	}
	retryCount := 0
	if rcRaw, exists := values[FieldRetryCount]; exists {
		if n, err := strconv.Atoi(streamValueString(rcRaw)); err == nil && n >= 0 {
			retryCount = n
		}
	}
	return kbID, retryCount, true
}

// streamValueString Stream 值转字符串（go-redis 返回 string 或 []byte）。
func streamValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return ""
	}
}

func isBlank(s string) bool { return strings.TrimSpace(s) == "" }

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// newAttemptID 生成 36 位执行代次（Java UUID.randomUUID().toString()）。
func newAttemptID() string { return uuid.NewString() }
