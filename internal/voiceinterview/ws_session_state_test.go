// sessionState 纯逻辑单测：多段拼接（joinSegments）、合并缓冲、提交覆盖、
// CAS 处理锁与回声冷却。对齐 Java SessionState（L1337-1459）外部行为。
package voiceinterview

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestJoinSegments多段拼接语义 对齐 Java joinSegments L1368-1383 的五条分支。
func TestJoinSegments多段拼接语义(t *testing.T) {
	cases := []struct {
		name     string
		previous string
		next     string
		want     string
	}{
		{"完全相同去重", "你好", "你好", "你好"},
		{"后段包含前段取后段", "你好", "你好，请问", "你好，请问"},
		{"前段以后段结尾保留前段", "你好，请问", "请问", "你好，请问"},
		{"句号结尾空格拼接", "第一个问题。", "第二个话题", "第一个问题。 第二个话题"},
		{"问号结尾空格拼接", "是什么？", "为什么", "是什么？ 为什么"},
		{"感叹号结尾空格拼接", "很好！", "继续", "很好！ 继续"},
		{"英文句点结尾空格拼接", "ok.", "next", "ok. next"},
		{"非句末逗号拼接", "第一个问题", "第二个话题", "第一个问题，第二个话题"},
		{"两侧空白归一", "  前段  ", " 后段 ", "前段，后段"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinSegments(tc.previous, tc.next); got != tc.want {
				t.Errorf("joinSegments(%q, %q) = %q, want %q", tc.previous, tc.next, got, tc.want)
			}
		})
	}
}

// Test最终结果拼入合并缓冲 最终段入缓冲、空段忽略、部分级预览拼接。
func Test最终结果拼入合并缓冲(t *testing.T) {
	s := newSessionState(800 * time.Millisecond)

	s.appendFinalSttSegment("  第一段 ")
	if got := s.mergeBufferPreview(); got != "第一段" {
		t.Errorf("首段入缓冲错误: %q", got)
	}

	// 部分级预览：与缓冲智能拼接，不污染缓冲。
	if got := s.mergeBufferPreviewWithPartial("第二段正在说"); got != "第一段，第二段正在说" {
		t.Errorf("部分预览拼接错误: %q", got)
	}
	if got := s.mergeBufferPreview(); got != "第一段" {
		t.Errorf("部分预览不应改动缓冲: %q", got)
	}
	// 缓冲为空时部分预览取部分文本本身。
	empty := newSessionState(800 * time.Millisecond)
	if got := empty.mergeBufferPreviewWithPartial("实时文本"); got != "实时文本" {
		t.Errorf("空缓冲部分预览错误: %q", got)
	}

	s.appendFinalSttSegment("   ")
	if got := s.mergeBufferPreview(); got != "第一段" {
		t.Errorf("空白段应被忽略: %q", got)
	}

	s.appendFinalSttSegment("第二段终稿")
	if got := s.mergeBufferPreview(); got != "第一段，第二段终稿" {
		t.Errorf("多段拼接缓冲错误: %q", got)
	}
}

// TestSubmit修正文本覆盖缓冲 对齐 Java setMergeBufferDirectly（空白忽略、修剪）。
func TestSubmit修正文本覆盖缓冲(t *testing.T) {
	s := newSessionState(800 * time.Millisecond)
	s.appendFinalSttSegment("识别出的口误")

	s.setMergeBufferDirectly("  修正后的回答  ")
	if got := s.mergeBufferPreview(); got != "修正后的回答" {
		t.Errorf("覆盖缓冲错误: %q", got)
	}

	s.setMergeBufferDirectly("   ")
	if got := s.mergeBufferPreview(); got != "修正后的回答" {
		t.Errorf("空白覆盖应被忽略: %q", got)
	}
}

// TestTakeMergeBufferAndClear 取走即清空（Java takeMergeBufferAndClear）。
func TestTakeMergeBufferAndClear(t *testing.T) {
	s := newSessionState(800 * time.Millisecond)
	s.appendFinalSttSegment("待提交话语")

	taken := s.takeMergeBufferAndClear()
	if taken != "待提交话语" {
		t.Errorf("取走内容错误: %q", taken)
	}
	if got := s.mergeBufferPreview(); got != "" {
		t.Errorf("取走后应清空: %q", got)
	}
}

// Test处理锁CAS防双答 抢占失败返回 false，释放后可再抢。
func Test处理锁CAS防双答(t *testing.T) {
	s := newSessionState(800 * time.Millisecond)
	if !s.tryAcquireProcessing() {
		t.Fatal("首次抢占应成功")
	}
	if s.tryAcquireProcessing() {
		t.Fatal("处理中二次抢占应失败")
	}
	s.releaseProcessing()
	if !s.tryAcquireProcessing() {
		t.Fatal("释放后应可再抢")
	}
}

// Test回声冷却窗口 AI 说话期间与冷却窗口内判定为「AI 说话或冷却」。
func Test回声冷却窗口(t *testing.T) {
	s := newSessionState(30 * time.Millisecond)

	s.setAiSpeaking(true)
	if !s.isAiSpeakingOrCooldown() {
		t.Error("AI 说话期间应被判定为冷却")
	}

	s.setAiSpeaking(false)
	if s.isAiSpeakingOrCooldown() {
		t.Error("未进冷却前不应判定为冷却")
	}

	s.endAiSpeaking()
	if !s.isAiSpeakingOrCooldown() {
		t.Error("结束说话后冷却窗口内应判定为冷却")
	}
	time.Sleep(50 * time.Millisecond)
	if s.isAiSpeakingOrCooldown() {
		t.Error("冷却窗口过后不应再判定为冷却")
	}
}

// Test回合取消函数 断连时取消进行中的回合（Java processingThread.interrupt 语义）。
func Test回合取消函数(t *testing.T) {
	s := newSessionState(800 * time.Millisecond)
	s.cancelTurn() // 无回合时空操作，不应 panic

	ctx, cancel := context.WithCancel(context.Background())
	s.setTurnCancel(cancel)
	s.cancelTurn()
	select {
	case <-ctx.Done():
	default:
		t.Error("cancelTurn 应取消已注册的回合上下文")
	}
	s.cancelTurn() // 清除后再次调用为空操作
}

// Test会话状态并发互斥 并发读写合并缓冲与处理锁无数据竞争（go test -race 覆盖）。
func Test会话状态并发互斥(t *testing.T) {
	s := newSessionState(1 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.appendFinalSttSegment("段")
				_ = s.mergeBufferPreviewWithPartial("部分")
				s.setMergeBufferDirectly("覆盖")
				s.markSttActivity()
				s.tryAcquireProcessing()
				s.releaseProcessing()
				s.setAiSpeaking(true)
				s.endAiSpeaking()
				_ = s.isAiSpeakingOrCooldown()
				s.takeMergeBufferAndClear()
			}
		}()
	}
	wg.Wait()
}
