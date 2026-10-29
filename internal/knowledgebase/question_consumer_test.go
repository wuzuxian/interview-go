// 出题 Stream 消费者与恢复调度测试（票 04）：fakeStream/fakeStore 服务缝
// （先例 internal/voiceinterview/evaluation_recovery_test.go），锁条件领取、
// 重试重投、重试耗尽转失败与 QUEUED/PROCESSING 卡住补投决策。
package knowledgebase

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// genStreamFixture 消费/恢复链路夹具。
type genStreamFixture struct {
	rec      *actionRecorder
	store    *fakeStore
	stream   *fakeStream
	gen      *fakeGenExecutor
	consumer *QuestionGenConsumer
}

// fakeGenExecutor ExecuteGeneration 接缝替身（记录调用、可脚本化失败；
// 成功路径镜像真实执行器的落库语义——replace 小事务写 COMPLETED）。
type fakeGenExecutor struct {
	store      *fakeStore
	calls      []genExecCall
	lastConfig QuestionGenerationConfig
	err        error
	failKinds  map[int64]error
}

type genExecCall struct {
	kbID   int64
	taskID string
}

func (f *fakeGenExecutor) ExecuteGeneration(ctx context.Context, kbID int64, taskID string, cfg QuestionGenerationConfig) error {
	f.calls = append(f.calls, genExecCall{kbID: kbID, taskID: taskID})
	f.lastConfig = cfg
	if f.failKinds != nil {
		if err, ok := f.failKinds[kbID]; ok {
			return err
		}
	}
	if f.err != nil {
		return f.err
	}
	_, _ = f.store.ReplaceQuestionsAndComplete(ctx, kbID, taskID, nil, 0, time.Now())
	return nil
}

func newGenStreamFixture(t *testing.T) *genStreamFixture {
	t.Helper()
	rec := &actionRecorder{}
	store := newFakeStore(rec)
	stream := newFakeStream(rec)
	gen := &fakeGenExecutor{store: store}
	logger := discardLogger()
	consumer := NewQuestionGenConsumer(stream, store, gen, NewQuestionGenProducer(stream, store, logger), logger)
	return &genStreamFixture{rec: rec, store: store, stream: stream, gen: gen, consumer: consumer}
}

// addQueuedTask 落一个 QUEUED 出题任务（含配置快照）。
func (f *genStreamFixture) addQueuedTask(kbID int64, taskID string) {
	kb := sampleKB(kbID)
	kb.VectorStatus = VectorCompleted
	kb.QuestionGenStatus = QuestionGenQueued
	kb.QuestionGenTaskID = stringPtr(taskID)
	kb.QuestionGenConfig = stringPtr(`{"difficulty":"mid","questionCount":3,"followUpCount":2,"categoryLimit":3,"llmProvider":null}`)
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	f.store.kbs[kbID] = kb
}

func questionMsg(kbID int64, taskID string, retry int) map[string]any {
	return map[string]any{
		FieldKBID:           strconv.FormatInt(kbID, 10),
		QuestionFieldTaskID: taskID,
		FieldRetryCount:     strconv.Itoa(retry),
	}
}

// Test出题消费者领取与完成 QUEUED → PROCESSING 条件领取 → 执行 → COMPLETED → ACK。
func Test出题消费者领取与完成(t *testing.T) {
	f := newGenStreamFixture(t)
	f.addQueuedTask(1, "task-1")
	f.stream.reads = [][]VectorizeMessage{{{ID: "m1", Values: questionMsg(1, "task-1", 0)}}}

	f.consumer.HandleMessage(context.Background(), "m1", questionMsg(1, "task-1", 0))

	kb := f.store.snapshot(1)
	if kb.QuestionGenStatus != QuestionGenCompleted {
		t.Fatalf("状态 = %v, want COMPLETED", kb.QuestionGenStatus)
	}
	if len(f.gen.calls) != 1 || f.gen.calls[0].taskID != "task-1" {
		t.Fatalf("执行记录 = %v", f.gen.calls)
	}
	if cfg := f.gen.lastConfig; cfg.Difficulty != "mid" || cfg.QuestionCount != 3 ||
		cfg.FollowUpCount != 2 || cfg.CategoryLimit != 3 || cfg.LLMProvider != nil {
		t.Fatalf("消费侧应从配置快照读取参数: %+v", cfg)
	}
	if !containsEvent(f.rec.list(), "ack") {
		t.Fatalf("应 ACK")
	}
}

