// 恢复调度器与解析服务单测（fake 缝 + 注入时钟）。
// 契约来源：Java VectorizeRecoveryScheduler / DocumentParseService 逐字。
package knowledgebase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/docparse"
)

// TestRecoveryPending超阈补投 PENDING 超 10 分钟：触碰时间 + 计数 +1 + 补投。
func TestRecoveryPending超阈补投(t *testing.T) {
	f := newKBFixture()
	producer := NewVectorizeProducer(f.stream, f.store, discardLogger())
	scheduler := NewRecoveryScheduler(f.store, producer, RecoveryConfig{
		Enabled: true, PendingThreshold: 10 * time.Minute, ProcessingThreshold: 15 * time.Minute,
		Interval: time.Minute, BatchSize: 10, MaxRecoveryCount: 3,
	}, discardLogger())

	kb := sampleKB(0)
	kb.VectorStatus = VectorPending
	kb.VectorUpdatedAt = timePtr(f.now.Add(-11 * time.Minute))
	staleID := f.store.addKB(kb)
	fresh := sampleKB(0)
	fresh.VectorStatus = VectorPending
	fresh.VectorUpdatedAt = timePtr(f.now.Add(-9 * time.Minute))
	f.store.addKB(fresh)

	scheduler.RecoverStuckTasks(context.Background(), f.now)

	if f.stream.addCount() != 1 {
		t.Fatalf("应补投 1 条, got %d", f.stream.addCount())
	}
	added := f.stream.addAt(0)
	if added[FieldKBID] != itoa(staleID) || added[FieldRetryCount] != "0" {
		t.Errorf("补投消息 = %v", added)
	}
	got := f.store.snapshot(staleID)
	if got.VectorRecoveryCount != 1 {
		t.Errorf("恢复计数 = %d, want 1", got.VectorRecoveryCount)
	}
	// 触碰：vector_updated_at 推进到注入时钟（同一周期不重复补投）。
	if got.VectorUpdatedAt == nil || !got.VectorUpdatedAt.Equal(f.now) {
		t.Errorf("触碰时间 = %v, want %v", got.VectorUpdatedAt, f.now)
	}
}

// TestRecoveryProcessing超阈先重置再补投 PROCESSING 超 15 分钟无心跳。
func TestRecoveryProcessing超阈先重置再补投(t *testing.T) {
	f := newKBFixture()
	producer := NewVectorizeProducer(f.stream, f.store, discardLogger())
	scheduler := NewRecoveryScheduler(f.store, producer, RecoveryConfig{
		Enabled: true, PendingThreshold: 10 * time.Minute, ProcessingThreshold: 15 * time.Minute,
		Interval: time.Minute, BatchSize: 10, MaxRecoveryCount: 3,
	}, discardLogger())

	kb := sampleKB(0)
	kb.VectorStatus = VectorProcessing
	kb.VectorAttemptID = stringPtr("attempt-old")
	kb.VectorUpdatedAt = timePtr(f.now.Add(-16 * time.Minute))
	id := f.store.addKB(kb)

	scheduler.RecoverStuckTasks(context.Background(), f.now)

	got := f.store.snapshot(id)
	if got.VectorStatus != VectorPending {
		t.Fatalf("状态 = %v, want PENDING（先条件重置）", got.VectorStatus)
	}
	if got.VectorAttemptID != nil {
		t.Errorf("代次应清空, got %v", *got.VectorAttemptID)
	}
	if f.stream.addCount() != 1 {
		t.Fatalf("重置后应补投, got %d", f.stream.addCount())
	}
	if got.VectorRecoveryCount != 1 {
		t.Errorf("恢复计数 = %d, want 1", got.VectorRecoveryCount)
	}
}

// TestRecovery达上限转FAILED recovery_count ≥ 3 → FAILED（含逐字文案）。
func TestRecovery达上限转FAILED(t *testing.T) {
	f := newKBFixture()
	producer := NewVectorizeProducer(f.stream, f.store, discardLogger())
	scheduler := NewRecoveryScheduler(f.store, producer, RecoveryConfig{
		Enabled: true, PendingThreshold: 10 * time.Minute, ProcessingThreshold: 15 * time.Minute,
		Interval: time.Minute, BatchSize: 10, MaxRecoveryCount: 3,
	}, discardLogger())

	kb := sampleKB(0)
	kb.VectorStatus = VectorPending
	kb.VectorUpdatedAt = timePtr(f.now.Add(-11 * time.Minute))
	kb.VectorRecoveryCount = 2
	id := f.store.addKB(kb)

	scheduler.RecoverStuckTasks(context.Background(), f.now)

	got := f.store.snapshot(id)
	if got.VectorStatus != VectorFailed {
		t.Fatalf("状态 = %v, want FAILED", got.VectorStatus)
	}
	if got.VectorError == nil || *got.VectorError != "自动恢复次数达到上限（3），请手动重试" {
		t.Errorf("vectorError = %v", got.VectorError)
	}
	if f.stream.addCount() != 0 {
		t.Errorf("达上限不应补投, got %d", f.stream.addCount())
	}
}

// TestRecovery关闭时不动 总开关。
func TestRecovery关闭时不动(t *testing.T) {
	f := newKBFixture()
	producer := NewVectorizeProducer(f.stream, f.store, discardLogger())
	scheduler := NewRecoveryScheduler(f.store, producer, RecoveryConfig{Enabled: false}, discardLogger())
	kb := sampleKB(0)
	kb.VectorStatus = VectorPending
	kb.VectorUpdatedAt = timePtr(f.now.Add(-2 * time.Hour))
	f.store.addKB(kb)

	scheduler.RecoverStuckTasks(context.Background(), f.now)
	if f.stream.addCount() != 0 {
		t.Errorf("关闭时不应补投, got %d", f.stream.addCount())
	}
}

