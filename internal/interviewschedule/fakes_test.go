// 测试内存 fake：模拟 GORM Repo 行为（identity 主键、createdAt/updatedAt 落库、
// Between 双端闭区间、插入序返回）。
package interviewschedule

import (
	"context"
	"slices"
	"sync"
	"time"
)

type fakeStore struct {
	mu   sync.Mutex
	seq  int64
	rows map[int64]*InterviewScheduleEntity
	// order 保持插入序，供 FindAll 稳定断言（真库为自然返回序，无排序保证）。
	order []int64
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[int64]*InterviewScheduleEntity{}}
}

func (f *fakeStore) Create(_ context.Context, e *InterviewScheduleEntity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	e.ID = f.seq
	now := testNow
	e.CreatedAt = &now
	e.UpdatedAt = &now
	cp := *e
	f.rows[e.ID] = &cp
	f.order = append(f.order, e.ID)
	return nil
}

func (f *fakeStore) Save(_ context.Context, e *InterviewScheduleEntity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[e.ID]; !ok {
		return nil // 真库 Save 不存在行不会发生（服务层先查后存），防御性空转
	}
	now := testNow
	e.UpdatedAt = &now
	cp := *e
	f.rows[e.ID] = &cp
	return nil
}

func (f *fakeStore) FindByID(_ context.Context, id int64) (*InterviewScheduleEntity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.rows[id]; ok {
		cp := *e
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeStore) FindAll(_ context.Context) ([]InterviewScheduleEntity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]InterviewScheduleEntity, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, *f.rows[id])
	}
	return out, nil
}

func (f *fakeStore) FindByStatus(_ context.Context, status InterviewStatus) ([]InterviewScheduleEntity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]InterviewScheduleEntity, 0)
	for _, id := range f.order {
		if f.rows[id].Status == status {
			out = append(out, *f.rows[id])
		}
	}
	return out, nil
}

func (f *fakeStore) FindByInterviewTimeBetween(_ context.Context, start, end time.Time) ([]InterviewScheduleEntity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]InterviewScheduleEntity, 0)
	for _, id := range f.order {
		t := f.rows[id].InterviewTime
		if !t.Before(start) && !t.After(end) {
			out = append(out, *f.rows[id])
		}
	}
	return out, nil
}

func (f *fakeStore) DeleteByID(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, id)
	f.order = slices.DeleteFunc(f.order, func(v int64) bool { return v == id })
	return nil
}
