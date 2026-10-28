// 评估任务恢复调度器（票 06）：补投停滞的评估任务，宕机恢复语义不弱于 Java。
// Java 评估流的恢复仅靠 Stream PEL 认领（笔记 06），interview 表无恢复/栅栏列
// （spec 决策 2）；本调度器以 Redis ZSET（member=sessionId，score=最后入队毫秒）
// 记录在队任务：超过阈值仍未终态则重新 XADD（retryCount 归 0）补投，
// 终态（COMPLETED/FAILED）与已删除场次清理出集合。接线方式照 resume 模块的
// RecoveryScheduler（Start/Stop + 周期扫描）。
package interview

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// evalInflightKey 在队任务时间戳 ZSET 键（本仓库约定，Java 无对应物）。
const evalInflightKey = "interview:evaluate:inflight"

// EvalInflightEntry 在队任务记录。
type EvalInflightEntry struct {
	SessionID  string
	EnqueuedAt time.Time
}

// EvalInflightTracker 在队任务时间戳（恢复调度器的停滞判定依据）。
type EvalInflightTracker interface {
	MarkEnqueued(ctx context.Context, sessionID string, at time.Time) error
	Forget(ctx context.Context, sessionID string) error
	Stale(ctx context.Context, before time.Time, limit int) ([]EvalInflightEntry, error)
}

// redisEvalInflight 生产实现（ZSET，score=Unix 毫秒）。
type redisEvalInflight struct {
	rdb redis.Cmdable
}

// NewRedisEvalInflight 创建在队时间戳记录器。
func NewRedisEvalInflight(rdb redis.Cmdable) EvalInflightTracker {
	return &redisEvalInflight{rdb: rdb}
}

func (t *redisEvalInflight) MarkEnqueued(ctx context.Context, sessionID string, at time.Time) error {
	return t.rdb.ZAdd(ctx, evalInflightKey, redis.Z{
		Score:  float64(at.UnixMilli()),
		Member: sessionID,
	}).Err()
}

func (t *redisEvalInflight) Forget(ctx context.Context, sessionID string) error {
	return t.rdb.ZRem(ctx, evalInflightKey, sessionID).Err()
}

func (t *redisEvalInflight) Stale(ctx context.Context, before time.Time, limit int) ([]EvalInflightEntry, error) {
	ids, err := t.rdb.ZRangeByScore(ctx, evalInflightKey, &redis.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(before.UnixMilli(), 10),
		Count: int64(limit),
	}).Result()
	if err != nil {
		return nil, err
	}
	entries := make([]EvalInflightEntry, 0, len(ids))
	for _, id := range ids {
		ms, zerr := t.rdb.ZScore(ctx, evalInflightKey, id).Result()
		if zerr != nil {
			continue
		}
		entries = append(entries, EvalInflightEntry{
			SessionID:  id,
			EnqueuedAt: time.UnixMilli(int64(ms)),
		})
	}
	return entries, nil
}

// EvalRecoveryConfig 评估任务恢复配置（复用 app.async.recovery 段，
// 键与 resume 模块 RecoveryConfig 同源；Java 无评估专用恢复配置）。
type EvalRecoveryConfig struct {
	Enabled             bool
	PendingThreshold    time.Duration
	ProcessingThreshold time.Duration
	Interval            time.Duration
	BatchSize           int
}

// EvalRecoveryScheduler 评估任务恢复调度器：每 Interval 扫描在队时间戳超阈值
// 且仍未终态的场次并补投。恢复计数不上限（interview 表无 recovery 列，
// spec 决策 2；终态后不再扫描，重复补投由 shouldSkip 守卫去重）。
type EvalRecoveryScheduler struct {
	tracker EvalInflightTracker
	store   EvalStore
	stream  EvalStreamClient
	cfg     EvalRecoveryConfig
	logger  *slog.Logger
	stopCh  chan struct{}
	doneCh  chan struct{}
}

// NewEvalRecoveryScheduler 创建恢复调度器。
func NewEvalRecoveryScheduler(tracker EvalInflightTracker, store EvalStore, stream EvalStreamClient,
	cfg EvalRecoveryConfig, logger *slog.Logger) *EvalRecoveryScheduler {
	return &EvalRecoveryScheduler{
		tracker: tracker, store: store, stream: stream, cfg: cfg, logger: logger,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
}

// Start 启动调度（初始延迟 = Interval，照 resume 模块 RecoveryScheduler）。
func (s *EvalRecoveryScheduler) Start(ctx context.Context) {
	go func() {
		defer close(s.doneCh)
		timer := time.NewTimer(s.cfg.Interval)
		defer timer.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-ctx.Done():
				return
			case <-timer.C:
				s.recoverOnce(ctx)
				timer.Reset(s.cfg.Interval)
			}
		}
	}()
}

// Stop 停止调度。
func (s *EvalRecoveryScheduler) Stop() {
	close(s.stopCh)
	select {
	case <-s.doneCh:
	case <-time.After(3 * time.Second):
	}
}

func (s *EvalRecoveryScheduler) recoverOnce(ctx context.Context) {
	if !s.cfg.Enabled {
		return
	}
	now := time.Now()
	maxThreshold := maxDuration(s.cfg.PendingThreshold, s.cfg.ProcessingThreshold)
	entries, err := s.tracker.Stale(ctx, now.Add(-maxThreshold), s.cfg.BatchSize)
	if err != nil {
		s.logger.Error("扫描评估在队任务失败", slog.String("error", err.Error()))
		return
	}
	for _, entry := range entries {
		s.recoverEntry(ctx, entry, now)
	}
}

func (s *EvalRecoveryScheduler) recoverEntry(ctx context.Context, entry EvalInflightEntry, now time.Time) {
	entity, err := s.store.FindBySessionID(ctx, entry.SessionID)
	if err != nil {
		// 已删除场次：清理记录。
		s.forget(ctx, entry.SessionID)
		return
	}
	if entity.EvaluateStatus == nil {
		s.forget(ctx, entry.SessionID)
		return
	}
	var threshold time.Duration
	switch *entity.EvaluateStatus {
	case EvaluatePending:
		threshold = s.cfg.PendingThreshold
	case EvaluateProcess:
		threshold = s.cfg.ProcessingThreshold
	default:
		// 终态（COMPLETED/FAILED）：清理记录。
		s.forget(ctx, entry.SessionID)
		return
	}
	if entry.EnqueuedAt.After(now.Add(-threshold)) {
		return // 尚未停滞
	}

	if _, err := s.stream.XAddTask(ctx, map[string]any{
		EvalFieldSessionID:  entry.SessionID,
		EvalFieldRetryCount: "0",
	}); err != nil {
		s.logger.Error("评估任务补投失败",
			slog.String("sessionId", entry.SessionID), slog.String("error", err.Error()))
		return
	}
	if err := s.tracker.MarkEnqueued(ctx, entry.SessionID, now); err != nil {
		s.logger.Warn("刷新评估在队时间戳失败", slog.String("sessionId", entry.SessionID))
	}
	s.logger.Info("评估卡住任务已补投",
		slog.String("sessionId", entry.SessionID), slog.String("evaluateStatus", string(*entity.EvaluateStatus)))
}

func (s *EvalRecoveryScheduler) forget(ctx context.Context, sessionID string) {
	if err := s.tracker.Forget(ctx, sessionID); err != nil {
		s.logger.Warn("清理评估在队时间戳失败", slog.String("sessionId", sessionID))
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