// Test出题消费者领取失败幂等跳过 状态非 QUEUED（已被处理/替换）→ 不执行直接 ACK。
func Test出题消费者领取失败幂等跳过(t *testing.T) {
	f := newGenStreamFixture(t)
	f.addQueuedTask(1, "task-1")
	// 状态已被推进为 PROCESSING（其他实例已领取）。
	f.store.mu.Lock()
	f.store.kbs[1].QuestionGenStatus = QuestionGenProcessing
	f.store.mu.Unlock()

	f.consumer.HandleMessage(context.Background(), "m1", questionMsg(1, "task-1", 0))

	if len(f.gen.calls) != 0 {
		t.Fatalf("不应执行生成")
	}
	if !containsEvent(f.rec.list(), "ack") {
		t.Fatalf("应 ACK 丢弃")
	}
}

// Test出题消费者失败重试重投 执行失败 retryCount<3 → resetForRetry（回 QUEUED）
// 后以 retryCount+1 重投。
func Test出题消费者失败重试重投(t *testing.T) {
	f := newGenStreamFixture(t)
	f.addQueuedTask(1, "task-1")
	f.gen.err = errors.New("LLM 故障")

	f.consumer.HandleMessage(context.Background(), "m1", questionMsg(1, "task-1", 1))

	kb := f.store.snapshot(1)
	if kb.QuestionGenStatus != QuestionGenQueued {
		t.Fatalf("重试前应回 QUEUED: %v", kb.QuestionGenStatus)
	}
	if f.stream.addCount() != 1 {
		t.Fatalf("应重投一次")
	}
	msg := f.stream.addAt(0)
	if msg[QuestionFieldTaskID] != "task-1" || msg[FieldRetryCount] != "2" {
		t.Fatalf("重投消息 = %v", msg)
	}
}

// Test出题消费者重试耗尽转失败 retryCount=3 失败 → 不再重投，FAILED + 安全文案。
func Test出题消费者重试耗尽转失败(t *testing.T) {
	f := newGenStreamFixture(t)
	f.addQueuedTask(1, "task-1")
	f.gen.err = errors.New("LLM 故障")

	f.consumer.HandleMessage(context.Background(), "m1", questionMsg(1, "task-1", 3))

	kb := f.store.snapshot(1)
	if kb.QuestionGenStatus != QuestionGenFailed {
		t.Fatalf("状态 = %v, want FAILED", kb.QuestionGenStatus)
	}
	if deref(kb.QuestionGenError) != safeFailureMessage {
		t.Fatalf("error = %q, want %q", deref(kb.QuestionGenError), safeFailureMessage)
	}
	if f.stream.addCount() != 0 {
		t.Fatalf("重试耗尽不应重投")
	}
}

// Test出题消费者重置失败丢弃 resetForRetry 失败（任务已被新任务替换）→ ACK 丢弃不重投。
func Test出题消费者重置失败丢弃(t *testing.T) {
	f := newGenStreamFixture(t)
	f.addQueuedTask(1, "task-1")
	f.gen.err = errors.New("LLM 故障")
	// 领取后、失败前被新任务替换：模拟 store 的条件重置失败（状态已非 PROCESSING）。
	f.store.failResetForRetry = true

	f.consumer.HandleMessage(context.Background(), "m1", questionMsg(1, "task-1", 1))

	if f.stream.addCount() != 0 {
		t.Fatalf("重置失败不应重投")
	}
	if !containsEvent(f.rec.list(), "ack") {
		t.Fatalf("应 ACK 丢弃")
	}
}

