// 评估队列链路单测（票 06）：入队语义（PENDING 置位 + 消息字段）、重试次数递增与上限、
// shouldSkip 状态守卫、失败落 evaluate_error。契约来源：笔记 06 逐字 + 裁决 #7。
package interview

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 契约：EnqueueEvaluation 先置 evaluate_status=PENDING（并清空 evaluate_error），
// 再 XADD 消息 {sessionId, retryCount="0"}——sessionId 为业务 session_id（Java 消息体
// 逐字），不是内部行 ID；顺序"先 PENDING 后入队"由共享事件日志断言。
func TestEnqueueEvaluationPendingThenXAdd(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	stream := newFakeEvalStream(rec)
	producer := NewEvaluateStreamProducer(stream, nil, store, discardLogger())

	session := evalTestSession(42, "biz-session-1", StatusCompleted, nil)
	session.EvaluateError = stringPtr("旧错误")
	store.addSession(session)

	if err := producer.EnqueueEvaluation(context.Background(), 42); err != nil {
		t.Fatalf("EnqueueEvaluation 失败: %v", err)
	}

	events := rec.list()
	if len(events) != 2 || events[0] != "evaluate:PENDING" || events[1] != "xadd" {
		t.Fatalf("顺序应为 先置 PENDING 再 XADD: %v", events)
	}
	if len(store.evalUpdates) != 1 || store.evalUpdates[0].Status != EvaluatePending {
		t.Fatalf("应恰有一次 PENDING 状态写入: %+v", store.evalUpdates)
	}
	if store.evalUpdates[0].Error != nil {
		t.Fatalf("置 PENDING 应清空 evaluate_error: %v", *store.evalUpdates[0].Error)
	}
	if stream.addCount() != 1 {
		t.Fatalf("应恰投递一条消息: %d", stream.addCount())
	}
	msg := stream.addAt(0)
	if msg[EvalFieldSessionID] != "biz-session-1" {
		t.Fatalf("消息 sessionId 应为业务 ID: %v", msg[EvalFieldSessionID])
	}
	if msg[EvalFieldRetryCount] != "0" {
		t.Fatalf("首条消息 retryCount 应为 0: %v", msg[EvalFieldRetryCount])
	}
}

// 契约：XADD 失败对齐 Java AbstractStreamProducer.onSendFailed——置 FAILED +
// evaluate_error="任务入队失败"，且不向调用方报错（提交/交卷请求不因入队失败而 500，
// 前端经 evaluateStatus 轮询可见 FAILED）。
func TestEnqueueEvaluationXAddFailureMarksFailed(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	stream := newFakeEvalStream(rec)
	stream.addErr = errors.New("redis down")
	producer := NewEvaluateStreamProducer(stream, nil, store, discardLogger())
	store.addSession(evalTestSession(42, "biz-session-1", StatusCompleted, nil))

	if err := producer.EnqueueEvaluation(context.Background(), 42); err != nil {
		t.Fatalf("XADD 失败不应向调用方报错（Java sendTask 吞掉并置 FAILED）: %v", err)
	}
	if len(store.evalUpdates) != 2 {
		t.Fatalf("应有 PENDING + FAILED 两次状态写入: %+v", store.evalUpdates)
	}
	last := store.evalUpdates[len(store.evalUpdates)-1]
	if last.Status != EvaluateFailed {
		t.Fatalf("末次状态应为 FAILED: %s", last.Status)
	}
	if last.Error == nil || *last.Error != "任务入队失败" {
		t.Fatalf("evaluate_error 应为 任务入队失败: %+v", last.Error)
	}
}

// 契约：置 PENDING 失败向上传播（Java updateEvaluateStatus 抛出 → 500 语义）。
func TestEnqueueEvaluationPendingUpdateFailurePropagates(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	stream := newFakeEvalStream(rec)
	producer := NewEvaluateStreamProducer(stream, nil, store, discardLogger())
	store.addSession(evalTestSession(42, "biz-session-1", StatusCompleted, nil))
	store.failEvalUpd = errors.New("db down")

	if err := producer.EnqueueEvaluation(context.Background(), 42); err == nil {
		t.Fatal("置 PENDING 失败应向上传播")
	}
	if stream.addCount() != 0 {
		t.Fatal("置 PENDING 失败后不应投递消息")
	}
}

