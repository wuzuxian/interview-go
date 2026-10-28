// 评估恢复与僵尸清理巡检调度器（票 08 建，票 09 并入僵尸段）：按固定间隔调用
// Service.EndStaleInProgressSessions（IN_PROGRESS 超 2h 强制结束 + 评估投递）与
// Service.RecoverStaleEvaluations（PENDING 超 3min 补投、PROCESSING 超 30min 置败），
// 对齐 Java WebSocketHandler 内 @Scheduled(fixedDelay = 60_000) 触发 cleanupStaleSessions
// （L1153-1163）。巡检逻辑本身在 service.go（DB 状态时间戳 + 注入时钟），
// 本文件只负责起停节拍。
package voiceinterview

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// EvalRecoveryScheduler 评估恢复巡检调度器。
type EvalRecoveryScheduler struct {
	svc      *Service
	interval time.Duration
	logger   *slog.Logger

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}
}

// NewEvalRecoveryScheduler 创建调度器；interval 非正时回落 Java 默认 60s。
func NewEvalRecoveryScheduler(svc *Service, interval time.Duration, logger *slog.Logger) *EvalRecoveryScheduler {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &EvalRecoveryScheduler{
		svc: svc, interval: interval, logger: logger,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
}

// Start 启动巡检循环（后台 goroutine，立即执行首轮）。
func (r *EvalRecoveryScheduler) Start(ctx context.Context) {
	go func() {
		defer close(r.doneCh)
		r.logger.Info("语音评估恢复巡检已启动", slog.Duration("interval", r.interval))
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		r.sweepOnce(ctx)
		for {
			select {
			case <-r.stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.sweepOnce(ctx)
			}
		}
	}()
}

// Stop 停止巡检循环（等待退出）。
func (r *EvalRecoveryScheduler) Stop() {
	r.stopOnce.Do(func() { close(r.stopCh) })
	select {
	case <-r.doneCh:
	case <-time.After(5 * time.Second):
	}
}

func (r *EvalRecoveryScheduler) sweepOnce(ctx context.Context) {
	now := time.Now()

	// 票 09 僵尸段：IN_PROGRESS 超 2h 强制结束 + 评估投递（Java cleanupStaleSessions
	// 第一段，与评估恢复共用 60s 节拍）。失败记日志后继续评估段（两段相互独立）。
	ended, err := r.svc.EndStaleInProgressSessions(ctx, now)
	if err != nil {
		r.logger.Error("语音僵尸会话清理失败", slog.String("error", err.Error()))
	}
	if ended > 0 {
		r.logger.Info("语音僵尸会话清理完成", slog.Int("cleaned", ended))
	}

	cleaned, err := r.svc.RecoverStaleEvaluations(ctx, now)
	if err != nil {
		r.logger.Error("语音评估恢复巡检失败", slog.String("error", err.Error()))
		return
	}
	if cleaned > 0 {
		r.logger.Info("语音评估恢复巡检完成", slog.Int("cleaned", cleaned))
	}
}
