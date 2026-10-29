// 向量化消费者编排单测（fakeStream/fakeStore/fakeParser/fakeVectorizer 缝）：
// 领取、跳过、心跳 fencing、重试与终态。契约来源：Java VectorizeStreamConsumer +
// AbstractStreamConsumer.processMessage 逐字。
package knowledgebase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// consumerFixture 消费链路夹具（消费一次入队消息）。
type consumerFixture struct {
	*kbFixture
	consumer *VectorizeConsumer
}

func newConsumerFixture() *consumerFixture {
	f := &consumerFixture{kbFixture: newKBFixture()}
	f.consumer = NewVectorizeConsumer(f.stream, f.store, f.parser, f.vec, 30*time.Second, discardLogger())
	return f
}

// feed 入队一条新消息并消费一轮。
func (f *consumerFixture) feed(t *testing.T, values map[string]any) {
	t.Helper()
	f.stream.reads = [][]VectorizeMessage{{{ID: "1-1", Values: values}}}
	f.consumer.consumeOnce(context.Background())
}

func vectorizeMessage(kbID int64, retryCount int) map[string]any {
	return map[string]any{
		FieldKBID:       itoa(kbID),
		FieldRetryCount: itoa(int64(retryCount)),
	}
}

// TestConsume正常完成领取心跳终态与ACK 顺序契约完整链路。
func TestConsume正常完成领取心跳终态与ACK(t *testing.T) {
	f := newConsumerFixture()
	kb := f.addProcessingKB(1)
	kb.VectorStatus = VectorPending // 待领取
	f.parser.content = "解析出的文本内容"

	f.feed(t, vectorizeMessage(1, 0))

	if f.vec.calls != 1 {
		t.Fatalf("应执行 1 次向量化, got %d", f.vec.calls)
	}
	if f.vec.lastKbID != 1 || f.vec.lastContent != "解析出的文本内容" {
		t.Errorf("向量化入参 kbId=%d content=%q", f.vec.lastKbID, f.vec.lastContent)
	}
	if f.vec.lastSource.Name != "面试笔记" || f.vec.lastSource.Filename != "面试笔记.md" || f.vec.lastSource.FileHash == "" {
		t.Errorf("SourceInfo = %+v", f.vec.lastSource)
	}
	// 解析后心跳（执行权在写库时刻仍然匹配）。
	if !containsEvent(f.rec.list(), "heartbeat") {
		t.Error("下载解析后应心跳一次")
	}
	// 终态条件写入使用的执行代次与领取/向量化一致（fencing 链路闭环）。
	if f.store.lastCompleteAttempt != f.vec.lastAttempt {
		t.Errorf("complete attemptId = %q, want %q", f.store.lastCompleteAttempt, f.vec.lastAttempt)
	}
	// 终态条件写入 + ACK。
	events := f.rec.list()
	assertEventOrder(t, events, "complete-if-processing", "ack")
	got := f.store.snapshot(1)
	if got.VectorStatus != VectorCompleted {
		t.Errorf("终态 = %v, want COMPLETED", got.VectorStatus)
	}
	if len(f.stream.acks) != 1 {
		t.Errorf("应 ACK 一次, got %v", f.stream.acks)
	}
}

// TestConsumeCOMPLETED跳过 幂等守卫。
func TestConsumeCOMPLETED跳过(t *testing.T) {
	f := newConsumerFixture()
	kb := f.addProcessingKB(1)
	kb.VectorStatus = VectorCompleted
	f.feed(t, vectorizeMessage(1, 0))
	if f.vec.calls != 0 {
		t.Errorf("COMPLETED 应跳过, got %d 次向量化", f.vec.calls)
	}
	if len(f.stream.acks) != 1 {
		t.Error("跳过应 ACK")
	}
}

// TestConsume实体不存在跳过 Java Optional.empty → orElse(true)。
func TestConsume实体不存在跳过(t *testing.T) {
	f := newConsumerFixture()
	f.feed(t, vectorizeMessage(99, 0))
	if f.vec.calls != 0 || len(f.stream.acks) != 1 {
		t.Errorf("不存在应跳过并 ACK, calls=%d acks=%v", f.vec.calls, f.stream.acks)
	}
}