// 契约：场次不存在（Java ifPresent 语义）静默跳过，不投递消息。
func TestEnqueueEvaluationMissingSessionSkips(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	stream := newFakeEvalStream(rec)
	producer := NewEvaluateStreamProducer(stream, nil, store, discardLogger())

	if err := producer.EnqueueEvaluation(context.Background(), 404); err != nil {
		t.Fatalf("场次不存在应静默跳过: %v", err)
	}
	if stream.addCount() != 0 || len(store.evalUpdates) != 0 {
		t.Fatalf("不应有任何副作用: adds=%d updates=%d", stream.addCount(), len(store.evalUpdates))
	}
}

// 契约（笔记 06 shouldSkip 逐字）：会话不存在或 evaluate_status 已 COMPLETED 跳过；
// PENDING/PROCESSING/FAILED/未设置都不跳过——重试消息依赖该守卫去重（裁决 #7）。
func TestConsumerShouldSkipGuard(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	consumer := NewEvaluateConsumer(newFakeEvalStream(rec), nil, store, &stubEvalRunner{}, nil, discardLogger())

	completed := EvaluateCompleted
	pending := EvaluatePending
	processing := EvaluateProcess
	failed := EvaluateFailed

	cases := []struct {
		name     string
		session  *SessionEntity
		wantSkip bool
	}{
		{"会话不存在跳过", nil, true},
		{"COMPLETED 跳过", evalTestSession(1, "s1", StatusEvaluated, &completed), true},
		{"PENDING 不跳过", evalTestSession(1, "s1", StatusCompleted, &pending), false},
		{"PROCESSING 不跳过（重试消息依赖）", evalTestSession(1, "s1", StatusCompleted, &processing), false},
		{"FAILED 不跳过", evalTestSession(1, "s1", StatusCompleted, &failed), false},
		{"evaluate_status 为空不跳过", evalTestSession(1, "s1", StatusCompleted, nil), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store.sessions = map[string]*SessionEntity{}
			if tc.session != nil {
				store.addSession(tc.session)
			}
			if got := consumer.shouldSkip(context.Background(), evalPayload{SessionID: "s1"}); got != tc.wantSkip {
				t.Fatalf("shouldSkip = %v, want %v", got, tc.wantSkip)
			}
		})
	}
}

// 契约（裁决 #7）：消费失败重新 XADD（retryCount+1）并 ACK 原消息；
// retryCount=0/1/2 → 重投 1/2/3；retryCount=3（≥上限）不再重投，置 FAILED +
// evaluate_error="评估 failed after retry 3"（≤500 字符）。
func TestConsumerRetryIncrementAndCap(t *testing.T) {
	cases := []struct {
		name          string
		retryCount    string
		wantRequeue   bool
		wantNextRetry string
		wantFailed    bool
	}{
		{"首次失败重投 retryCount=1", "0", true, "1", false},
		{"第二次失败重投 retryCount=2", "1", true, "2", false},
		{"第三次失败重投 retryCount=3", "2", true, "3", false},
		{"retryCount=3 耗尽置 FAILED", "3", false, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &evalTestRecorder{}
			store := newFakeEvalStore(rec)
			stream := newFakeEvalStream(rec)
			runner := &stubEvalRunner{err: errors.New("llm down")}
			consumer := NewEvaluateConsumer(stream, nil, store, runner, nil, discardLogger())
			store.addSession(evalTestSession(1, "s1", StatusCompleted, nil))

			consumer.handleMessage(context.Background(), "9-1", map[string]any{
				EvalFieldSessionID:  "s1",
				EvalFieldRetryCount: tc.retryCount,
			})

			if tc.wantRequeue {
				if stream.addCount() != 1 {
					t.Fatalf("应重新 XADD 一条消息: %d", stream.addCount())
				}
				msg := stream.addAt(0)
				if msg[EvalFieldSessionID] != "s1" || msg[EvalFieldRetryCount] != tc.wantNextRetry {
					t.Fatalf("重投消息不符: %v", msg)
				}
				last := store.evalUpdates[len(store.evalUpdates)-1]
				if last.Status == EvaluateFailed {
					t.Fatalf("重投路径不应置 FAILED: %+v", last)
				}
			} else {
				if stream.addCount() != 0 {
					t.Fatalf("重试耗尽不应重投: %v", stream.addAt(0))
				}
				last := store.evalUpdates[len(store.evalUpdates)-1]
				if last.Status != EvaluateFailed {
					t.Fatalf("应置 FAILED: %+v", last)
				}
				if last.Error == nil || *last.Error != "评估 failed after retry 3" {
					t.Fatalf("evaluate_error 不符: %+v", last.Error)
				}
				if len(*last.Error) > 500 {
					t.Fatal("evaluate_error 应 ≤500 字符")
				}
			}
			// 原消息始终 ACK（裁决 #7：重新 XADD 后 ACK 原消息）。
			if len(stream.acked) != 1 || stream.acked[0] != "9-1" {
				t.Fatalf("原消息应被 ACK: %v", stream.acked)
			}
		})
	}
}

