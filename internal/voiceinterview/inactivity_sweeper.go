// 不活动巡检器（票 09，Java VoiceInterviewWebSocketHandler.checkPauseTimeout
// @Scheduled(fixedRate = 30000) L1133-1151 同构）：每 30s 扫描活动连接的最近活动
// 时间——超过 4:30 推一次 pause_timeout_warning（Java 每 30s 重复推，票面收敛为
// 同一不活动窗口一次）；达到 5:00 推 pause_timeout、以 timeout 原因暂停落库、
// 服务端断连、停 ASR（Java handlePauseTimeout L1185-1217 步骤逐字）。
// 巡检节奏与时钟可注入（Start 的 interval 取 Config，sweepOnce 的时钟经 nowFn），
// 测试不真等。
package voiceinterview

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Java 默认常量（配置缺省时兜底，逐字对齐 L87-88 与 @Scheduled(fixedRate = 30000)）。
const (
	defaultInactivityCheckInterval = 30 * time.Second  // 巡检节奏
	defaultInactivityWarnAfter     = 270 * time.Second // 4:30 预警
	defaultInactivityPauseAfter    = 300 * time.Second // 5:00 暂停
)

// 不活动巡检动作（inactivityAction 的返回值）。
const (
	inactivityNone  = "none"
	inactivityWarn  = "warn"
	inactivityPause = "pause"
)

// inactivityAction 不活动时长 → 巡检动作（Java checkPauseTimeout L1141-1149 边界
// 逐字）：严格大于预警窗口且严格小于暂停窗口 → 预警；大于等于暂停窗口 → 暂停；
// 其余不动。
func inactivityAction(elapsed, warnAfter, pauseAfter time.Duration) string {
	switch {
	case elapsed > warnAfter && elapsed < pauseAfter:
		return inactivityWarn
	case elapsed >= pauseAfter:
		return inactivityPause
	default:
		return inactivityNone
	}
}

// InactivitySweeper 不活动巡检器。
type InactivitySweeper struct {
	gw         *Gateway
	interval   time.Duration
	warnAfter  time.Duration
	pauseAfter time.Duration
	nowFn      func() time.Time // 时钟注入点（默认 time.Now，测试替换）
	logger     *slog.Logger

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}
}

// NewInactivitySweeper 创建巡检器；窗口与节奏取 Config（Inactivity* 字段），零值
// 回落 Java 常量 30s / 4:30 / 5:00。
func NewInactivitySweeper(gw *Gateway, cfg Config, logger *slog.Logger) *InactivitySweeper {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.InactivityCheckInterval <= 0 {
		cfg.InactivityCheckInterval = defaultInactivityCheckInterval
	}
	if cfg.InactivityWarnAfter <= 0 {
		cfg.InactivityWarnAfter = defaultInactivityWarnAfter
	}
	if cfg.InactivityPauseAfter <= 0 {
		cfg.InactivityPauseAfter = defaultInactivityPauseAfter
	}
	return &InactivitySweeper{
		gw: gw, interval: cfg.InactivityCheckInterval,
		warnAfter: cfg.InactivityWarnAfter, pauseAfter: cfg.InactivityPauseAfter,
		nowFn: time.Now, logger: logger,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
}

// Start 启动巡检循环（后台 goroutine，立即执行首轮；ctx 结束或 Stop 停止）。
func (s *InactivitySweeper) Start(ctx context.Context) {
	go func() {
		defer close(s.doneCh)
		s.logger.Info("语音不活动巡检已启动", slog.Duration("interval", s.interval))
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		s.sweepOnce()
		for {
			select {
			case <-s.stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.sweepOnce()
			}
		}
	}()
}

// Stop 停止巡检循环（等待退出）。
func (s *InactivitySweeper) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	select {
	case <-s.doneCh:
	case <-time.After(5 * time.Second):
	}
}

// sweepOnce 单轮巡检：对每条活动连接按最近活动时间判定预警/暂停
// （Java lastActivityTime.forEach L1137-1150）。
func (s *InactivitySweeper) sweepOnce() {
	now := s.nowFn()
	for _, sess := range s.gw.liveSnapshot() {
		elapsed := now.Sub(sess.lastActivityAt())
		switch inactivityAction(elapsed, s.warnAfter, s.pauseAfter) {
		case inactivityWarn:
			if sess.markPauseWarned() {
				sess.SendPauseTimeoutWarning()
			}
		case inactivityPause:
			s.handlePauseTimeout(sess)
		}
	}
}

// handlePauseTimeout 超时暂停完整路径（Java handlePauseTimeout L1185-1217 步骤逐字）：
// ① 下发 pause_timeout（同步发送，确保断连前送达）→ ② 以 timeout 原因暂停落库 →
// ③ 服务端关闭连接（GOING_AWAY）→ ④ 停 ASR 并移出活动表。暂停失败（如会话已非
// IN_PROGRESS）按 Java catch 语义中止后续步骤，连接保留。断开后的本地清理（取消
// 回合/关发送通道/注销）由读循环的断开路径幂等完成；会话已 PAUSED，断开自动结束
// 路径为空操作，不会误结束。
func (s *InactivitySweeper) handlePauseTimeout(sess *wsSession) {
	s.logger.Warn("会话长时间无活动，执行暂停",
		slog.Int64("sessionId", sess.id), slog.Duration("pauseAfter", s.pauseAfter))

	// ① 下发暂停提示（Java L1188-1196：session.isOpen() 才发）。
	if sess.alive() {
		sess.sendJSONSync(buildPauseTimeout())
	}

	// ② 以 timeout 原因暂停落库（Java L1199：interviewService.pauseSession(sessionId, "timeout")）。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.gw.svc.PauseSession(ctx, sess.id, "timeout"); err != nil {
		s.logger.Error("处理暂停超时失败",
			slog.Int64("sessionId", sess.id), slog.String("error", err.Error()))
		return
	}

	// ③ 服务端关闭连接（Java L1202-1204：session.close(CloseStatus.GOING_AWAY)）；
	// 读循环随之退出并走断开清理。
	if sess.alive() {
		_ = sess.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "pause timeout"),
			time.Now().Add(time.Second))
		_ = sess.conn.Close()
	}

	// ④ 停 ASR 并移出活动表（Java L1206-1210），防止资源泄漏与重复巡检。
	if sess.asr != nil {
		sess.asr.Stop(sess.key)
	}
	s.gw.unregister(sess)
	s.logger.Info("会话已因超时暂停", slog.Int64("sessionId", sess.id))
}
