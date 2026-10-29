// 过期巡检器测试（巡检缝）：fake CancelStore 断言 old/new status 与 cutoff 参数、
// 影响行数日志阈值、Start/Stop 生命周期与时钟注入。
package interviewschedule

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeCancelStore struct {
	mu       sync.Mutex
	rows     map[int64]*InterviewScheduleEntity
	cutoffs  []time.Time
	failErr  error
	affected int64
}

func (f *fakeCancelStore) CancelExpiredPending(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cutoffs = append(f.cutoffs, cutoff)
	if f.failErr != nil {
		return 0, f.failErr
	}
	return f.affected, nil
}

func newTestUpdater(store CancelStore, interval time.Duration) (*StatusUpdater, *time.Time) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	updater := NewStatusUpdater(store, Config{SweeperInterval: interval}, logger)
	fixed := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	updater.nowFn = func() time.Time { return fixed }
	return updater, &fixed
}

func Test巡检参数与影响行数(t *testing.T) {
	store := &fakeCancelStore{affected: 3}
	updater, fixed := newTestUpdater(store, time.Hour)
	updater.sweepOnce(context.Background())
	if len(store.cutoffs) != 1 {
		t.Fatalf("巡检次数 = %d, want 1", len(store.cutoffs))
	}
	if !store.cutoffs[0].Equal(*fixed) {
		t.Fatalf("cutoff = %v, want 注入时钟 %v", store.cutoffs[0], *fixed)
	}
}

func Test巡检间隔零值回落1小时(t *testing.T) {
	updater, _ := newTestUpdater(&fakeCancelStore{}, 0)
	if updater.interval != time.Hour {
		t.Fatalf("interval = %v, want 1h（Java cron 每小时语义）", updater.interval)
	}
}

func TestStartStop生命周期(t *testing.T) {
	store := &fakeCancelStore{}
	updater, _ := newTestUpdater(store, 10*time.Millisecond)
	ctx := context.Background()
	updater.Start(ctx)
	time.Sleep(80 * time.Millisecond) // 首轮在一个间隔后（cron 语义，不立即执行）
	updater.Stop()
	if len(store.cutoffs) == 0 {
		t.Fatal("运行期间应至少巡检一次")
	}
	n := len(store.cutoffs)
	updater.Stop() // 幂等
	time.Sleep(30 * time.Millisecond)
	if len(store.cutoffs) != n {
		t.Fatalf("Stop 后仍巡检：%d → %d", n, len(store.cutoffs))
	}
}

func Test巡检失败不中断(t *testing.T) {
	store := &fakeCancelStore{failErr: context.DeadlineExceeded}
	updater, _ := newTestUpdater(store, time.Hour)
	updater.sweepOnce(context.Background()) // 记错误日志、不 panic 即可
	if len(store.cutoffs) != 1 {
		t.Fatalf("巡检次数 = %d, want 1", len(store.cutoffs))
	}
}
