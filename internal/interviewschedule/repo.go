// 面试日程数据访问：查询语义逐条对齐 Java InterviewScheduleRepository 派生查询
// （findAll 不带排序、Between 双端闭区间）；列表无分页。SQL 语义由票 04 smoke
// 真库兜底，单测以内存 fake 测编排决策。
package interviewschedule

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// Store 面试日程模块数据面接缝（生产实现 *Repo，测试注入内存 fake）。
type Store interface {
	Create(ctx context.Context, e *InterviewScheduleEntity) error
	Save(ctx context.Context, e *InterviewScheduleEntity) error
	FindByID(ctx context.Context, id int64) (*InterviewScheduleEntity, error)
	FindAll(ctx context.Context) ([]InterviewScheduleEntity, error)
	FindByStatus(ctx context.Context, status InterviewStatus) ([]InterviewScheduleEntity, error)
	FindByInterviewTimeBetween(ctx context.Context, start, end time.Time) ([]InterviewScheduleEntity, error)
	DeleteByID(ctx context.Context, id int64) error
}

// Repo 生产实现。
type Repo struct {
	db *gorm.DB
}

// NewRepo 创建生产实现。
func NewRepo(db *gorm.DB) *Repo { return &Repo{db: db} }

// Create 插入（Java repository.save；ID 由数据库 identity 派发，回填 e.ID）。
func (r *Repo) Create(ctx context.Context, e *InterviewScheduleEntity) error {
	return r.db.WithContext(ctx).Create(e).Error
}

// Save 全量更新（Java repository.save 对托管实体的 UPDATE；GORM 按字段名约定
// 刷 updated_at，语义对齐 @PreUpdate）。
func (r *Repo) Save(ctx context.Context, e *InterviewScheduleEntity) error {
	return r.db.WithContext(ctx).Save(e).Error
}

// FindByID 按主键查（Java findById）；不存在返回 (nil, nil)，由服务层落 9001。
func (r *Repo) FindByID(ctx context.Context, id int64) (*InterviewScheduleEntity, error) {
	var e InterviewScheduleEntity
	err := r.db.WithContext(ctx).First(&e, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// FindAll 全量（Java findAll；无 ORDER BY，与 Java 同为数据库自然返回序）。
func (r *Repo) FindAll(ctx context.Context) ([]InterviewScheduleEntity, error) {
	var list []InterviewScheduleEntity
	err := r.db.WithContext(ctx).Find(&list).Error
	return list, err
}

// FindByStatus 按状态过滤（Java findByStatus）。
func (r *Repo) FindByStatus(ctx context.Context, status InterviewStatus) ([]InterviewScheduleEntity, error) {
	var list []InterviewScheduleEntity
	err := r.db.WithContext(ctx).Where("status = ?", status).Find(&list).Error
	return list, err
}

// FindByInterviewTimeBetween 时间范围过滤（Java findByInterviewTimeBetween，
// BETWEEN 双端闭区间）。
func (r *Repo) FindByInterviewTimeBetween(ctx context.Context, start, end time.Time) ([]InterviewScheduleEntity, error) {
	var list []InterviewScheduleEntity
	err := r.db.WithContext(ctx).Where("interview_time BETWEEN ? AND ?", start, end).Find(&list).Error
	return list, err
}

// DeleteByID 物理删除（Java deleteById；不存在时影响 0 行、不报错——服务层不校验，
// 恒返回成功）。
func (r *Repo) DeleteByID(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&InterviewScheduleEntity{}, "id = ?", id).Error
}

// CancelExpiredPending 过期取消（Java updateStatusByStatusAndInterviewTimeBefore
// 的 JPQL 批量 UPDATE 逐字）：status=PENDING 且 interview_time < cutoff →
// CANCELLED，返回影响行数。UpdateColumns 绕过 GORM hooks 与 updated_at 自动
// 更新——对齐 JPQL 批量 UPDATE 不触发 @PreUpdate、updated_at 保持原值的语义。
func (r *Repo) CancelExpiredPending(ctx context.Context, cutoff time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Model(&InterviewScheduleEntity{}).
		Where("status = ? AND interview_time < ?", StatusPending, cutoff).
		UpdateColumn("status", StatusCancelled)
	return res.RowsAffected, res.Error
}
