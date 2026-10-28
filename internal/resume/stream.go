package resume

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/docparse"
)

// Redis Stream 常量，与 Java 版 AsyncTaskStreamConstants 对齐。
const (
	StreamKey        = "resume:analyze:stream"
	GroupName        = "analyze-group"
	ConsumerPrefix   = "analyze-consumer-"
	FieldResumeID    = "resumeId"
	FieldRetryCount  = "retryCount"
	MaxRetryCount    = 3
	BatchSize        = 10
	PollInterval     = 1 * time.Second
	PendingIdleTime  = 5 * time.Minute
	PendingClaimSize = 10
	StreamMaxLen     = 1000
)

// XAdder Stream 投递能力（*redis.Client 实现）。
type XAdder interface {
	XAdd(ctx context.Context, args *redis.XAddArgs) *redis.StringCmd
}

// StreamProducerImpl 简历分析任务生产者，对应 Java AnalyzeStreamProducer。
type StreamProducerImpl struct {
	rdb    XAdder
	repo   *Repo
	logger *slog.Logger
}

// NewStreamProducer 创建生产者。
func NewStreamProducer(rdb XAdder, repo *Repo, logger *slog.Logger) *StreamProducerImpl {
	return &StreamProducerImpl{rdb: rdb, repo: repo, logger: logger}
}

// SendAnalyzeTask 投递任务；失败时 PENDING → FAILED（终态保护），返回是否投递成功。
func (p *StreamProducerImpl) SendAnalyzeTask(ctx context.Context, resumeID int64) bool {
	err := p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamKey,
		MaxLen: StreamMaxLen,
		Approx: true,
		Values: map[string]any{
			FieldResumeID:   fmtInt64(resumeID),
			FieldRetryCount: "0",
		},
	}).Err()
	if err == nil {
		return true
	}
	p.logger.Error("分析任务投递失败", slog.Int64("resumeId", resumeID), slog.String("error", err.Error()))
	if _, ferr := p.repo.FailAnalyzeIfPending(ctx, resumeID, "任务投递失败: "+err.Error(), time.Now()); ferr != nil {
		p.logger.Error("投递失败标记 FAILED 失败", slog.Int64("resumeId", resumeID), slog.String("error", ferr.Error()))
	}
	return false
}

// Consumer 简历分析 Stream 消费者，对应 Java AnalyzeStreamConsumer + AbstractStreamConsumer：
// 单线程循环 XREADGROUP，pending 消息超时后 XAUTOCLAIM 回收，最多重试 3 次。
type Consumer struct {
	rdb     *redis.Client
	repo    *Repo
	grading *GradingService
	history *HistoryService
	upload  *Uploader
	vector  ResumeVectorizer
	logger  *slog.Logger

	consumerName string
	stopCh       chan struct{}
	stopOnce     sync.Once
	doneCh       chan struct{}
}

// ResumeVectorizer 分析完成后的向量化回调整口（pgvector 可选步骤）。
type ResumeVectorizer interface {
	UpsertResumeEmbedding(ctx context.Context, resumeID int64, content string) error
}

// NewConsumer 创建消费者。
func NewConsumer(rdb *redis.Client, repo *Repo, grading *GradingService, history *HistoryService, upload *Uploader, vector ResumeVectorizer, logger *slog.Logger) *Consumer {
	name := ConsumerPrefix + uuid.NewString()[:8]
	return &Consumer{
		rdb: rdb, repo: repo, grading: grading, history: history, upload: upload, vector: vector,
		logger: logger, consumerName: name,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
}

// Start 启动消费循环（后台 goroutine）。
func (c *Consumer) Start(ctx context.Context) {
	go func() {
		defer close(c.doneCh)
		// 确保 Stream + Group 存在（XGROUP CREATE MKSTREAM）。
		if err := c.rdb.XGroupCreateMkStream(ctx, StreamKey, GroupName, "0").Err(); err != nil {
			if !isBusyGroup(err) {
				c.logger.Warn("创建 Stream Group 失败", slog.String("group", GroupName), slog.String("error", err.Error()))
			}
		}
		c.logger.Info("简历分析消费者已启动", slog.String("consumer", c.consumerName))
		c.loop(ctx)
		c.logger.Info("简历分析消费者已停止", slog.String("consumer", c.consumerName))
	}()
}

// Stop 停止消费循环（等待退出）。
func (c *Consumer) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	select {
	case <-c.doneCh:
	case <-time.After(5 * time.Second):
	}
}

