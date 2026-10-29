// 过期待面试巡检器（Java ScheduleStatusUpdater 同构）：批量把 interview_time
// 已过仍 PENDING 的日程置 CANCELLED。Java 为 @Scheduled(cron = "0 0 * * * ?")
// 每小时整点触发；Go 按裁决 Q3-A 收敛为间隔节拍（Config 默认 1h），首轮不在
// 启动时立即执行（对齐 cron 语义：首个触发点在一个间隔之后）。巡检动作与
// 时钟可注入，测试不真等。
package interviewschedule

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// CancelStore 过期巡检所需的数据面（Store 的巡检子集；生产实现 *Repo，测试注入 fake）。
type CancelStore interface {
	CancelExpiredPending(ctx context.Context, cutoff time.Time) (int64, error)
}

// StatusUpdater 过期待面试巡检器。
type StatusUpdater struct {
	store    CancelStore
	interval time.Duration
	nowFn    func() time.Time // 时钟注入点（默认 time.Now，测试替换）
	logger   *slog.Logger

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}
}

// NewStatusUpdater 创建巡检器；间隔取 Config（SweeperInterval），零值回落 Java
// cron 语义 1 小时。
func NewStatusUpdater(store CancelStore, cfg Config, logger *slog.Logger) *StatusUpdater {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.SweeperInterval <= 0 {
		cfg.SweeperInterval = time.Hour
	}
	return &StatusUpdater{
		store: store, interval: cfg.SweeperInterval, nowFn: time.Now, logger: logger,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
}

// Start 启动巡检循环（后台 goroutine；ctx 结束或 Stop 停止；首轮在一个间隔之后，
// 对齐 Java cron 不立即触发）。
func (u *StatusUpdater) Start(ctx context.Context) {
	go func() {
		defer close(u.doneCh)
		u.logger.Info("面试日程过期巡检已启动", slog.Duration("interval", u.interval))
		ticker := time.NewTicker(u.interval)
		defer ticker.Stop()
		for {
			select {
			case <-u.stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				u.sweepOnce(ctx)
			}
		}
	}()
}

// Stop 停止巡检循环（等待退出）。
func (u *StatusUpdater) Stop() {
	u.stopOnce.Do(func() { close(u.stopCh) })
	select {
	case <-u.doneCh:
	case <-time.After(5 * time.Second):
	}
}

// sweepOnce 单轮巡检（Java updateExpiredInterviews L22-29 逐字）：批量
// PENDING→CANCELLED（interview_time < now），影响行数 >0 记日志。JPQL 批量
// UPDATE 不触发 @PreUpdate → updated_at 不变，repo 侧以 UpdateColumns 对齐。
func (u *StatusUpdater) sweepOnce(ctx context.Context) {
	updated, err := u.store.CancelExpiredPending(ctx, u.nowFn())
	if err != nil {
		u.logger.Error("过期面试巡检失败", slog.String("error", err.Error()))
		return
	}
	if updated > 0 {
		u.logger.Info("已将过期面试标记为已取消", slog.Int64("count", updated))
	}
}