// TestRecovery补投失败置FAILED 恢复投递失败 → PENDING → FAILED。
func TestRecovery补投失败置FAILED(t *testing.T) {
	f := newKBFixture()
	f.stream.addErr = errors.New("redis down")
	producer := NewVectorizeProducer(f.stream, f.store, discardLogger())
	scheduler := NewRecoveryScheduler(f.store, producer, RecoveryConfig{
		Enabled: true, PendingThreshold: 10 * time.Minute, ProcessingThreshold: 15 * time.Minute,
		Interval: time.Minute, BatchSize: 10, MaxRecoveryCount: 3,
	}, discardLogger())

	kb := sampleKB(0)
	kb.VectorStatus = VectorPending
	kb.VectorUpdatedAt = timePtr(f.now.Add(-11 * time.Minute))
	id := f.store.addKB(kb)

	scheduler.RecoverStuckTasks(context.Background(), f.now)

	got := f.store.snapshot(id)
	if got.VectorStatus != VectorFailed {
		t.Fatalf("状态 = %v, want FAILED（onSendFailed）", got.VectorStatus)
	}
	if got.VectorError == nil || *got.VectorError != "任务入队失败" {
		t.Errorf("vectorError = %v", got.VectorError)
	}
}

// ===== 解析服务 =====

// TestParseService下载解析清洗 端到端：下载 → 提取 → 清洗。
func TestParseService下载解析清洗(t *testing.T) {
	f := newKBFixture()
	// 上传一份 md（内容含图片链接行与分隔线，验证 Java cleanText 语义生效）。
	data := []byte("# 标题\nimage123.png\nhttps://cdn.example.com/x.png\n------\n正文内容\n\n\n\n结尾")
	f.storage.objects["knowledgebases/2026/10/29/abcd1234_笔记.md"] = data
	svc := NewParseService(f.storage, 2*time.Minute, 5*1024*1024, discardLogger())

	got, err := svc.DownloadAndParseContent(context.Background(), "knowledgebases/2026/10/29/abcd1234_笔记.md", "笔记.md")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, want := range []string{"# 标题", "正文内容", "结尾"} {
		if !strings.Contains(got, want) {
			t.Errorf("清洗后文本缺少 %q, got %q", want, got)
		}
	}
	if strings.Contains(got, "image123.png") || strings.Contains(got, "https://") || strings.Contains(got, "------") {
		t.Errorf("图片行/链接/分隔线应被清除, got %q", got)
	}
}

// TestParseService下载失败与空文件 Java 文案逐字。
func TestParseService下载失败与空文件(t *testing.T) {
	f := newKBFixture()
	svc := NewParseService(f.storage, 2*time.Minute, 5*1024*1024, discardLogger())

	_, err := svc.DownloadAndParseContent(context.Background(), "missing.md", "missing.md")
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Code != 500 || ae.Message != "下载文件失败" {
		t.Errorf("缺失对象错误 = %v", err)
	}

	f.storage.objects["empty.md"] = nil
	_, err = svc.DownloadAndParseContent(context.Background(), "empty.md", "empty.md")
	if !errors.As(err, &ae) || ae.Code != 500 || ae.Message != "下载文件失败" {
		t.Errorf("空文件错误 = %v", err)
	}
}

// TestParseService解析失败兜底 非法 PDF 字节 → "文件解析失败"。
func TestParseService解析失败兜底(t *testing.T) {
	f := newKBFixture()
	f.storage.objects["bad.pdf"] = []byte("%PDF-1.4 \x00\x01\x02 破损内容")
	svc := NewParseService(f.storage, 2*time.Minute, 5*1024*1024, discardLogger())

	_, err := svc.DownloadAndParseContent(context.Background(), "bad.pdf", "bad.pdf")
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Code != 500 || ae.Message != "文件解析失败" {
		t.Fatalf("解析失败错误 = %v", err)
	}
}

// TestParseService提取文本上限5MB 超 Java 写上限 → 解析失败。
func TestParseService提取文本上限5MB(t *testing.T) {
	f := newKBFixture()
	big := strings.Repeat("字", 5*1024*1024+1)
	f.storage.objects["big.txt"] = []byte(big)
	svc := NewParseService(f.storage, 2*time.Minute, 5*1024*1024, discardLogger())

	_, err := svc.DownloadAndParseContent(context.Background(), "big.txt", "big.txt")
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Code != 500 || ae.Message != "文件解析失败" {
		t.Fatalf("超上限错误 = %v", err)
	}
}

// TestParseService解析超时 隔离超时语义（Java future.get(2min) → cancel）。
func TestParseService解析超时(t *testing.T) {
	f := newKBFixture()
	// 足够大的文本保证解析耗时超过 1ns 的超时阈值。
	f.storage.objects["slow.txt"] = []byte(strings.Repeat("段落内容需要解析时间。\n", 200000))
	svc := NewParseService(f.storage, time.Nanosecond, 5*1024*1024, discardLogger())

	_, err := svc.DownloadAndParseContent(context.Background(), "slow.txt", "slow.txt")
	var ae *apperr.AppError
	if !errors.As(err, &ae) || ae.Code != 500 || ae.Message != "文件解析超时，请检查文件内容" {
		t.Fatalf("超时错误 = %v", err)
	}
	_ = docparse.CleanTextJava
}