// 契约：重投 XADD 失败置 FAILED + evaluate_error="重试入队失败: ..."（Java retryMessage 逐字）。
func TestConsumerRetryEnqueueFailureMarksFailed(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	stream := newFakeEvalStream(rec)
	stream.addErr = errors.New("redis down")
	consumer := NewEvaluateConsumer(stream, nil, store, &stubEvalRunner{err: errors.New("llm down")}, nil, discardLogger())
	store.addSession(evalTestSession(1, "s1", StatusCompleted, nil))

	consumer.handleMessage(context.Background(), "9-1", map[string]any{
		EvalFieldSessionID:  "s1",
		EvalFieldRetryCount: "0",
	})

	last := store.evalUpdates[len(store.evalUpdates)-1]
	if last.Status != EvaluateFailed {
		t.Fatalf("重投失败应置 FAILED: %+v", last)
	}
	if last.Error == nil || !strings.HasPrefix(*last.Error, "重试入队失败: ") {
		t.Fatalf("evaluate_error 应以 重试入队失败 开头: %+v", last.Error)
	}
}

// 契约：成功链路 = 从 DB 重放会话与作答（简历文本取自 resumes 关联，不用请求参数）→
// 评估 → saveReport 落库 → 置 COMPLETED、ACK；入队时间戳清理。
func TestConsumerSuccessReplayAndSaveReport(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	stream := newFakeEvalStream(rec)
	tracker := &fakeTracker{}
	runner := &stubEvalRunner{report: evalSampleReport()}
	refs := &fakeRefSource{text: "参考段"}
	consumer := NewEvaluateConsumer(stream, tracker, store, runner, refs, discardLogger())

	session := evalTestSession(1, "s1", StatusCompleted, nil)
	session.QuestionsJSON = stringPtr(evalSampleQuestionsJSON(t))
	store.addSession(session)
	store.setAnswers(1, []AnswerEntity{
		{SessionID: 1, QuestionIndex: intPtr(0), UserAnswer: stringPtr("A1"), Score: intPtr(0)},
	})
	store.resumes[7] = "简历正文"

	consumer.handleMessage(context.Background(), "9-1", map[string]any{
		EvalFieldSessionID:  "s1",
		EvalFieldRetryCount: "0",
	})

	if runner.callCount() != 1 {
		t.Fatalf("应恰评估一次: %d", runner.callCount())
	}
	input := runner.lastInput
	if input.SessionID != "s1" {
		t.Fatalf("sessionId 不符: %s", input.SessionID)
	}
	if input.ResumeText != "简历正文" {
		t.Fatalf("简历文本应取自 resumes 关联: %q", input.ResumeText)
	}
	if len(input.Questions) != 2 || input.Questions[0].UserAnswer == nil || *input.Questions[0].UserAnswer != "A1" {
		t.Fatalf("作答应从 interview_answers 重放: %+v", input.Questions)
	}
	if input.Questions[1].UserAnswer != nil {
		t.Fatalf("未答题 userAnswer 应为空: %+v", input.Questions[1])
	}
	if input.SkillReference != "参考段" {
		t.Fatalf("参考段不符: %q", input.SkillReference)
	}
	if refs.lastSkill != "java-backend" {
		t.Fatalf("参考段应按会话 skillId 构建: %s", refs.lastSkill)
	}
	if runner.lastProvide == nil || *runner.lastProvide != "test-provider" {
		t.Fatalf("provider 应取会话 llm_provider: %+v", runner.lastProvide)
	}

	// saveReport：会话列 + 逐题回填已写入，末状态 COMPLETED。
	if len(store.reportWrites) != 1 {
		t.Fatalf("应写一次会话报告列: %d", len(store.reportWrites))
	}
	if len(store.answerWrites) != 2 {
		t.Fatalf("应逐题回填 2 行: %d", len(store.answerWrites))
	}
	events := rec.list()
	if last := store.evalUpdates[len(store.evalUpdates)-1]; last.Status != EvaluateCompleted {
		t.Fatalf("末次评估状态应为 COMPLETED: %+v", store.evalUpdates)
	}
	if !eventBefore(events, "report-columns", "evaluate:COMPLETED") || !eventBefore(events, "evaluate:COMPLETED", "ack") {
		t.Fatalf("事件顺序应为 评估→落库→COMPLETED→ACK: %v", events)
	}
	if len(stream.acked) != 1 {
		t.Fatalf("消息应被 ACK: %v", stream.acked)
	}
	if len(tracker.forgot) != 1 || tracker.forgot[0] != "s1" {
		t.Fatalf("终态应清理在队时间戳: %v", tracker.forgot)
	}
}