// TestConsume领取失败ACK丢弃 非本消费者任务。
func TestConsume领取失败ACK丢弃(t *testing.T) {
	f := newConsumerFixture()
	// 保持 PROCESSING（非 PENDING）→ 领取失败。
	f.addProcessingKB(1)
	f.feed(t, vectorizeMessage(1, 0))
	if f.vec.calls != 0 {
		t.Error("领取失败不应执行向量化")
	}
	if len(f.stream.acks) != 1 {
		t.Error("领取失败应 ACK 丢弃")
	}
	if containsEvent(f.rec.list(), "complete-if-processing") {
		t.Error("领取失败不应写终态")
	}
}

// TestConsume解析空文本重试 空文本 → 重置 PENDING + 重新入队 retryCount+1。
func TestConsume解析空文本重试(t *testing.T) {
	f := newConsumerFixture()
	kb := f.addProcessingKB(1)
	kb.VectorStatus = VectorPending
	f.parser.content = "   "

	f.feed(t, vectorizeMessage(1, 0))

	events := f.rec.list()
	// 顺序契约：先条件重置（PROCESSING→PENDING）再重新入队，最后 ACK。
	assertEventOrder(t, events, "reset-to-pending", "xadd")
	assertEventOrder(t, events, "xadd", "ack")
	if f.stream.addCount() != 1 {
		t.Fatalf("应重新入队 1 条, got %d", f.stream.addCount())
	}
	added := f.stream.addAt(0)
	if added[FieldKBID] != "1" || added[FieldRetryCount] != "1" {
		t.Errorf("重投消息 = %v, want retryCount=1", added)
	}
	if got := f.store.snapshot(1); got.VectorStatus != VectorPending {
		t.Errorf("重试后状态 = %v, want PENDING", got.VectorStatus)
	}
}

// TestConsume重试上限置FAILED retryCount=3 耗尽 → failVectorIfProcessing。
func TestConsume重试上限置FAILED(t *testing.T) {
	f := newConsumerFixture()
	kb := f.addProcessingKB(1)
	kb.VectorStatus = VectorPending
	f.parser.err = errors.New("文件解析失败")

	f.feed(t, vectorizeMessage(1, 3))

	if f.stream.addCount() != 0 {
		t.Errorf("重试耗尽不应重新入队, got %d", f.stream.addCount())
	}
	got := f.store.snapshot(1)
	if got.VectorStatus != VectorFailed {
		t.Fatalf("终态 = %v, want FAILED", got.VectorStatus)
	}
	if got.VectorError == nil || *got.VectorError != "向量化 failed after retry 3" {
		t.Errorf("vectorError = %v, want %q", got.VectorError, "向量化 failed after retry 3")
	}
	if len(f.stream.acks) != 1 {
		t.Error("置败后应 ACK")
	}
}

// TestConsume执行权失效不覆盖终态 心跳 fencing：解析期间代次被剥夺 →
// 重置失败 ACK 丢弃（新任务的结果不被旧代次覆盖）。
func TestConsume执行权失效不覆盖终态(t *testing.T) {
	f := newConsumerFixture()
	kb := f.addProcessingKB(1)
	kb.VectorStatus = VectorPending
	// 消费中途改写代次：解析时把实体 attemptId 换掉（模拟恢复重置+新领取）。
	f.parser.hook = func() {
		f.store.mu.Lock()
		f.store.kbs[1].VectorAttemptID = stringPtr("attempt-new")
		f.store.mu.Unlock()
	}
	f.feed(t, vectorizeMessage(1, 0))

	events := f.rec.list()
	if containsEvent(events, "complete-if-processing") {
		t.Errorf("执行权失效不应写终态, events = %v", events)
	}
	// 重置 PROCESSING→PENDING 也失败（attempt 不再匹配）→ ACK 丢弃。
	if containsEvent(events, "xadd") {
		t.Errorf("执行权失效不应重新入队, events = %v", events)
	}
	if len(f.stream.acks) != 1 {
		t.Errorf("应 ACK 丢弃, acks = %v", f.stream.acks)
	}
	got := f.store.snapshot(1)
	if got.VectorStatus == VectorCompleted {
		t.Error("终态不应被旧代次覆盖")
	}
}

