package resume

import (
	"context"
	"log/slog"
	"time"
)

// RecoveryConfig 卡住任务恢复配置，键与 Java 版 app.async.recovery.resume-analyze 对齐。
type RecoveryConfig struct {
	Enabled             bool
	PendingThreshold    time.Duration
	ProcessingThreshold time.Duration
	Interval            time.Duration
	BatchSize           int
	MaxRecoveryCount    int
}

// DefaultRecoveryConfig 默认值与 application.yml 一致。
func DefaultRecoveryConfig() RecoveryConfig {
	return RecoveryConfig{
		Enabled:             true,
		PendingThreshold:    10 * time.Minute,
		ProcessingThreshold: 15 * time.Minute,
		Interval:            time.Minute,
		BatchSize:           10,
		MaxRecoveryCount:    3,
	}
}

// RecoveryScheduler 卡住任务恢复调度器，对应 Java ResumeAnalysisRecoveryScheduler：
// 每 Interval 扫描超时的 PENDING / PROCESSING 任务并补投。
type RecoveryScheduler struct {
	repo     *Repo
	producer *StreamProducerImpl
	cfg      RecoveryConfig
	logger   *slog.Logger
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewRecoveryScheduler 创建恢复调度器。
func NewRecoveryScheduler(repo *Repo, producer *StreamProducerImpl, cfg RecoveryConfig, logger *slog.Logger) *RecoveryScheduler {
	return &RecoveryScheduler{
		repo: repo, producer: producer, cfg: cfg, logger: logger,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
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
				s.recoverOnce(ctx)
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

func (s *RecoveryScheduler) recoverOnce(ctx context.Context) {
	if !s.cfg.Enabled {
		return
	}
	now := time.Now()

	// PENDING 超阈值 → 触碰时间后补投
	stalePending, err := s.repo.FindStaleByAnalyzeStatus(ctx, StatusPending, now.Add(-s.cfg.PendingThreshold), s.cfg.BatchSize)
	if err != nil {
		s.logger.Error("扫描 PENDING 卡住任务失败", slog.String("error", err.Error()))
	}
	for _, r := range stalePending {
		s.recover(ctx, r.ID, true)
	}

	// PROCESSING 无进展超阈值 → 重置 PENDING 后补投
	staleProcessing, err := s.repo.FindStaleByAnalyzeStatus(ctx, StatusProcessing, now.Add(-s.cfg.ProcessingThreshold), s.cfg.BatchSize)
	if err != nil {
		s.logger.Error("扫描 PROCESSING 卡住任务失败", slog.String("error", err.Error()))
	}
	for _, r := range staleProcessing {
		reset, rerr := s.repo.ResetStaleAnalyzeProcessing(ctx, r.ID, now.Add(-s.cfg.ProcessingThreshold), now)
		if rerr != nil {
			continue
		}
		if reset == 1 {
			s.recover(ctx, r.ID, false)
		}
	}
}

func (s *RecoveryScheduler) recover(ctx context.Context, resumeID int64, alreadyPending bool) {
	now := time.Now()
	if alreadyPending {
		touched, err := s.repo.TouchQueuedAnalyzeForRecovery(ctx, resumeID, now.Add(-s.cfg.PendingThreshold), now)
		if err != nil || touched != 1 {
			return
		}
	}
	if err := s.repo.IncrementAnalyzeRecoveryCount(ctx, resumeID); err != nil {
		return
	}
	r, err := s.repo.FindByID(ctx, resumeID)
	if err != nil {
		return
	}
	if r.AnalyzeRecoveryCount >= s.cfg.MaxRecoveryCount {
		_, _ = s.repo.FailAnalyzeIfPending(ctx, resumeID,
			"自动恢复次数达到上限（"+fmtInt(r.AnalyzeRecoveryCount)+"），请手动重试", now)
		s.logger.Warn("简历分析任务自动恢复达到上限转 FAILED",
			slog.Int64("resumeId", resumeID), slog.Int("recoveryCount", r.AnalyzeRecoveryCount))
		return
	}
	sent := s.producer.SendAnalyzeTask(ctx, resumeID)
	s.logger.Info("简历分析卡住任务已补投",
		slog.Int64("resumeId", resumeID), slog.Bool("sent", sent), slog.Int("recoveryCount", r.AnalyzeRecoveryCount))
}