func isBusyGroup(err error) bool {
	return err != nil && err.Error() == "BUSYGROUP Consumer Group name already exists"
}

func (c *Consumer) loop(ctx context.Context) {
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

// consumeOnce 一轮消费：新消息 → pending 回收。
func (c *Consumer) consumeOnce(ctx context.Context) {
	// 1) XREADGROUP '>' 新消息
	res, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    GroupName,
		Consumer: c.consumerName,
		Streams:  []string{StreamKey, ">"},
		Count:    BatchSize,
		Block:    500 * time.Millisecond,
	}).Result()
	if err == nil {
		for _, stream := range res {
			for _, msg := range stream.Messages {
				c.handleMessage(ctx, msg.ID, msg.Values)
			}
		}
		return
	}
	if err != redis.Nil {
		c.logger.Error("消费消息失败", slog.String("error", err.Error()))
	}

	// 2) XAUTOCLAIM 回收超时 pending
	claimed, _, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   StreamKey,
		Group:    GroupName,
		Consumer: c.consumerName,
		MinIdle:  PendingIdleTime,
		Start:    "0-0",
		Count:    PendingClaimSize,
	}).Result()
	if err != nil {
		return
	}
	for _, msg := range claimed {
		c.logger.Info("回收超时 pending 消息", slog.String("messageId", msg.ID))
		c.handleMessage(ctx, msg.ID, msg.Values)
	}
}

func (c *Consumer) handleMessage(ctx context.Context, messageID string, values map[string]any) {
	resumeID, retryCount, ok := parsePayload(values)
	if !ok {
		c.logger.Warn("消息格式错误，跳过", slog.String("messageId", messageID))
		c.ack(ctx, messageID)
		return
	}
	identifier := slog.Int64("resumeId", resumeID)
	c.logger.Info("Processing 简历分析 task", identifier, slog.String("messageId", messageID), slog.Int("retryCount", retryCount))

	if c.shouldSkip(ctx, resumeID) {
		c.ack(ctx, messageID)
		c.logger.Info("简历分析 task skipped", identifier)
		return
	}

	// 条件领取：PENDING → PROCESSING
	attemptID := uuid.NewString()
	claimed, err := c.repo.TryMarkAnalyzeProcessing(ctx, resumeID, attemptID, time.Now())
	if err != nil || claimed == 0 {
		c.ack(ctx, messageID)
		c.logger.Info("简历分析 task was not claimed", identifier)
		return
	}

	if err := c.processBusiness(ctx, resumeID, attemptID); err != nil {
		c.logger.Error("简历分析 task failed", identifier, slog.String("error", err.Error()))
		if retryCount < MaxRetryCount {
			c.retryMessage(ctx, resumeID, attemptID, retryCount+1)
		} else {
			_, _ = c.repo.FailAnalyzeIfProcessing(ctx, resumeID, attemptID,
				"简历分析 failed after retry "+fmtInt(retryCount), time.Now())
		}
		c.ack(ctx, messageID)
		return
	}

	_, _ = c.repo.CompleteAnalyzeIfProcessing(ctx, resumeID, attemptID, time.Now())
	c.ack(ctx, messageID)
	c.logger.Info("简历分析 task completed", identifier)
}

func (c *Consumer) shouldSkip(ctx context.Context, resumeID int64) bool {
	r, err := c.repo.FindByID(ctx, resumeID)
	if err != nil {
		return true // 已删除
	}
	return r.AnalyzeStatus == StatusCompleted
}

