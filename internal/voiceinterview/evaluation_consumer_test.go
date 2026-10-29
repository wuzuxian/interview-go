// 评估 Stream 消费者单测：顺序契约（跳过非完成态判定 → 置 PROCESSING → 评估 →
// 置 COMPLETED + 落库 → ACK）、失败重投 retryCount+1 上限 3 后置 FAILED、
// 消息字段解析。契约来源：Java VoiceEvaluateStreamConsumer +
// AbstractStreamConsumer.processMessage（L96-144）逐字。
package voiceinterview

import (
	"context"
	"strconv"
	"testing"
)

// consumerFixture 消费者夹具：消费者直连真实 EvaluationService（外部行为串联）。
type consumerFixture struct {
	*evalChainFixture
	consumer *VoiceEvaluateConsumer
}

func newConsumerFixture() *consumerFixture {
	f := &consumerFixture{evalChainFixture: newEvalChainFixture()}
	f.consumer = NewVoiceEvaluateConsumer(f.stream, f.store, f.svc, discardLogger())
	return f
}

// voiceEvalMessage 构造一条 stream 消息。
func voiceEvalMessage(sessionID int64, retryCount int) map[string]any {
	return map[string]any{
		VoiceEvalFieldSessionID:  strconv.FormatInt(sessionID, 10),
		VoiceEvalFieldRetryCount: strconv.Itoa(retryCount),
	}
}

func evalStatusPtr(s VoiceEvaluateStatus) *VoiceEvaluateStatus { return &s }

func TestVoiceConsumerCompletesAndPersists(t *testing.T) {
	f := newConsumerFixture()
	f.store.addSession(evalChainSession(5, evalStatusPtr(VoiceEvaluatePending)))
	f.store.setDialogues(5, []MessageEntity{
		voiceMsg(5, 0, stringPtr("介绍一下你的项目"), stringPtr("回答")),
	})
	f.runner.report = sampleVoiceReport(5)

	f.consumer.handleMessage(context.Background(), "1-1", voiceEvalMessage(5, 0))

	events := f.rec.list()
	// 顺序契约：置 PROCESSING → 评估（落库）→ 置 COMPLETED → ACK。
	assertEventOrder(t, events,
		"update-eval:PROCESSING", "save-eval", "update-eval:COMPLETED", "ack")
	if f.runner.calls != 1 {
		t.Errorf("评估引擎应执行一次, got %d", f.runner.calls)
	}
	if _, ok := f.store.evaluations[5]; !ok {
		t.Error("COMPLETED 前应落 voice_interview_evaluations")
	}
	if len(f.stream.adds) != 0 {
		t.Errorf("成功路径不应重投, got %d 条", len(f.stream.adds))
	}
}

func TestVoiceConsumerSkipsCompleted(t *testing.T) {
	f := newConsumerFixture()
	f.store.addSession(evalChainSession(5, evalStatusPtr(VoiceEvaluateCompleted)))

	f.consumer.handleMessage(context.Background(), "1-1", voiceEvalMessage(5, 0))

	events := f.rec.list()
	if !containsEvent(events, "ack") {
		t.Error("幂等跳过后应 ACK")
	}
	if containsEvent(events, "update-eval:PROCESSING") {
		t.Error("已完成场次不应再置 PROCESSING")
	}
	if f.runner.calls != 0 {
		t.Errorf("已完成场次不应触发评估, got %d 次", f.runner.calls)
	}
}

func TestVoiceConsumerSkipsMissingSession(t *testing.T) {
	f := newConsumerFixture()

	f.consumer.handleMessage(context.Background(), "1-1", voiceEvalMessage(5, 0))

	events := f.rec.list()
	if !containsEvent(events, "ack") {
		t.Error("会话不存在的消息应 ACK 丢弃")
	}
	if containsEvent(events, "update-eval:PROCESSING") {
		t.Error("会话不应被置 PROCESSING")
	}
}