// 契约：evaluate_status 已 COMPLETED 的消息跳过（不调引擎、不落库）但 ACK。
func TestConsumerSkipsCompletedMessage(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	stream := newFakeEvalStream(rec)
	runner := &stubEvalRunner{report: evalSampleReport()}
	consumer := NewEvaluateConsumer(stream, nil, store, runner, nil, discardLogger())
	completed := EvaluateCompleted
	store.addSession(evalTestSession(1, "s1", StatusEvaluated, &completed))

	consumer.handleMessage(context.Background(), "9-1", map[string]any{
		EvalFieldSessionID:  "s1",
		EvalFieldRetryCount: "1",
	})

	if runner.callCount() != 0 {
		t.Fatalf("已 COMPLETED 不应重调引擎: %d", runner.callCount())
	}
	if len(stream.acked) != 1 {
		t.Fatalf("跳过也应 ACK: %v", stream.acked)
	}
}

// 契约：会话已删除的消息（Java processBusiness warn-and-return）不评估、不重试、照常 ACK。
func TestConsumerMissingSessionAckAndNoRetry(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	stream := newFakeEvalStream(rec)
	runner := &stubEvalRunner{report: evalSampleReport()}
	consumer := NewEvaluateConsumer(stream, nil, store, runner, nil, discardLogger())

	consumer.handleMessage(context.Background(), "9-1", map[string]any{
		EvalFieldSessionID:  "ghost",
		EvalFieldRetryCount: "0",
	})

	if runner.callCount() != 0 {
		t.Fatal("已删除场次不应评估")
	}
	if stream.addCount() != 0 {
		t.Fatal("已删除场次不应重投")
	}
	if len(stream.acked) != 1 {
		t.Fatalf("应 ACK: %v", stream.acked)
	}
}

// 契约：消息字段解析（Java parsePayload / parseRetryCount）：sessionId 缺失视为格式错误；
// retryCount 缺失/非法 → 0。
func TestParseEvalPayloadAndRetryCount(t *testing.T) {
	if _, ok := parseEvalPayload(map[string]any{}); ok {
		t.Fatal("缺 sessionId 应解析失败")
	}
	if _, ok := parseEvalPayload(map[string]any{EvalFieldSessionID: ""}); ok {
		t.Fatal("空 sessionId 应解析失败")
	}
	payload, ok := parseEvalPayload(map[string]any{EvalFieldSessionID: "s1"})
	if !ok || payload.SessionID != "s1" {
		t.Fatalf("解析失败: %+v %v", payload, ok)
	}
	cases := []struct {
		values map[string]any
		want   int
	}{
		{map[string]any{EvalFieldSessionID: "s1"}, 0},
		{map[string]any{EvalFieldSessionID: "s1", EvalFieldRetryCount: "2"}, 2},
		{map[string]any{EvalFieldSessionID: "s1", EvalFieldRetryCount: "x"}, 0},
		{map[string]any{EvalFieldSessionID: "s1", EvalFieldRetryCount: "-1"}, 0},
	}
	for _, tc := range cases {
		if got := parseEvalRetryCount(tc.values); got != tc.want {
			t.Fatalf("parseEvalRetryCount(%v) = %d, want %d", tc.values, got, tc.want)
		}
	}
}

// 契约：评估错误列截断 500 字符（Java truncateError），按字符计不切碎多字节。
func TestTruncateEvaluateError(t *testing.T) {
	if got := truncateEvaluateError("abc"); got != "abc" {
		t.Fatalf("短文本不应截断: %q", got)
	}
	long := strings.Repeat("评", 600)
	got := truncateEvaluateError(long)
	if len([]rune(got)) != 500 {
		t.Fatalf("应截断到 500 字符: %d", len([]rune(got)))
	}
	if ptr := truncateEvaluateErrorPtr(nil); ptr != nil {
		t.Fatal("nil 应原样保留（清空列）")
	}
}

// fakeTracker 在队时间戳替身。
type fakeTracker struct {
	marked []string
	forgot []string
}

func (f *fakeTracker) MarkEnqueued(ctx context.Context, sessionID string, at time.Time) error {
	f.marked = append(f.marked, sessionID)
	return nil
}

func (f *fakeTracker) Forget(ctx context.Context, sessionID string) error {
	f.forgot = append(f.forgot, sessionID)
	return nil
}

func (f *fakeTracker) Stale(ctx context.Context, before time.Time, limit int) ([]EvalInflightEntry, error) {
	return nil, nil
}
