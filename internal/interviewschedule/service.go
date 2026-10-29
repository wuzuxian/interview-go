// 日程业务逻辑（Java InterviewScheduleService 逐步对齐）：创建强制 PENDING、
// 更新不动 status、列表三选一分支（start&end 范围 > status > 全量）、删除不校验
// 存在性、状态更新无流转校验；不存在统一 9001「面试日程不存在: {id}」。
package interviewschedule

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

// Service 日程服务。
type Service struct {
	store  Store
	logger *slog.Logger
}

// NewService 创建服务。
func NewService(store Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: store, logger: logger}
}

// scheduleNotFound 9001 + id 后缀（Java BusinessException(
// ErrorCode.INTERVIEW_SCHEDULE_NOT_FOUND, "面试日程不存在: " + id)）。
func scheduleNotFound(id int64) *apperr.AppError {
	return apperr.New(apperr.CodeScheduleNotFound, fmt.Sprintf("面试日程不存在: %d", id))
}

// Create 创建日程：拷贝 8 业务字段，status 强制 PENDING（客户端传什么都不看，
// Java create L30-37）。
func (s *Service) Create(ctx context.Context, req *CreateInterviewRequest) (*InterviewScheduleDTO, error) {
	entity := &InterviewScheduleEntity{
		CompanyName:   req.CompanyName,
		Position:      req.Position,
		InterviewTime: req.InterviewTime.Time(),
		InterviewType: req.InterviewType,
		MeetingLink:   req.MeetingLink,
		RoundNumber:   req.roundNumberOrDefault(),
		Interviewer:   req.Interviewer,
		Notes:         req.Notes,
		Status:        StatusPending,
	}
	if err := s.store.Create(ctx, entity); err != nil {
		return nil, err
	}
	s.logger.Info("创建面试记录",
		slog.String("companyName", req.CompanyName), slog.String("position", req.Position))
	return toDTO(entity), nil
}

// GetByID 详情；不存在 → 9001。
func (s *Service) GetByID(ctx context.Context, id int64) (*InterviewScheduleDTO, error) {
	entity, err := s.getByIDOrThrow(ctx, id)
	if err != nil {
		return nil, err
	}
	return toDTO(entity), nil
}

// GetAll 列表：start 与 end 同时非空 → 时间范围（范围优先，status 被忽略）；
// 否则 status 非空 → 按状态；否则全量（Java getAll L58-72 三选一分支逐字）。
// 无分页、无排序。
func (s *Service) GetAll(ctx context.Context, status *string, start, end *time.Time) ([]InterviewScheduleDTO, error) {
	var (
		entities []InterviewScheduleEntity
		err      error
	)
	switch {
	case start != nil && end != nil:
		entities, err = s.store.FindByInterviewTimeBetween(ctx, *start, *end)
	case status != nil && *status != "":
		st, ok := ParseInterviewStatus(*status)
		if !ok {
			return nil, apperr.BadRequest("无效的面试状态: " + *status)
		}
		entities, err = s.store.FindByStatus(ctx, st)
	default:
		entities, err = s.store.FindAll(ctx)
	}
	if err != nil {
		return nil, err
	}
	dtos := make([]InterviewScheduleDTO, 0, len(entities))
	for i := range entities {
		dtos = append(dtos, *toDTO(&entities[i]))
	}
	return dtos, nil
}

// Update 整条更新：拷贝 8 业务字段，id 与 status 保持不变（Java BeanUtils
// copyProperties(request, entity, "id", "status")）；不存在 → 9001。
func (s *Service) Update(ctx context.Context, id int64, req *CreateInterviewRequest) (*InterviewScheduleDTO, error) {
	entity, err := s.getByIDOrThrow(ctx, id)
	if err != nil {
		return nil, err
	}
	entity.CompanyName = req.CompanyName
	entity.Position = req.Position
	entity.InterviewTime = req.InterviewTime.Time()
	entity.InterviewType = req.InterviewType
	entity.MeetingLink = req.MeetingLink
	entity.RoundNumber = req.roundNumberOrDefault()
	entity.Interviewer = req.Interviewer
	entity.Notes = req.Notes
	if err := s.store.Save(ctx, entity); err != nil {
		return nil, err
	}
	s.logger.Info("更新面试记录", slog.Int64("id", id))
	return toDTO(entity), nil
}

// Delete 删除：物理删且不校验存在性（Java deleteById 语义），删除不存在 id
// 同样返回成功。
func (s *Service) Delete(ctx context.Context, id int64) error {
	return s.store.DeleteByID(ctx, id)
}

// UpdateStatus 更新状态：任意状态可跳任意状态，无流转校验（Java 现状）；
// 不存在 → 9001。
func (s *Service) UpdateStatus(ctx context.Context, id int64, status InterviewStatus) (*InterviewScheduleDTO, error) {
	entity, err := s.getByIDOrThrow(ctx, id)
	if err != nil {
		return nil, err
	}
	s.logger.Info("更新面试状态",
		slog.Int64("id", id), slog.String("status", string(status)))
	entity.Status = status
	if err := s.store.Save(ctx, entity); err != nil {
		return nil, err
	}
	return toDTO(entity), nil
}

// getByIDOrThrow 不存在 → 9001。
func (s *Service) getByIDOrThrow(ctx context.Context, id int64) (*InterviewScheduleEntity, error) {
	entity, err := s.store.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if entity == nil {
		return nil, scheduleNotFound(id)
	}
	return entity, nil
}

// roundNumberOrDefault 请求缺省轮次回落 1（Java CreateInterviewRequest 字段默认值；
// 显式 null 与缺失同路径，偏离清单第 3 条）。
func (r *CreateInterviewRequest) roundNumberOrDefault() *int {
	if r.RoundNumber == nil {
		n := 1
		return &n
	}
	return r.RoundNumber
}

// toDTO 实体转 DTO（Java BeanUtils.copyProperties 全字段同名拷贝）。
func toDTO(e *InterviewScheduleEntity) *InterviewScheduleDTO {
	return &InterviewScheduleDTO{
		ID:            e.ID,
		CompanyName:   e.CompanyName,
		Position:      e.Position,
		InterviewTime: NaiveTime(e.InterviewTime),
		InterviewType: e.InterviewType,
		MeetingLink:   e.MeetingLink,
		RoundNumber:   e.RoundNumber,
		Interviewer:   e.Interviewer,
		Notes:         e.Notes,
		Status:        e.Status,
		CreatedAt:     NaiveTime(derefTime(e.CreatedAt)),
		UpdatedAt:     NaiveTime(derefTime(e.UpdatedAt)),
	}
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ParseInterviewStatus 状态取值解析（四枚举，大小写敏感——Java valueOf 语义）。
func ParseInterviewStatus(s string) (InterviewStatus, bool) {
	switch st := InterviewStatus(strings.TrimSpace(s)); st {
	case StatusPending, StatusCompleted, StatusCancelled, StatusRescheduled:
		return st, true
	default:
		return "", false
	}
}
