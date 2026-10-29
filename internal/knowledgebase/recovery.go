// 知识库向量化卡住任务恢复调度器（Java VectorizeRecoveryScheduler 逐字移植）：
// 60s 巡检；PENDING 超阈值补投；PROCESSING 超阈值无心跳先条件重置回 PENDING 再补投；
// recovery_count 达上限转 FAILED。恢复前条件更新原子推进 vector_updated_at，
// 保证一个调度周期内同一实体只补投一次；外部投递在事务外完成。
package knowledgebase

import (
	"context"
	"log/slog"
	"strconv"
	"time"
)

// RecoveryScheduler 恢复调度器。
type RecoveryScheduler struct {
	store    Store
	producer *VectorizeProducer
	cfg      RecoveryConfig
	logger   *slog.Logger
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewRecoveryScheduler 创建恢复调度器。
func NewRecoveryScheduler(store Store, producer *VectorizeProducer, cfg RecoveryConfig, logger *slog.Logger) *RecoveryScheduler {
	return &RecoveryScheduler{store: store, producer: producer, cfg: cfg, logger: logger,
		stopCh: make(chan struct{}), doneCh: make(chan struct{})}
}

// Start 启动调度（初始延迟 = Interval）。
func (s *RecoveryScheduler) Start(ctx context.Context) {
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
				s.RecoverStuckTasks(ctx, time.Now())
				timer.Reset(s.cfg.Interval)
			}
		}
	}()
}

// Stop 停止调度。
func (s *RecoveryScheduler) Stop() {
	close(s.stopCh)
	select {
	case <-s.doneCh:
	case <-time.After(3 * time.Second):
	}
}

// RecoverStuckTasks 一轮巡检（now 注入时钟，供测试）。
func (s *RecoveryScheduler) RecoverStuckTasks(ctx context.Context, now time.Time) {
	if !s.cfg.Enabled {
		return
	}
	stalePending, err := s.store.FindStaleByVectorStatus(ctx, VectorPending, now.Add(-s.cfg.PendingThreshold), s.cfg.BatchSize)
	if err != nil {
		s.logger.Error("扫描 PENDING 卡住任务失败", slog.String("error", err.Error()))
	}
	for _, kb := range stalePending {
		s.recover(ctx, kb.ID, true, now)
	}
	staleProcessing, err := s.store.FindStaleByVectorStatus(ctx, VectorProcessing, now.Add(-s.cfg.ProcessingThreshold), s.cfg.BatchSize)
	if err != nil {
		s.logger.Error("扫描 PROCESSING 卡住任务失败", slog.String("error", err.Error()))
	}
	for _, kb := range staleProcessing {
		// PROCESSING 无进展：先条件重置回 PENDING，再走统一的补投路径。
		reset, rerr := s.store.ResetStaleVectorProcessing(ctx, kb.ID, now.Add(-s.cfg.ProcessingThreshold), now)
		if rerr != nil {
			continue
		}
		if reset == 1 {
			s.recover(ctx, kb.ID, false, now)
		}
	}
}

func (s *RecoveryScheduler) recover(ctx context.Context, kbID int64, alreadyPending bool, now time.Time) {
	if alreadyPending {
		touched, err := s.store.TouchQueuedVectorForRecovery(ctx, kbID, now.Add(-s.cfg.PendingThreshold), now)
		if err != nil || touched != 1 {
			return // 已被其他路径推进或恢复
		}
	}
	if err := s.store.IncrementVectorRecoveryCount(ctx, kbID); err != nil {
		return
	}
	kb, err := s.store.FindByID(ctx, kbID)
	if err != nil {
		return
	}
	if kb.VectorRecoveryCount >= s.cfg.MaxRecoveryCount {
		_, _ = s.store.FailVectorIfPending(ctx, kbID,
			"自动恢复次数达到上限（"+strconv.Itoa(kb.VectorRecoveryCount)+"），请手动重试", now)
		s.logger.Warn("向量化任务自动恢复达到上限转 FAILED",
			slog.Int64("kbId", kbID), slog.Int("recoveryCount", kb.VectorRecoveryCount))
		return
	}
	sent := s.producer.SendVectorizeTask(ctx, kbID)
	s.logger.Info("向量化卡住任务已补投",
		slog.Int64("kbId", kbID), slog.Bool("sent", sent), slog.Int("recoveryCount", kb.VectorRecoveryCount))
}