func TestVoiceConsumerSessionDeletedMidflight(t *testing.T) {
	f := newConsumerFixture()
	f.store.addSession(evalChainSession(5, evalStatusPtr(VoiceEvaluatePending)))
	// 第一次 FindSessionByID（跳过检查）可见，其后（processBusiness）已被删除。
	f.store.findFailAfter = 1

	f.consumer.handleMessage(context.Background(), "1-1", voiceEvalMessage(5, 0))

	events := f.rec.list()
	if containsEvent(events, "update-eval:FAILED") {
		t.Error("会话删除属正常跳过，不应置 FAILED")
	}
	if !containsEvent(events, "update-eval:COMPLETED") {
		t.Error("删除路径按成功收尾置 COMPLETED（对已删行更新无效果）")
	}
	if f.runner.calls != 0 {
		t.Errorf("会话已删除不应调用引擎, got %d 次", f.runner.calls)
	}
}

// 会话查询的非「不存在」错误（DB 故障等）不跳过：放行走 processBusiness 报错
// 重投（Java findById 异常传入 processMessage catch 的重试路径），而非 ACK 丢弃。
func TestVoiceConsumer查询错误走重试而非跳过(t *testing.T) {
	f := newConsumerFixture()
	f.store.addSession(evalChainSession(5, evalStatusPtr(VoiceEvaluatePending)))
	f.store.findErr = errEvalBoom

	f.consumer.handleMessage(context.Background(), "1-1", voiceEvalMessage(5, 0))

	events := f.rec.list()
	if f.stream.addCount() != 1 {
		t.Fatalf("查询错误应重投一条消息, got %d 条", f.stream.addCount())
	}
	added := f.stream.addAt(0)
	if got := added[VoiceEvalFieldRetryCount]; got != "1" {
		t.Errorf("重投消息 retryCount = %v, want \"1\"", got)
	}
	if got := added[VoiceEvalFieldSessionID]; got != "5" {
		t.Errorf("重投消息 voiceSessionId = %v, want \"5\"", got)
	}
	if !containsEvent(events, "ack") {
		t.Error("原消息重投后应 ACK")
	}
	if f.runner.calls != 0 {
		t.Errorf("查询错误不应进入评估引擎, got %d 次", f.runner.calls)
	}
	// 状态未置 FAILED（未达重试上限，Java retryCount 语义）。
	if containsEvent(events, "update-eval:FAILED") {
		t.Error("未达上限不应置 FAILED")
	}
}

func TestVoiceConsumerRetryThenFailedAfterLimit(t *testing.T) {
	f := newConsumerFixture()
	f.store.addSession(evalChainSession(5, evalStatusPtr(VoiceEvaluateProcess)))
	f.store.setDialogues(5, []MessageEntity{
		voiceMsg(5, 0, stringPtr("问题"), stringPtr("回答")),
	})
	f.runner.err = errEvalBoom

	// retryCount=0/1/2：重投 retryCount+1，原消息 ACK，不置 FAILED。
	for _, retry := range []int{0, 1, 2} {
		f.consumer.handleMessage(context.Background(), "1-"+strconv.Itoa(retry+1), voiceEvalMessage(5, retry))
		want := strconv.Itoa(retry + 1)
		added := f.stream.addAt(f.stream.addCount() - 1)
		if got := added[VoiceEvalFieldRetryCount]; got != want {
			t.Errorf("retryCount=%d 应重投 retryCount=%s, got %v", retry, want, got)
		}
		if got := added[VoiceEvalFieldSessionID]; got != "5" {
			t.Errorf("重投消息 voiceSessionId = %v, want \"5\"", got)
		}
		if containsEvent(f.rec.list(), "update-eval:FAILED") {
			t.Fatalf("retryCount=%d 未达上限不应置 FAILED", retry)
		}
	}

	// retryCount=3：达到上限，置 FAILED + evaluate_error（Java L135-141 逐字文案）。
	f.consumer.handleMessage(context.Background(), "1-9", voiceEvalMessage(5, 3))
	updates := f.store.evalUpdates
	last := updates[len(updates)-1]
	if last.status != VoiceEvaluateFailed {
		t.Errorf("上限后应置 FAILED, got %s", last.status)
	}
	if last.err == nil || *last.err != "语音面试评估 failed after retry 3" {
		t.Errorf("evaluate_error = %v, want %q", last.err, "语音面试评估 failed after retry 3")
	}
	if f.stream.addCount() != 3 {
		t.Errorf("重投应恰好 3 次, got %d", f.stream.addCount())
	}
	if !containsEvent(f.rec.list(), "ack") {
		t.Error("终态消息应 ACK")
	}
}

