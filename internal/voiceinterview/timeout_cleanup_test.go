// 票 09 单测与管道测试：不活动巡检（4:30 预警一次 / 5:00 暂停断连停 ASR）、
// 僵尸会话清理（IN_PROGRESS 超 2h 强制结束+评估投递，以 DB 为准）、优雅关闭关闭
// 全部活动连接。契约来源：Java VoiceInterviewWebSocketHandler checkPauseTimeout
// （L1133-1151）/ sendPauseWarning（L1169-1179）/ handlePauseTimeout（L1185-1217）
// 与 VoiceInterviewService.cleanupStaleSessions 第一段（L739-752）逐字。
// 巡检节奏与时钟可注入（sweepOnce + 注入时钟），测试不真等。
package voiceinterview

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// ===== 纯函数：不活动巡检动作判定 =====

// 边界判定（Java checkPauseTimeout L1141-1149 逐字）：elapsed 严格大于预警窗口且
// 严格小于暂停窗口 → 预警；大于等于暂停窗口 → 暂停；其余不动。
func TestInactivityAction边界判定(t *testing.T) {
	warn := 270 * time.Second  // 4:30
	pause := 300 * time.Second // 5:00
	cases := []struct {
		name    string
		elapsed time.Duration
		want    string
	}{
		{"刚建连不动", 0, inactivityNone},
		{"四分二十九秒不动", 269 * time.Second, inactivityNone},
		{"恰好四分三十秒不动（严格大于）", warn, inactivityNone},
		{"四分三十秒又一纳秒预警", warn + time.Nanosecond, inactivityWarn},
		{"四分五十九秒预警", 299 * time.Second, inactivityWarn},
		{"恰好五分钟暂停（大于等于）", pause, inactivityPause},
		{"五分零一秒暂停", pause + time.Second, inactivityPause},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inactivityAction(tc.elapsed, warn, pause); got != tc.want {
				t.Errorf("inactivityAction(%v) = %q, want %q", tc.elapsed, got, tc.want)
			}
		})
	}
}

// ===== 夹具：僵尸清理（Service 层，注入时钟） =====

// addSessionAt 以固定 StartTime 加一场指定状态的会话（时钟注入，不用 time.Now）。
func (f *recoveryFixture) addSessionAt(status VoiceSessionStatus, startAgo time.Duration) int64 {
	start := f.now.Add(-startAgo)
	st := status
	phase := PhaseTech
	e := &SessionEntity{
		UserID:          stringPtr(DefaultUserID),
		RoleType:        "java-backend",
		CurrentPhase:    &phase,
		Status:          &st,
		StartTime:       &start,
		PlannedDuration: intPtr(30),
	}
	return f.store.addSession(e)
}