func (c *Consumer) processBusiness(ctx context.Context, resumeID int64, attemptID string) error {
	r, err := c.repo.FindByID(ctx, resumeID)
	if err != nil {
		c.logger.Warn("简历已被删除，跳过分析任务", slog.Int64("resumeId", resumeID))
		return nil
	}

	text := derefStr(r.ResumeText)
	if strings.TrimSpace(text) == "" {
		// 历史数据正文为空：从存储恢复文本并回填。
		data, derr := c.upload.storage.Download(ctx, derefStr(r.StorageKey))
		if derr != nil {
			return apperr.New(apperr.CodeResumeParseFailed, "无法获取简历文本内容: "+derr.Error())
		}
		parsed, perr := docparse.Parse(data, r.OriginalFilename)
		if perr != nil {
			return perr
		}
		if strings.TrimSpace(parsed.Text) == "" {
			return apperr.New(apperr.CodeResumeParseFailed, "无法获取简历文本内容")
		}
		// 心跳
		if _, herr := c.repo.HeartbeatAnalyzeProcessing(ctx, resumeID, attemptID, time.Now()); herr != nil {
			return apperr.Internal("简历分析任务执行权已失效")
		}
		text = parsed.Text
		if err := c.repo.UpdateResumeText(ctx, resumeID, text); err != nil {
			return err
		}
	}

	// AI 调用失败（超时/网络/限流）时向上返回错误：触发 Stream 重试
	// （PROCESSING→PENDING 重新入队，最多 3 次；耗尽后转 FAILED，前端可手动重分析），
	// 避免瞬时故障被兜底成 0 分的假"分析完成"记录。
	analysis, err := c.grading.Analyze(ctx, text)
	if err != nil {
		return err
	}
	// LLM 分析完成心跳
	if n, herr := c.repo.HeartbeatAnalyzeProcessing(ctx, resumeID, attemptID, time.Now()); herr != nil || n != 1 {
		return apperr.Internal("简历分析任务执行权已失效")
	}

	if err := c.history.SaveAnalysisIfOwned(ctx, resumeID, attemptID, analysis); err != nil {
		return err
	}

	// pgvector 向量化（尽力而为，失败不影响分析结果）
	if c.vector != nil {
		vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if err := c.vector.UpsertResumeEmbedding(vctx, resumeID, text); err != nil {
			c.logger.Warn("简历向量化失败（不影响分析结果）", slog.Int64("resumeId", resumeID), slog.String("error", err.Error()))
		}
	}
	return nil
}

func (c *Consumer) retryMessage(ctx context.Context, resumeID int64, attemptID string, nextRetry int) {
	// PROCESSING → PENDING 成功才重新投递，否则记录并 ACK 丢弃。
	reset, err := c.repo.ResetAnalyzeToPending(ctx, resumeID, attemptID, time.Now())
	if err != nil || reset != 1 {
		c.logger.Warn("重试重置失败（任务已完成或状态不再匹配），ACK 丢弃",
			slog.Int64("resumeId", resumeID), slog.Int("retryCount", nextRetry))
		return
	}
	err = c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: StreamKey,
		MaxLen: StreamMaxLen,
		Approx: true,
		Values: map[string]any{
			FieldResumeID:   fmtInt64(resumeID),
			FieldRetryCount: fmtInt(nextRetry),
		},
	}).Err()
	if err == nil {
		c.logger.Info("简历分析任务已重新入队", slog.Int64("resumeId", resumeID), slog.Int("retryCount", nextRetry))
		return
	}
	c.logger.Error("重试入队失败", slog.Int64("resumeId", resumeID), slog.String("error", err.Error()))
	// 重投失败标 FAILED，避免停在 PENDING 被恢复调度器反复补投。
	_, _ = c.repo.FailAnalyzeIfPending(ctx, resumeID, "重试入队失败", time.Now())
}

func (c *Consumer) ack(ctx context.Context, messageID string) {
	if err := c.rdb.XAck(ctx, StreamKey, GroupName, messageID).Err(); err != nil {
		c.logger.Error("ACK 失败", slog.String("messageId", messageID), slog.String("error", err.Error()))
	}
}

func parsePayload(values map[string]any) (resumeID int64, retryCount int, ok bool) {
	raw, exists := values[FieldResumeID]
	if !exists {
		return 0, 0, false
	}
	resumeID = parseI64(anyToString(raw))
	if resumeID <= 0 {
		return 0, 0, false
	}
	retryCount = 0
	if rcRaw, exists := values[FieldRetryCount]; exists {
		retryCount = int(parseI64(anyToString(rcRaw)))
	}
	return resumeID, retryCount, true
}

func anyToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return ""
	}
}

func parseI64(s string) int64 {
	var n int64
	neg := false
	for i, ch := range s {
		if i == 0 && ch == '-' {
			neg = true
			continue
		}
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int64(ch-'0')
	}
	if neg {
		return -n
	}
	return n
}

func fmtInt(v int) string {
	return fmtInt64(int64(v))
}

func fmtInt64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