// Test出题消息格式错误丢弃 缺 taskId 的消息 ACK 丢弃（Java parsePayload null 分支）。
func Test出题消息格式错误丢弃(t *testing.T) {
	f := newGenStreamFixture(t)
	f.addQueuedTask(1, "task-1")

	f.consumer.HandleMessage(context.Background(), "m1", map[string]any{FieldKBID: "1"})

	if len(f.gen.calls) != 0 {
		t.Fatalf("不应执行")
	}
	if !containsEvent(f.rec.list(), "ack") {
		t.Fatalf("应 ACK 丢弃")
	}
}

// Test出题任务投递失败转FAILED 生产者 XADD 失败 → markFailed（FAILED + 安全文案）。
func Test出题任务投递失败转FAILED(t *testing.T) {
	f := newGenStreamFixture(t)
	f.addQueuedTask(1, "task-1")
	f.stream.addErr = errAddTask

	producer := NewQuestionGenProducer(f.stream, f.store, discardLogger())
	if sent := producer.SendGenerateTask(context.Background(), 1, "task-1", 0); sent {
		t.Fatalf("应返回 false")
	}
	kb := f.store.snapshot(1)
	if kb.QuestionGenStatus != QuestionGenFailed || deref(kb.QuestionGenError) != safeFailureMessage {
		t.Fatalf("投递失败应转 FAILED: %+v", kb)
	}
}

// Test出题卡住任务恢复调度 QUEUED 超 2 分钟补投；PROCESSING 超 20 分钟先重置再补投。
func Test出题卡住任务恢复调度(t *testing.T) {
	f := newGenStreamFixture(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

	// QUEUED 卡住。
	queued := sampleKB(1)
	queued.VectorStatus = VectorCompleted
	queued.QuestionGenStatus = QuestionGenQueued
	queued.QuestionGenTaskID = stringPtr("task-q")
	stale := now.Add(-3 * time.Minute)
	queued.QuestionGenUpdatedAt = &stale
	f.store.kbs[1] = queued

	// PROCESSING 卡住（超 20 分钟）。
	processing := sampleKB(2)
	processing.VectorStatus = VectorCompleted
	processing.QuestionGenStatus = QuestionGenProcessing
	processing.QuestionGenTaskID = stringPtr("task-p")
	processingStale := now.Add(-25 * time.Minute)
	processing.QuestionGenUpdatedAt = &processingStale
	f.store.kbs[2] = processing

	// 刚更新过的任务不应被补投。
	fresh := sampleKB(3)
	fresh.VectorStatus = VectorCompleted
	fresh.QuestionGenStatus = QuestionGenQueued
	fresh.QuestionGenTaskID = stringPtr("task-f")
	freshAt := now.Add(-30 * time.Second)
	fresh.QuestionGenUpdatedAt = &freshAt
	f.store.kbs[3] = fresh

	scheduler := NewQuestionGenRecoveryScheduler(f.store, NewQuestionGenProducer(f.stream, f.store, discardLogger()), discardLogger())
	scheduler.RecoverStaleTasks(context.Background(), now)

	if f.stream.addCount() != 2 {
		t.Fatalf("补投次数 = %d, want 2", f.stream.addCount())
	}
	if got := f.store.snapshot(1).QuestionGenStatus; got != QuestionGenQueued {
		t.Errorf("QUEUED 恢复后状态应保持 QUEUED: %v", got)
	}
	if got := f.store.snapshot(2).QuestionGenStatus; got != QuestionGenQueued {
		t.Errorf("PROCESSING 恢复应先重置回 QUEUED: %v", got)
	}
	for _, msg := range f.stream.adds {
		taskID, _ := msg[QuestionFieldTaskID].(string)
		if taskID != "task-q" && taskID != "task-p" {
			t.Errorf("不应补投新任务 %v", taskID)
		}
	}
}
