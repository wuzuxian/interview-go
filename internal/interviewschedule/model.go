// Package interviewschedule 面试日程模块（Java interview-guide
// modules/interviewschedule 的 Go 复刻）：/api/interview-schedule 全部接口
// （邀约解析/CRUD/状态更新）+ 过期待面试巡检。模块与简历、面试场次、语音面试、
// 知识库零数据关联（源设计：interview_schedule 表无外键无 user_id），仅复用
// result/apperr 响应设施与 interview 的 prompt 防注入设施。
package interviewschedule

import "time"

// InterviewStatus 日程状态（Java InterviewStatus 枚举，四值；无 CONFIRMED，
// 状态间可任意跳转、无状态机校验；RESCHEDULED 仅为保留取值，无接口自动置入）。
type InterviewStatus string

const (
	StatusPending     InterviewStatus = "PENDING"
	StatusCompleted   InterviewStatus = "COMPLETED"
	StatusCancelled   InterviewStatus = "CANCELLED"
	StatusRescheduled InterviewStatus = "RESCHEDULED"
)

// InterviewScheduleEntity 对应表 interview_schedule（Java Flyway V1 建表，
// 应用不做迁移，ADR 0001）：无外键、无 user_id、除主键无二级索引，status 带
// CHECK 约束（迁移侧已建）。可空列一律指针。createdAt/updatedAt 由 GORM 按字段
// 名约定维护（创建刷两者、更新刷 UpdatedAt），语义对齐 Java @PrePersist/@PreUpdate；
// 过期巡检走 UpdateColumns 批量更新，绕过该约定（对齐 Java JPQL 批量 UPDATE
// 不触发 @PreUpdate、updated_at 不变的语义）。
type InterviewScheduleEntity struct {
	ID            int64           `gorm:"column:id;primaryKey;autoIncrement"`
	CompanyName   string          `gorm:"column:company_name;not null"`
	Position      string          `gorm:"column:position;not null"`
	InterviewTime time.Time       `gorm:"column:interview_time;not null"`
	InterviewType *string         `gorm:"column:interview_type"` // ONSITE, VIDEO, PHONE（纯字符串存储，无枚举校验）
	MeetingLink   *string         `gorm:"column:meeting_link;type:text"`
	RoundNumber   *int            `gorm:"column:round_number"`
	Interviewer   *string         `gorm:"column:interviewer"`
	Notes         *string         `gorm:"column:notes;type:text"`
	Status        InterviewStatus `gorm:"column:status;not null"`
	CreatedAt     *time.Time      `gorm:"column:created_at"`
	UpdatedAt     *time.Time      `gorm:"column:updated_at"`
}

// TableName 表名。
func (InterviewScheduleEntity) TableName() string { return "interview_schedule" }