// TestConsume解析后心跳失败立即终止 心跳返回 0 → 错误上抛走重试路径。
func TestConsume解析后心跳失败立即终止(t *testing.T) {
	f := newConsumerFixture()
	kb := f.addProcessingKB(1)
	kb.VectorStatus = VectorPending
	// 解析时移除实体，模拟领取后执行权被剥夺 → 心跳 0。
	f.parser.hook = func() {
		f.store.mu.Lock()
		delete(f.store.kbs, 1)
		f.store.mu.Unlock()
	}
	f.feed(t, vectorizeMessage(1, 0))

	if f.vec.calls != 0 {
		t.Error("心跳失败不应执行向量化")
	}
	if containsEvent(f.rec.list(), "complete-if-processing") {
		t.Error("心跳失败不应写终态")
	}
}

// TestConsume节流心跳30s Embedding 阶段心跳按阈值节流。
func TestConsume节流心跳30s(t *testing.T) {
	f := newConsumerFixture()
	kb := f.addProcessingKB(1)
	kb.VectorStatus = VectorPending
	f.parser.content = "内容"
	_ = kb

	// fakeVectorizer 每次 VectorizeAndStore 触发一次回调；先正常消费一轮。
	f.feed(t, vectorizeMessage(1, 0))
	if f.vec.calls != 1 {
		t.Fatalf("应执行向量化, got %d", f.vec.calls)
	}
	attempt := f.vec.lastAttempt
	base := countEvents(f.rec.list(), "heartbeat")
	if base == 0 {
		t.Fatal("解析后应至少一次心跳")
	}
	// 立即再次节流回调：距上次 < 30s，不写库。
	if err := f.consumer.throttledEmbeddingHeartbeat(context.Background(), 1, attempt); err != nil {
		t.Fatalf("节流回调不应失败: %v", err)
	}
	if got := countEvents(f.rec.list(), "heartbeat"); got != base {
		t.Errorf("节流窗口内不应写心跳, %d → %d", base, got)
	}
}

// TestConsume格式错误消息ACK丢弃 kbId 缺失或非数字。
func TestConsume格式错误消息ACK丢弃(t *testing.T) {
	f := newConsumerFixture()
	f.feed(t, map[string]any{"other": "1"})
	if len(f.stream.acks) != 1 {
		t.Error("格式错误应 ACK 丢弃")
	}
	if f.vec.calls != 0 {
		t.Error("格式错误不应执行向量化")
	}
	f2 := newConsumerFixture()
	f2.feed(t, map[string]any{FieldKBID: "not-a-number"})
	if len(f2.stream.acks) != 1 {
		t.Error("非数字 kbId 应 ACK 丢弃")
	}
}

// TestConsume领取后实体被删除视为成功 processBusiness 二次复查
// （Java findById orElse(null) → 告警返回，视为成功不写终态）。
func TestConsume领取后实体被删除视为成功(t *testing.T) {
	f := newConsumerFixture()
	kb := f.addProcessingKB(1)
	kb.VectorStatus = VectorPending
	f.parser.content = "内容"
	// shouldSkip 与领取均通过；processBusiness 复查时实体已被删除。
	f.parser.hook = func() {}
	// hook 在 parser 之前无法删除（领取后才调用 parser）——此处实体删除发生在
	// processBusiness 复查前：用 store 脚本化 FindByID 第二次起返回不存在。
	f.store.mu.Lock()
	f.store.deleteAfterFind = true
	f.store.mu.Unlock()
	f.feed(t, vectorizeMessage(1, 0))

	if f.vec.calls != 0 {
		t.Errorf("实体已删除不应向量化, got %d", f.vec.calls)
	}
	if containsEvent(f.rec.list(), "complete-if-processing") && containsEvent(f.rec.list(), "fail-if-processing") {
		t.Error("已删除实体不应写终态")
	}
	if len(f.stream.acks) != 1 {
		t.Error("应 ACK")
	}
}

// countEvents 统计含前缀的事件数。
func countEvents(events []string, prefix string) int {
	n := 0
	for _, e := range events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}