// 僵尸清理主路径：IN_PROGRESS 超 2h 强制结束——COMPLETED + endTime + actualDuration +
// 评估 PENDING 落库，提交后投递评估流；未超 2h 的进行中会话不动。
func TestEndStaleInProgressSessions超时强制结束并投递评估(t *testing.T) {
	f := newRecoveryFixture()
	staleID := f.addSessionAt(VoiceStatusInProgress, 3*time.Hour)
	freshID := f.addSessionAt(VoiceStatusInProgress, 1*time.Hour) // 未超阈不动

	cleaned, err := f.svc.EndStaleInProgressSessions(context.Background(), f.now)
	if err != nil {
		t.Fatalf("僵尸清理失败: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("应强制结束 1 场, got %d", cleaned)
	}
	if len(f.store.updateEnd) != 1 {
		t.Fatalf("应结束一次, 实际 %d 次: %v", len(f.store.updateEnd), f.rec.list())
	}
	e := f.store.updateEnd[0]
	if e.ID != staleID {
		t.Errorf("应结束超时场次 %d, 实际 %d", staleID, e.ID)
	}
	if statusEnum(e.Status) != VoiceStatusCompleted {
		t.Errorf("应置 COMPLETED: %v", e.Status)
	}
	if e.EndTime == nil {
		t.Error("应写 endTime")
	}
	if e.ActualDuration == nil || *e.ActualDuration != 3*60*60 {
		t.Errorf("actualDuration 应为 3h 秒数 10800: %v", e.ActualDuration)
	}
	if e.EvaluateStatus == nil || *e.EvaluateStatus != VoiceEvaluatePending {
		t.Errorf("应置评估 PENDING: %v", e.EvaluateStatus)
	}
	// 事务提交后投递（先落库后 XADD，复用结束语义顺序）。
	assertEventOrder(t, f.rec.list(), "update-end", "xadd")
	if len(f.stream.adds) != 1 {
		t.Fatalf("应投递一条评估任务, got %d", len(f.stream.adds))
	}
	task := f.stream.addAt(0)
	if task[VoiceEvalFieldSessionID] != strconv.FormatInt(staleID, 10) {
		t.Errorf("投递的会话 ID 错误: %v", task[VoiceEvalFieldSessionID])
	}
	_ = freshID
}

// 非 IN_PROGRESS（PAUSED/COMPLETED）即使超时也不动；恰好 2h 不算超时
// （Java findByStatusAndStartTimeBefore 严格小于）。
func TestEndStaleInProgressSessions非进行中与未满阈值不动(t *testing.T) {
	f := newRecoveryFixture()
	f.addSessionAt(VoiceStatusPaused, 5*time.Hour)
	f.addSessionAt(VoiceStatusCompleted, 5*time.Hour)
	f.addSessionAt(VoiceStatusInProgress, 2*time.Hour) // 恰好阈值，严格小于不算

	cleaned, err := f.svc.EndStaleInProgressSessions(context.Background(), f.now)
	if err != nil {
		t.Fatalf("僵尸清理失败: %v", err)
	}
	if cleaned != 0 {
		t.Fatalf("不应有任何清理, got %d", cleaned)
	}
	if len(f.store.updateEnd) != 0 || len(f.stream.adds) != 0 {
		t.Errorf("不应有结束或投递: end=%v adds=%d", f.store.updateEnd, len(f.stream.adds))
	}
}

// 以 DB 为准不受缓存影响：缓存里是"刚恢复的进行中"快照，DB 行是超时未结束的
// 僵尸行 → 仍按 DB 强制结束（Java cleanupStaleSessions 直查 repository）。
func TestEndStaleInProgressSessions以DB为准不受缓存影响(t *testing.T) {
	f := newRecoveryFixture()
	staleID := f.addSessionAt(VoiceStatusInProgress, 3*time.Hour)

	// 缓存投毒：同一场次在缓存中是 1 分钟前才开始的新快照。
	freshStart := f.now.Add(-time.Minute)
	fresh := &SessionEntity{
		ID:        staleID,
		RoleType:  "java-backend",
		Status:    statusPtr(VoiceStatusInProgress),
		StartTime: &freshStart,
	}
	if err := f.cache.Save(context.Background(), entityToCached(fresh)); err != nil {
		t.Fatalf("写缓存失败: %v", err)
	}

	cleaned, err := f.svc.EndStaleInProgressSessions(context.Background(), f.now)
	if err != nil {
		t.Fatalf("僵尸清理失败: %v", err)
	}
	if cleaned != 1 || len(f.store.updateEnd) != 1 {
		t.Fatalf("应无视缓存按 DB 强制结束 1 场: cleaned=%d end=%v", cleaned, f.store.updateEnd)
	}
	if f.store.updateEnd[0].ID != staleID {
		t.Errorf("应结束场次 %d, 实际 %d", staleID, f.store.updateEnd[0].ID)
	}
}

// NewService 零值 Recovery 补 Java 默认 2h（cleanupStaleSessions minusHours(2)）。
func TestNewService僵尸窗口默认2小时(t *testing.T) {
	svc := NewService(ServiceDeps{Logger: discardLogger()})
	if svc.recovery.StaleInProgressAfter != 2*time.Hour {
		t.Errorf("StaleInProgressAfter 默认应为 2h, got %v", svc.recovery.StaleInProgressAfter)
	}
}

// ===== 夹具：不活动巡检（管道，注入时钟） =====

// sweeperFixture 管道夹具 + 可控时钟的巡检器。
type sweeperFixture struct {
	*gatewayFixture
	sw    *InactivitySweeper
	clock func() time.Time
	set   func(time.Time)
}

func newSweeperFixture(t *testing.T, warnAfter, pauseAfter time.Duration) *sweeperFixture {
	gf := newGatewayFixture(t, func(cfg *Config, _ *[]string) {
		cfg.InactivityWarnAfter = warnAfter
		cfg.InactivityPauseAfter = pauseAfter
	})
	var current time.Time = time.Now()
	sf := &sweeperFixture{gatewayFixture: gf}
	sf.clock = func() time.Time { return current }
	sf.set = func(v time.Time) { current = v }
	sf.sw = NewInactivitySweeper(gf.gw, cfgOf(gf.gw), discardLogger())
	sf.sw.nowFn = sf.clock
	return sf
}

// cfgOf 读网关实际生效的配置（构造巡检器用）。
func cfgOf(gw *Gateway) Config { return gw.cfg }

// countControlActions 静置 quiet 后非阻塞清点队列中各 control action 数。
func (c *wsClient) countControlActions(t *testing.T, quiet time.Duration) map[string]int {
	t.Helper()
	time.Sleep(quiet)
	counts := map[string]int{}
	for {
		select {
		case m := <-c.ch:
			if m["type"] == "control" {
				action, _ := m["action"].(string)
				counts[action]++
			}
		default:
			return counts
		}
	}
}

// NewInactivitySweeper 窗口与节奏常量对齐 Java：零值回落 30s 巡检 / 4:30 预警 /
// 5:00 暂停；显式配置原样生效（节奏可注入）。
func TestNewInactivitySweeper窗口与节奏常量(t *testing.T) {
	t.Run("零值回落Java常量", func(t *testing.T) {
		sw := NewInactivitySweeper(nil, Config{}, discardLogger())
		if sw.interval != 30*time.Second || sw.warnAfter != 270*time.Second || sw.pauseAfter != 300*time.Second {
			t.Errorf("默认值错误: interval=%v warn=%v pause=%v", sw.interval, sw.warnAfter, sw.pauseAfter)
		}
	})
	t.Run("显式配置生效", func(t *testing.T) {
		sw := NewInactivitySweeper(nil, Config{
			InactivityCheckInterval: 5 * time.Millisecond,
			InactivityWarnAfter:     10 * time.Millisecond,
			InactivityPauseAfter:    20 * time.Millisecond,
		}, discardLogger())
		if sw.interval != 5*time.Millisecond || sw.warnAfter != 10*time.Millisecond || sw.pauseAfter != 20*time.Millisecond {
			t.Errorf("显式配置未生效: interval=%v warn=%v pause=%v", sw.interval, sw.warnAfter, sw.pauseAfter)
		}
	})
}

// 超时暂停完整路径（Java handlePauseTimeout L1185-1217 步骤逐字）：
// 预警窗口内推一次 pause_timeout_warning（多轮巡检不重复）→ 达到暂停窗口推
// pause_timeout → 会话 PAUSED 落库（update-pause）→ 服务端断连 → ASR 停止；
// 全程不结束会话（PAUSED 令断开自动结束路径空操作）、不投递评估。
func TestGateway暂停超时预警与超时暂停路径(t *testing.T) {
	sf := newSweeperFixture(t, 200*time.Millisecond, 400*time.Millisecond)
	base := sf.clock()
	id := sf.f.store.addSession(inProgressSession(time.Hour))
	c := sf.mustWSClient(t, id)
	c.drainEstablish(t)

	// ① 预警窗口：一轮巡检推一次 pause_timeout_warning（话术逐字）。
	sf.set(base.Add(300 * time.Millisecond))
	sf.sw.sweepOnce()
	warn := c.nextOfAction(t, "pause_timeout_warning", 2*time.Second)
	if warn["message"] != "会话将在30秒后暂停，请继续说话或点击继续" {
		t.Errorf("预警话术错误: %v", warn)
	}

	// ② 同一不活动窗口内再巡检两轮：不再重复预警。
	sf.set(base.Add(310 * time.Millisecond))
	sf.sw.sweepOnce()
	sf.sw.sweepOnce()
	if counts := c.countControlActions(t, 300*time.Millisecond); counts["pause_timeout_warning"] != 0 {
		t.Errorf("同一窗口内预警只推一次, 多推: %v", counts)
	}

	// ③ 达到暂停窗口：pause_timeout（话术逐字，断连前送达）。
	sf.set(base.Add(500 * time.Millisecond))
	sf.sw.sweepOnce()
	timeoutMsg := c.nextOfAction(t, "pause_timeout", 2*time.Second)
	if timeoutMsg["message"] != "会话因超时已暂停,可在历史记录中恢复" {
		t.Errorf("超时话术错误: %v", timeoutMsg)
	}

	// ④ 会话以 timeout 暂停落库（PAUSED + pausedAt）。
	waitForCond(t, 2*time.Second, "暂停落库", func() bool {
		return len(sf.f.store.updatePause) == 1
	})
	if p := sf.f.store.updatePause[0]; p.id != id {
		t.Errorf("暂停的会话 ID 错误: %d", p.id)
	}

	// ⑤ 服务端断连 + ASR 停止。
	select {
	case <-c.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("服务端应在暂停超时后断开连接")
	}
	waitForCond(t, 2*time.Second, "ASR 已停止", func() bool { return sf.asr.finishTotal() >= 1 })

	// ⑥ 不结束会话、不投递评估（PAUSED 令断开自动结束为空操作）。
	time.Sleep(300 * time.Millisecond)
	if len(sf.f.store.updateEnd) != 0 {
		t.Errorf("暂停超时不应结束会话: %v", sf.f.store.updateEnd)
	}
	if len(sf.f.stream.adds) != 0 {
		t.Errorf("暂停超时不应投递评估: %v", sf.f.stream.adds)
	}
}

// 活动时间判定对齐：任何上行消息刷新活动时间并重置预警——预警后发上行音频，
// 活动时间前移（未触发暂停路径）、预警重新武装可再推一次。
func TestGateway上行消息刷新活动时间并重置预警(t *testing.T) {
	sf := newSweeperFixture(t, 200*time.Millisecond, 600*time.Millisecond)
	base := sf.clock()
	id := sf.f.store.addSession(inProgressSession(time.Hour))
	c := sf.mustWSClient(t, id)
	c.drainEstablish(t)

	// 预警一次。
	sf.set(base.Add(300 * time.Millisecond))
	sf.sw.sweepOnce()
	c.nextOfAction(t, "pause_timeout_warning", 2*time.Second)

	// 上行音频刷新活动时间（读泵 touchActivity）。
	sendWSMsg(t, c.conn, audioFrame(t, []byte{1, 2, 3, 4}))
	waitForCond(t, 3*time.Second, "音频已转发 ASR", func() bool {
		return len(sf.asr.appendPayloads()) == 1
	})
	var lastAct time.Time
	waitForCond(t, 2*time.Second, "活动时间已刷新", func() bool {
		live := sf.gw.liveSnapshot()
		for _, s := range live {
			if s.id == id && s.lastActivityAt().After(base) {
				lastAct = s.lastActivityAt()
				return true
			}
		}
		return false
	})

	// 活动后经过 550ms（小于暂停窗口 600ms）：若活动时间未刷新会触发暂停，
	// 实际应再次预警（预警随活动重新武装）。
	sf.set(lastAct.Add(550 * time.Millisecond))
	sf.sw.sweepOnce()
	warn2 := c.nextOfAction(t, "pause_timeout_warning", 2*time.Second)
	if warn2 == nil {
		t.Fatal("活动刷新后应能再次预警")
	}
	if len(sf.f.store.updatePause) != 0 {
		t.Errorf("刷新后不应触发暂停: %v", sf.f.store.updatePause)
	}
}

// 暂停失败（会话已非 IN_PROGRESS）按 Java catch 语义中止后续步骤：pause_timeout
// 已下发但不断连、不落库，连接保留可用（Java handlePauseTimeout L1214-1216）。
func TestGateway暂停失败时连接保留(t *testing.T) {
	sf := newSweeperFixture(t, 200*time.Millisecond, 400*time.Millisecond)
	base := sf.clock()
	id := sf.f.store.addSession(inProgressSession(time.Hour))
	c := sf.mustWSClient(t, id)
	c.drainEstablish(t)

	// 先正常结束会话（连接保留），巡检触发暂停时状态守卫失败。
	sendWSMsg(t, c.conn, map[string]any{"type": "control", "action": "end_interview"})
	waitForCond(t, 3*time.Second, "会话已结束", func() bool {
		return len(sf.f.store.updateEnd) == 1
	})

	sf.set(base.Add(500 * time.Millisecond))
	sf.sw.sweepOnce()
	c.nextOfAction(t, "pause_timeout", 2*time.Second) // 话术先发
	time.Sleep(200 * time.Millisecond)
	if len(sf.f.store.updatePause) != 0 {
		t.Errorf("非 IN_PROGRESS 不应暂停落库: %v", sf.f.store.updatePause)
	}

	// 连接保留：上行音频仍转发 ASR。
	sendWSMsg(t, c.conn, audioFrame(t, []byte{5, 6, 7, 8}))
	waitForCond(t, 3*time.Second, "连接仍可用（音频转发）", func() bool {
		return len(sf.asr.appendPayloads()) == 1
	})
}

// 优雅关闭：关闭全部活动连接（两场并发连接都走 04 断开自动结束路径）——
// 全部落库结束 + 评估投递 + ASR 停止。
func TestGateway优雅关闭关闭全部活动连接(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id1 := gf.f.store.addSession(inProgressSession(time.Hour))
	id2 := gf.f.store.addSession(inProgressSession(time.Hour))
	c1 := gf.mustWSClient(t, id1)
	c2 := gf.mustWSClient(t, id2)
	c1.drainEstablish(t)
	c2.drainEstablish(t)

	gf.gw.Close()

	waitForCond(t, 3*time.Second, "两场会话均自动结束", func() bool {
		return len(gf.f.store.updateEnd) == 2
	})
	ended := map[int64]bool{}
	for _, e := range gf.f.store.updateEnd {
		ended[e.ID] = true
	}
	if !ended[id1] || !ended[id2] {
		t.Errorf("两场都应结束: %v", ended)
	}
	waitForCond(t, 2*time.Second, "两场 ASR 均停止", func() bool {
		return gf.asr.finishTotal() >= 2
	})
	waitForCond(t, 2*time.Second, "评估均已投递", func() bool {
		gf.f.stream.mu.Lock()
		defer gf.f.stream.mu.Unlock()
		return len(gf.f.stream.adds) == 2
	})
	for _, c := range []*wsClient{c1, c2} {
		select {
		case <-c.closed:
		case <-time.After(2 * time.Second):
			t.Fatal("优雅关闭应断开全部连接")
		}
	}
}
