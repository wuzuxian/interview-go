// EndSessionIfInProgress 单测（WS 断开自动结束的会话域语义，对齐 Java
// VoiceInterviewService.endSessionIfInProgress L109-118）与升级校验读路径
// GetSessionEntity、来源白名单判定。
package voiceinterview

import (
	"context"
	"testing"
	"time"
)

// 仅 IN_PROGRESS 才结束：COMPLETED + endTime + 评估 PENDING + 提交后投递。
func TestEndSessionIfInProgress进行中会话结束并投递(t *testing.T) {
	f := newFixture()
	id := f.store.addSession(inProgressSession(time.Hour))

	if err := f.svc.EndSessionIfInProgress(context.Background(), id); err != nil {
		t.Fatalf("EndSessionIfInProgress: %v", err)
	}
	if got := len(f.store.updateEnd); got != 1 {
		t.Fatalf("应结束一次，实际 %d 次", got)
	}
	e := f.store.updateEnd[0]
	if statusEnum(e.Status) != VoiceStatusCompleted {
		t.Errorf("应置 COMPLETED: %v", e.Status)
	}
	if e.EndTime == nil {
		t.Error("应写 endTime")
	}
	if e.ActualDuration == nil {
		t.Error("应写 actualDuration")
	}
	if e.EvaluateStatus == nil || *e.EvaluateStatus != VoiceEvaluatePending {
		t.Errorf("应置评估 PENDING: %v", e.EvaluateStatus)
	}
	// 事务提交后投递（rec 顺序：update-end → xadd）。
	if !f.rec.has("update-end") || !f.rec.has("xadd") {
		t.Fatalf("事件顺序不完整: %v", f.rec.list())
	}
	endedAt := -1
	queuedAt := -1
	for i, ev := range f.rec.list() {
		if ev == "update-end" {
			endedAt = i
		}
		if ev == "xadd" {
			queuedAt = i
		}
	}
	if endedAt == -1 || queuedAt == -1 || endedAt > queuedAt {
		t.Errorf("评估投递应在结束之后: %v", f.rec.list())
	}
}

// 非 IN_PROGRESS（PAUSED/COMPLETED）静默返回，不结束、不投递（Java 原样返回）。
func TestEndSessionIfInProgress非进行中不动(t *testing.T) {
	f := newFixture()
	paused := VoiceStatusPaused
	p := inProgressSession(time.Hour)
	p.Status = &paused
	pausedID := f.store.addSession(p)

	done := VoiceStatusCompleted
	c := inProgressSession(time.Hour)
	c.Status = &done
	doneID := f.store.addSession(c)

	for _, id := range []int64{pausedID, doneID} {
		if err := f.svc.EndSessionIfInProgress(context.Background(), id); err != nil {
			t.Fatalf("非进行中会话应静默返回 nil: %v", err)
		}
	}
	if len(f.store.updateEnd) != 0 {
		t.Errorf("非进行中会话不应结束: %v", f.store.updateEnd)
	}
	if len(f.stream.adds) != 0 {
		t.Errorf("非进行中会话不应投递评估: %v", f.stream.adds)
	}
}

// 会话不存在静默返回（Java orElse(null) → return）。
func TestEndSessionIfInProgress不存在静默返回(t *testing.T) {
	f := newFixture()
	if err := f.svc.EndSessionIfInProgress(context.Background(), 424242); err != nil {
		t.Fatalf("不存在的会话应返回 nil: %v", err)
	}
	if len(f.store.updateEnd) != 0 || len(f.stream.adds) != 0 {
		t.Error("不存在的会话不应有任何副作用")
	}
}

// GetSessionEntity：存在返回实体、不存在返回 (nil, nil)。
func TestGetSessionEntity读路径(t *testing.T) {
	f := newFixture()
	id := f.store.addSession(inProgressSession(time.Hour))

	e, err := f.svc.GetSessionEntity(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSessionEntity: %v", err)
	}
	if e == nil || e.ID != id {
		t.Fatalf("应返回会话实体: %v", e)
	}

	e, err = f.svc.GetSessionEntity(context.Background(), 88888)
	if err != nil {
		t.Fatalf("不存在的会话不应报错: %v", err)
	}
	if e != nil {
		t.Errorf("不存在的会话应返回 nil: %v", e)
	}
}

// originAllowed 升级来源判定（空 Origin / 空白名单 / "*" / 精确命中）。
func TestOriginAllowed来源判定(t *testing.T) {
	cases := []struct {
		name     string
		origins  []string
		origin   string
		expected bool
	}{
		{"空Origin放行", []string{"https://a.test"}, "", true},
		{"空白名单放行全部", nil, "https://any.test", true},
		{"星号放行全部", []string{"*"}, "https://any.test", true},
		{"精确命中放行", []string{"https://a.test", "https://b.test"}, "https://b.test", true},
		{"未命中拒绝", []string{"https://a.test"}, "https://evil.test", false},
		{"空白归一命中", []string{" https://a.test "}, "https://a.test", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := originAllowed(tc.origins, tc.origin); got != tc.expected {
				t.Errorf("originAllowed(%v, %q) = %v, want %v", tc.origins, tc.origin, got, tc.expected)
			}
		})
	}
}
