// sessionState 单条 WebSocket 连接的会话状态（Java VoiceInterviewWebSocketHandler.SessionState
// 同构，L1337-1459）：话语合并缓冲、处理锁（CAS 防双答）、AI 说话标记与回声冷却、
// STT 活动时间。每会话独立实例、独立互斥锁，会话间隔离；跨字段更新在同一锁内完成
// （Java 用多枚 AtomicXXX 组合，这里收敛为一把锁，语义等价且免组合竞态）。
package voiceinterview

import (
	"context"
	"strings"
	"sync"
	"time"
)

// sessionState 会话状态。
type sessionState struct {
	mu sync.Mutex

	cooldown time.Duration // 回声冷却时长（Config.EchoCooldown，Java AI_SPEAK_COOLDOWN_MS=800）

	accumulatedText string             // 本回合已提交、待模型处理的用户文本
	processing      bool               // 处理锁（Java AtomicBoolean processing，CAS 抢占）
	aiSpeaking      bool               // AI 正在说话（Java AtomicBoolean aiSpeaking）
	aiSpeakEndAt    time.Time          // AI 说话结束时刻 + 冷却窗口（Java AtomicLong aiSpeakEndAt）
	mergeBuffer     string             // 多段 STT completed 的合并缓冲
	mergeStartedAt  time.Time          // 缓冲开始计时点；零值表示无进行中话语（Java AtomicLong 0）
	lastSttActivity time.Time          // 最近一次 STT 活动（partial/final）
	turnCancel      context.CancelFunc // 当前模型回合的取消函数（对应 Java processingThread.interrupt）
}

func newSessionState(cooldown time.Duration) *sessionState {
	return &sessionState{cooldown: cooldown, lastSttActivity: time.Now()}
}

// joinSegments 多段识别结果智能拼接（Java joinSegments L1368-1383 逐字）：
// 相同或后段以前段为前缀 → 取后段（识别自我修正）；前段以后段结尾 → 保留前段；
// 前段句末标点收尾 → 空格拼接；否则逗号拼接。
func joinSegments(previous, next string) string {
	trimmedPrevious := strings.TrimSpace(previous)
	trimmedNext := strings.TrimSpace(next)
	if trimmedNext == trimmedPrevious || strings.HasPrefix(trimmedNext, trimmedPrevious) {
		return trimmedNext
	}
	if strings.HasSuffix(trimmedPrevious, trimmedNext) {
		return trimmedPrevious
	}
	if strings.HasSuffix(trimmedPrevious, "。") || strings.HasSuffix(trimmedPrevious, "！") ||
		strings.HasSuffix(trimmedPrevious, "？") || strings.HasSuffix(trimmedPrevious, ".") ||
		strings.HasSuffix(trimmedPrevious, "!") || strings.HasSuffix(trimmedPrevious, "?") {
		return trimmedPrevious + " " + trimmedNext
	}
	return trimmedPrevious + "，" + trimmedNext
}

// appendFinalSttSegment 最终识别结果拼入合并缓冲（Java appendFinalSttSegment L1353-1366）：
// 空白段忽略；空缓冲时开始计时，非空时智能拼接；随后标记 STT 活动。
func (s *sessionState) appendFinalSttSegment(segment string) {
	seg := strings.TrimSpace(segment)
	if seg == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mergeBuffer == "" {
		s.mergeStartedAt = time.Now()
		s.mergeBuffer = seg
	} else {
		s.mergeBuffer = joinSegments(s.mergeBuffer, seg)
	}
	s.lastSttActivity = time.Now()
}

// mergeBufferPreview 合并缓冲当前内容（Java getMergeBufferPreview）。
func (s *sessionState) mergeBufferPreview() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mergeBuffer
}

// setMergeBufferDirectly 提交修正文本直接覆盖缓冲（Java setMergeBufferDirectly L1390-1399）：
// 空白忽略；缓冲原为无话语状态时开始计时。
func (s *sessionState) setMergeBufferDirectly(text string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mergeBuffer = trimmed
	if s.mergeStartedAt.IsZero() {
		s.mergeStartedAt = time.Now()
	}
}

// mergeBufferPreviewWithPartial 部分识别的实时字幕预览（Java
// getMergeBufferPreviewWithPartial L1401-1412）：缓冲为空取部分文本本身，
// 否则与缓冲智能拼接。
func (s *sessionState) mergeBufferPreviewWithPartial(partial string) string {
	current := strings.TrimSpace(partial)
	if current == "" {
		return s.mergeBufferPreview()
	}
	confirmed := s.mergeBufferPreview()
	if confirmed == "" {
		return current
	}
	return joinSegments(confirmed, current)
}

// takeMergeBufferAndClear 取走合并缓冲并清空（Java takeMergeBufferAndClear L1414-1417）。
func (s *sessionState) takeMergeBufferAndClear() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mergeStartedAt = time.Time{}
	taken := s.mergeBuffer
	s.mergeBuffer = ""
	return taken
}

// markSttActivity 标记 STT 活动（Java markSttActivity）。
func (s *sessionState) markSttActivity() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSttActivity = time.Now()
}

// lastSttActivityAt 最近 STT 活动时刻（Java getLastSttActivityAt；票 09 的暂停
// 超时巡检改用任意上行消息的 lastActivityTime，见 ws_gateway.go touchActivity）。
func (s *sessionState) lastSttActivityAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSttActivity
}

// getAccumulatedText 本回合已提交文本（Java getAccumulatedText）。
func (s *sessionState) getAccumulatedText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accumulatedText
}

// setAccumulatedText 回合结束清空/提交时设置（Java setAccumulatedText）。
func (s *sessionState) setAccumulatedText(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accumulatedText = text
}

// tryAcquireProcessing CAS 抢处理锁（Java isProcessing().compareAndSet(false,true)）。
func (s *sessionState) tryAcquireProcessing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.processing {
		return false
	}
	s.processing = true
	return true
}

// releaseProcessing 释放处理锁（Java isProcessing().set(false)）。
func (s *sessionState) releaseProcessing() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processing = false
}

// isProcessing 处理锁是否被持有（Java isProcessing().get()）。
func (s *sessionState) isProcessing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.processing
}

// setTurnCancel 记录当前模型回合的取消函数（对应 Java setProcessingThread，
// 断连时取消进行中的回合管道）。
func (s *sessionState) setTurnCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnCancel = cancel
}

// cancelTurn 取消进行中的模型回合（Java processingThread.interrupt）；无回合时为空操作。
func (s *sessionState) cancelTurn() {
	s.mu.Lock()
	cancel := s.turnCancel
	s.turnCancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// setAiSpeaking 置位/清除 AI 说话标记（Java aiSpeaking.set）。
func (s *sessionState) setAiSpeaking(speaking bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aiSpeaking = speaking
}

// endAiSpeaking AI 说话结束并进入回声冷却（Java finally 块 L845-847：
// aiSpeaking=false; aiSpeakEndAt=now+AI_SPEAK_COOLDOWN_MS）。
func (s *sessionState) endAiSpeaking() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aiSpeaking = false
	s.aiSpeakEndAt = time.Now().Add(s.cooldown)
}

// isAiSpeakingOrCooldown AI 正在说话或处于回声冷却窗口（Java isAiSpeakingOrCooldown
// L1452-1458）：两者为真期间丢弃麦克风输入，防止扬声器回声被识别为用户回答。
func (s *sessionState) isAiSpeakingOrCooldown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aiSpeaking {
		return true
	}
	return time.Now().Before(s.aiSpeakEndAt)
}