func TestVoiceConsumerRetryEnqueueFailureMarksFailed(t *testing.T) {
	f := newConsumerFixture()
	f.store.addSession(evalChainSession(5, evalStatusPtr(VoiceEvaluateProcess)))
	f.store.setDialogues(5, []MessageEntity{
		voiceMsg(5, 0, stringPtr("问题"), stringPtr("回答")),
	})
	f.runner.err = errEvalBoom
	f.stream.addErr = errEvalBoom

	f.consumer.handleMessage(context.Background(), "1-1", voiceEvalMessage(5, 0))

	last := f.store.evalUpdates[len(f.store.evalUpdates)-1]
	if last.status != VoiceEvaluateFailed {
		t.Errorf("重投入队失败应置 FAILED, got %s", last.status)
	}
	if last.err == nil || *last.err != "重试入队失败: boom" {
		t.Errorf("evaluate_error = %v, want %q", last.err, "重试入队失败: boom")
	}
}

// ===== 消息字段解析（Java parsePayload / parseRetryCount）=====

func TestParseVoiceEvalPayload(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		wantOK bool
		wantID int64
	}{
		{name: "常规消息", values: map[string]any{VoiceEvalFieldSessionID: "5"}, wantOK: true, wantID: 5},
		{name: "字节值消息", values: map[string]any{VoiceEvalFieldSessionID: []byte("12")}, wantOK: true, wantID: 12},
		{name: "缺字段视为格式错误", values: map[string]any{VoiceEvalFieldRetryCount: "0"}, wantOK: false},
		{name: "非数字视为格式错误", values: map[string]any{VoiceEvalFieldSessionID: "abc"}, wantOK: false},
		{name: "空串视为格式错误", values: map[string]any{VoiceEvalFieldSessionID: ""}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, ok := parseVoiceEvalPayload(tt.values)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && payload.SessionID != tt.wantID {
				t.Errorf("sessionId = %d, want %d", payload.SessionID, tt.wantID)
			}
		})
	}
}

func TestParseVoiceEvalRetryCount(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		want   int
	}{
		{name: "缺字段默认 0", values: map[string]any{}, want: 0},
		{name: "合法数字", values: map[string]any{VoiceEvalFieldRetryCount: "2"}, want: 2},
		{name: "非法数字归 0", values: map[string]any{VoiceEvalFieldRetryCount: "x"}, want: 0},
		{name: "负数归 0", values: map[string]any{VoiceEvalFieldRetryCount: "-1"}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseVoiceEvalRetryCount(tt.values); got != tt.want {
				t.Errorf("retryCount = %d, want %d", got, tt.want)
			}
		})
	}
}

// ===== 复用断言小工具 =====

func containsEvent(events []string, event string) bool {
	for _, e := range events {
		if e == event {
			return true
		}
	}
	return false
}

func assertEventOrder(t *testing.T, events []string, ordered ...string) {
	t.Helper()
	pos := -1
	for _, want := range ordered {
		found := -1
		for i, e := range events {
			if e == want && i > pos {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("事件顺序不符: 期望 %q 出现在位置 %d 之后, 实际事件 %v", want, pos, events)
		}
		pos = found
	}
}
