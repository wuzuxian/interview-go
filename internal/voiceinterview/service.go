// Service 语音面试会话域：创建/查询/结束/暂停/恢复/列表/删除/消息历史/阶段切换，
// 流程与 Java VoiceInterviewService（786 行）逐字对齐；结束后的评估入队在事务
// 提交后投递 Redis Stream（voice:evaluate:stream），失败置评估 FAILED。
// GET/POST evaluation 两接口属票 08，不在本票。
package voiceinterview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

// 会话域默认值，对齐 Java CommonConstants.InterviewDefaults 与 VoiceInterviewService 常量。
const (
	DefaultSkillID         = "java-backend" // Java InterviewDefaults.SKILL_ID
	DefaultDifficulty      = "mid"          // Java InterviewDefaults.DIFFICULTY
	DefaultPlannedDuration = 30
	DefaultUserID          = "default" // Java DEFAULT_USER_ID
	DefaultPauseReason     = "user_initiated"
)

// sendFailedMessage XADD 失败写入 evaluate_error 的固定文案
// （Java AbstractStreamProducer.sendTask → onSendFailed(payload, "任务入队失败")）。
const sendFailedMessage = "任务入队失败"

// SessionStore 会话持久化的消费方接缝（生产实现为 *Repo，测试注入内存替身）。
type SessionStore interface {
	CreateSession(ctx context.Context, e *SessionEntity) error
	FindSessionByID(ctx context.Context, sessionID int64) (*SessionEntity, error)
	UpdatePause(ctx context.Context, sessionID int64, pausedAt time.Time) error
	UpdateResume(ctx context.Context, sessionID int64, resumedAt time.Time) error
	UpdateEnd(ctx context.Context, e *SessionEntity) error
	UpdatePhase(ctx context.Context, sessionID int64, phase VoicePhase) error
	UpdateEvaluateStatus(ctx context.Context, sessionID int64, status VoiceEvaluateStatus, evalErr *string) error
	ListByUser(ctx context.Context, userID string, status *VoiceSessionStatus) ([]SessionEntity, error)
	CountDialogueMessages(ctx context.Context, sessionID int64) (int64, error)
	DeleteSessionCascade(ctx context.Context, sessionID int64) error
	FindDialogueMessages(ctx context.Context, sessionID int64) ([]MessageEntity, error)
}

// Service 会话域服务。
type Service struct {
	repo   SessionStore
	cache  SessionCacheStore
	stream VoiceEvalStream // 评估流入队接缝；nil 时不投递（早期票/测试）
	logger *slog.Logger
}

// ServiceDeps 服务依赖聚合。
type ServiceDeps struct {
	Repo   SessionStore
	Cache  SessionCacheStore
	Stream VoiceEvalStream
	Logger *slog.Logger
}

// NewService 创建会话域服务。
func NewService(deps ServiceDeps) *Service {
	return &Service{repo: deps.Repo, cache: deps.Cache, stream: deps.Stream, logger: deps.Logger}
}

// ===== 创建 =====

// normalizedCreate 创建请求的默认值归一结果（纯逻辑，单测覆盖）。
type normalizedCreate struct {
	skillID         string
	difficulty      string
	llmProvider     *string
	intro           bool
	tech            bool
	project         bool
	hr              bool
	plannedDuration int
}

// normalizeCreateRequest 补齐 Java 侧默认值：skillId 缺省 java-backend（仅判 null，
// 空串原样透传）、difficulty 缺省 mid、intro 缺省 false / tech/project/hr 缺省 true
// （@Builder.Default）、plannedDuration 缺省 30、llmProvider null 或空白（isBlank）
// → null（Java createSession L74-77）。
func normalizeCreateRequest(req CreateSessionRequest) normalizedCreate {
	n := normalizedCreate{
		skillID:         DefaultSkillID,
		difficulty:      DefaultDifficulty,
		llmProvider:     nil,
		intro:           boolValue(req.IntroEnabled),
		tech:            boolDefault(req.TechEnabled, true),
		project:         boolDefault(req.ProjectEnabled, true),
		hr:              boolDefault(req.HrEnabled, true),
		plannedDuration: DefaultPlannedDuration,
	}
	if req.SkillID != nil {
		n.skillID = *req.SkillID
	}
	if req.Difficulty != nil {
		n.difficulty = *req.Difficulty
	}
	if req.LLMProvider != nil && strings.TrimSpace(*req.LLMProvider) != "" {
		n.llmProvider = req.LLMProvider
	}
	if req.PlannedDuration != nil {
		n.plannedDuration = *req.PlannedDuration
	}
	return n
}

// CreateSession POST /api/voice-interview/sessions：默认值归一 → 落库（userId 固定
// default、roleType 恒取 skillId、status IN_PROGRESS）→ 写缓存 → 响应（含相对路径
// webSocketUrl）。对齐 Java createSession L72-102。
func (s *Service) CreateSession(ctx context.Context, req CreateSessionRequest) (*SessionResponseDTO, error) {
	n := normalizeCreateRequest(req)
	phase := determineFirstPhase(n.intro, n.tech, n.project, n.hr)
	e := &SessionEntity{
		UserID:          stringPtr(DefaultUserID),
		RoleType:        n.skillID,
		SkillID:         stringPtr(n.skillID),
		Difficulty:      stringPtr(n.difficulty),
		CustomJdText:    req.CustomJdText,
		ResumeID:        req.ResumeID,
		IntroEnabled:    boolPtr(n.intro),
		TechEnabled:     boolPtr(n.tech),
		ProjectEnabled:  boolPtr(n.project),
		HrEnabled:       boolPtr(n.hr),
		LLMProvider:     n.llmProvider,
		CurrentPhase:    phasePtr(phase),
		Status:          statusPtr(VoiceStatusInProgress),
		PlannedDuration: intPtr(n.plannedDuration),
	}
	if err := s.repo.CreateSession(ctx, e); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "创建语音面试会话失败", err)
	}
	s.cacheSession(ctx, e)

	s.logger.Info("已创建语音面试会话",
		slog.Int64("sessionId", e.ID),
		slog.String("skillId", n.skillID),
		slog.String("currentPhase", string(phase)))
	return buildSessionResponse(e), nil
}

// ===== 查询 =====

// getSession 读路径：缓存优先、DB 兜底、不回填缓存（对齐 Java getSession L172-189）。
// 都不存在返回 (nil, nil)。
func (s *Service) getSession(ctx context.Context, sessionID int64) (*SessionEntity, error) {
	cached, err := s.cache.Get(ctx, sessionID)
	if err != nil {
		s.logger.Warn("读语音会话缓存失败，回源 DB",
			slog.Int64("sessionId", sessionID), slog.String("error", err.Error()))
	}
	if cached != nil {
		return cachedToEntity(cached), nil
	}
	e, err := s.repo.FindSessionByID(ctx, sessionID)
	if errors.Is(err, ErrSessionNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

// GetSessionDTO GET /sessions/{id}：不存在返回 (nil, nil)，控制器转
// Result.error("Session not found: {id}")（对齐 Java getSessionDTO + Controller L69-75）。
func (s *Service) GetSessionDTO(ctx context.Context, sessionID int64) (*SessionResponseDTO, error) {
	e, err := s.getSession(ctx, sessionID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询语音面试会话失败", err)
	}
	if e == nil {
		return nil, nil
	}
	return buildSessionResponse(e), nil
}

// ===== 结束 =====

// fillEndOfSession 结束时的实体字段变更（纯函数，对齐 Java endSession L140-152）：
// endTime、currentPhase=COMPLETED、status=COMPLETED、actualDuration（秒，自
// startTime 起算）、evaluateStatus=PENDING。startTime 为空按零值起算（API 路径
// 不可能：创建时 @PrePersist/repo 已写入，仅防御脏数据）。
func fillEndOfSession(e *SessionEntity, now time.Time) {
	e.EndTime = &now
	e.CurrentPhase = phasePtr(PhaseCompleted)
	e.Status = statusPtr(VoiceStatusCompleted)
	var start time.Time
	if e.StartTime != nil {
		start = *e.StartTime
	}
	secs := int(now.Sub(start).Seconds())
	e.ActualDuration = &secs
	e.EvaluateStatus = evalPtr(VoiceEvaluatePending)
}

// EndSession POST /sessions/{id}/end：置 COMPLETED + PENDING 并在落库提交后投递
// 评估流。会话不存在仅告警并返回成功（对齐 Java endSession L131-134 的 warn +
// return，Result 仍为 success）。评估查询/重试接口属票 08。
func (s *Service) EndSession(ctx context.Context, sessionID int64) error {
	e, err := s.getSession(ctx, sessionID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternalError, "查询语音面试会话失败", err)
	}
	if e == nil {
		s.logger.Warn("结束会话未找到，跳过", slog.Int64("sessionId", sessionID))
		return nil
	}

	// 单条更新即 Java @Transactional endSession 的事务体；返回 nil 即已提交，
	// 之后的投递对齐 TransactionSynchronization.afterCommit。
	now := time.Now()
	fillEndOfSession(e, now)
	if err := s.repo.UpdateEnd(ctx, e); err != nil {
		return apperr.Wrap(apperr.CodeInternalError, "结束语音面试会话失败", err)
	}
	s.invalidateSessionCache(ctx, e.ID)

	s.logger.Info("语音面试会话已结束，评估已触发",
		slog.Int64("sessionId", e.ID),
		slog.Int("actualDuration", intValue(e.ActualDuration)))
	s.sendEvaluateTaskAfterCommit(ctx, e.ID)
	return nil
}

// ===== 暂停/恢复 =====

// pauseGuard 仅 IN_PROGRESS 可暂停；其余 400 "会话状态为 {status}，无法暂停"
// （Java pauseSession L401-405 消息逐字）。
func pauseGuard(status VoiceSessionStatus) error {
	if status != VoiceStatusInProgress {
		return apperr.BadRequest("会话状态为 " + string(status) + "，无法暂停")
	}
	return nil
}

// resumeGuard 仅 PAUSED 可恢复；其余 400 "会话状态为 {status}，无法恢复"
// （Java resumeSession L430-434 消息逐字）。
func resumeGuard(status VoiceSessionStatus) error {
	if status != VoiceStatusPaused {
		return apperr.BadRequest("会话状态为 " + string(status) + "，无法恢复")
	}
	return nil
}

// PauseSession PUT /sessions/{id}/pause：仅置状态与失效缓存，不断连（WS 侧属于
// 票 04/09）。对齐 Java pauseSession L395-414：走 DB（不走缓存读），不存在 →
// 404 "会话不存在: {id}"。
func (s *Service) PauseSession(ctx context.Context, sessionID int64, reason string) error {
	e, err := s.repo.FindSessionByID(ctx, sessionID)
	if errors.Is(err, ErrSessionNotFound) {
		return apperr.New(apperr.CodeNotFound, "会话不存在: "+strconv.FormatInt(sessionID, 10))
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternalError, "查询语音面试会话失败", err)
	}
	if err := pauseGuard(statusEnum(e.Status)); err != nil {
		return err
	}

	now := time.Now()
	if err := s.repo.UpdatePause(ctx, sessionID, now); err != nil {
		return apperr.Wrap(apperr.CodeInternalError, "暂停语音面试会话失败", err)
	}
	s.invalidateSessionCache(ctx, sessionID)

	s.logger.Info("语音面试会话已暂停",
		slog.Int64("sessionId", sessionID), slog.String("reason", reason))
	return nil
}

// ResumeSession PUT /sessions/{id}/resume：仅 PAUSED 可恢复，落 resumedAt、重建
// 缓存，返回含 webSocketUrl 的完整响应。对齐 Java resumeSession L424-446。
func (s *Service) ResumeSession(ctx context.Context, sessionID int64) (*SessionResponseDTO, error) {
	e, err := s.repo.FindSessionByID(ctx, sessionID)
	if errors.Is(err, ErrSessionNotFound) {
		return nil, apperr.New(apperr.CodeNotFound, "会话不存在: "+strconv.FormatInt(sessionID, 10))
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询语音面试会话失败", err)
	}
	if err := resumeGuard(statusEnum(e.Status)); err != nil {
		return nil, err
	}

	now := time.Now()
	if err := s.repo.UpdateResume(ctx, sessionID, now); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "恢复语音面试会话失败", err)
	}
	e.Status = statusPtr(VoiceStatusInProgress)
	e.ResumedAt = &now
	s.cacheSession(ctx, e)

	s.logger.Info("语音面试会话已恢复",
		slog.Int64("sessionId", sessionID))
	return buildSessionResponse(e), nil
}

// ===== 列表 =====

// GetAllSessions GET /sessions?userId&status：userId 缺省固定 default；status 非空
// 时归一大写并校验（非法值 → 400，对齐 Java valueOf 的 IllegalArgumentException →
// GlobalExceptionHandler 400）；每场附非 SUMMARY 消息计数。对齐 Java
// getAllSessions L456-482。
func (s *Service) GetAllSessions(ctx context.Context, userID, status string) ([]SessionMetaDTO, error) {
	if userID == "" {
		userID = DefaultUserID
	}
	var statusFilter *VoiceSessionStatus
	if status != "" {
		parsed, ok := parseSessionStatus(strings.ToUpper(status))
		if !ok {
			return nil, apperr.BadRequest("非法的会话状态过滤值: " + status)
		}
		statusFilter = &parsed
	}

	list, err := s.repo.ListByUser(ctx, userID, statusFilter)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询语音面试会话列表失败", err)
	}
	out := make([]SessionMetaDTO, 0, len(list))
	for i := range list {
		e := &list[i]
		count, err := s.repo.CountDialogueMessages(ctx, e.ID)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternalError, "统计语音面试消息数失败", err)
		}
		out = append(out, SessionMetaDTO{
			SessionID:      e.ID,
			RoleType:       e.RoleType,
			Status:         statusValue(e.Status),
			CurrentPhase:   phaseValue(e.CurrentPhase),
			CreatedAt:      e.CreatedAt,
			UpdatedAt:      e.UpdatedAt,
			ActualDuration: e.ActualDuration,
			MessageCount:   count,
			EvaluateStatus: evalStringPtr(e.EvaluateStatus),
			EvaluateError:  e.EvaluateError,
		})
	}
	return out, nil
}

// ===== 删除 =====

// DeleteSession DELETE /sessions/{id}：级联删消息与评估（事务内语句顺序
// 评估 → 消息 → 会话在 repo）；不存在 → 10001 "会话不存在: {id}"（Java
// VOICE_SESSION_NOT_FOUND + 自定义消息逐字）。对齐 Java deleteSession L681-690。
func (s *Service) DeleteSession(ctx context.Context, sessionID int64) error {
	err := s.repo.DeleteSessionCascade(ctx, sessionID)
	if errors.Is(err, ErrSessionNotFound) {
		return apperr.VoiceSessionNotFoundWithMessage("会话不存在: " + strconv.FormatInt(sessionID, 10))
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternalError, "删除语音面试会话失败", err)
	}
	s.logger.Info("已删除语音面试会话", slog.Int64("sessionId", sessionID))
	return nil
}

// ===== 消息历史 =====

// GetConversationHistoryDTO GET /sessions/{id}/messages：非 SUMMARY 消息按
// sequence_num 升序（排除与排序为 repo SQL 语义），映射八字段 DTO。对齐 Java
// getConversationHistoryDTO L372-385。
func (s *Service) GetConversationHistoryDTO(ctx context.Context, sessionID int64) ([]VoiceInterviewMessageDTO, error) {
	rows, err := s.repo.FindDialogueMessages(ctx, sessionID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询语音面试消息失败", err)
	}
	out := make([]VoiceInterviewMessageDTO, 0, len(rows))
	for _, m := range rows {
		out = append(out, VoiceInterviewMessageDTO{
			ID:                 m.ID,
			SessionID:          m.SessionID,
			MessageType:        m.MessageType,
			Phase:              phaseNamePtr(m.Phase),
			UserRecognizedText: m.UserRecognizedText,
			AiGeneratedText:    m.AiGeneratedText,
			Timestamp:          m.Timestamp,
			SequenceNum:        m.SequenceNum,
		})
	}
	return out, nil
}

// ===== 阶段切换（WS start_phase 的会话服务入口，票 04 消费）=====

// StartPhase 切换当前阶段：非法阶段仅记日志不改状态（对齐 Java startPhase L219-221
// 捕获 valueOf 异常只 log）；会话不存在仅告警。
func (s *Service) StartPhase(ctx context.Context, sessionID int64, phase string) error {
	e, err := s.getSession(ctx, sessionID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternalError, "查询语音面试会话失败", err)
	}
	if e == nil {
		s.logger.Warn("切换阶段未找到会话，跳过", slog.Int64("sessionId", sessionID))
		return nil
	}
	p, ok := parsePhase(strings.ToUpper(phase))
	if !ok {
		s.logger.Error("非法阶段字符串，忽略", slog.Int64("sessionId", sessionID), slog.String("phase", phase))
		return nil
	}
	old := phaseValue(e.CurrentPhase)
	if err := s.repo.UpdatePhase(ctx, sessionID, p); err != nil {
		return apperr.Wrap(apperr.CodeInternalError, "更新语音面试阶段失败", err)
	}
	e.CurrentPhase = &p
	s.cacheSession(ctx, e)

	s.logger.Info("语音面试会话已切换阶段",
		slog.Int64("sessionId", sessionID),
		slog.String("from", old), slog.String("to", string(p)))
	return nil
}

// ===== 阶段机顺延（Java getNextPhase L554-572，票 05 阶段机消费）=====

// getNextPhase 当前阶段之后的下一个启用阶段；全关或已到尾段返回 COMPLETED。
// currentPhase 为空时取首个启用阶段（对齐 Java getFirstEnabledPhase）。
func getNextPhase(e *SessionEntity) VoicePhase {
	current := phaseValue(e.CurrentPhase)
	switch VoicePhase(current) {
	case PhaseIntro:
		if boolValue(e.TechEnabled) {
			return PhaseTech
		}
		return nextAfterTech(e)
	case PhaseTech:
		return nextAfterTech(e)
	case PhaseProject:
		if boolValue(e.HrEnabled) {
			return PhaseHR
		}
		return PhaseCompleted
	default: // HR、COMPLETED 及空值
		if current == "" {
			return firstEnabledPhase(e)
		}
		return PhaseCompleted
	}
}

func nextAfterTech(e *SessionEntity) VoicePhase {
	if boolValue(e.ProjectEnabled) {
		return PhaseProject
	}
	if boolValue(e.HrEnabled) {
		return PhaseHR
	}
	return PhaseCompleted
}

func firstEnabledPhase(e *SessionEntity) VoicePhase {
	if boolValue(e.IntroEnabled) {
		return PhaseIntro
	}
	if boolValue(e.TechEnabled) {
		return PhaseTech
	}
	if boolValue(e.ProjectEnabled) {
		return PhaseProject
	}
	if boolValue(e.HrEnabled) {
		return PhaseHR
	}
	return PhaseCompleted
}

// determineFirstPhase 创建时按启用开关定首阶段（Java determineFirstPhase L580-586）。
func determineFirstPhase(intro, tech, project, hr bool) VoicePhase {
	switch {
	case intro:
		return PhaseIntro
	case tech:
		return PhaseTech
	case project:
		return PhaseProject
	case hr:
		return PhaseHR
	default:
		return PhaseCompleted
	}
}

// ===== 评估入队（事务提交后）=====

// sendEvaluateTaskAfterCommit 评估流投递：仅在落库事务提交成功后调用（gorm
// Transaction 返回 nil 即已提交），对齐 Java TransactionSynchronization.afterCommit。
// XADD 失败不向调用方报错，改为置评估 FAILED + "任务入队失败"（对齐 Java
// VoiceEvaluateStreamProducer.onSendFailed）。无消费者时消息静默堆积。
func (s *Service) sendEvaluateTaskAfterCommit(ctx context.Context, sessionID int64) {
	if s.stream == nil {
		return
	}
	messageID, err := s.stream.XAddTask(ctx, buildEvaluateTaskMessage(sessionID))
	if err != nil {
		s.logger.Error("发送语音面试评估任务失败",
			slog.String("voiceSessionId", strconv.FormatInt(sessionID, 10)),
			slog.String("error", err.Error()))
		s.UpdateEvaluateStatus(ctx, sessionID, VoiceEvaluateFailed, stringPtr(sendFailedMessage))
		return
	}
	s.logger.Info("语音面试评估任务已发送到Stream",
		slog.String("stream", VoiceEvalStreamKey),
		slog.String("messageId", messageID),
		slog.String("voiceSessionId", strconv.FormatInt(sessionID, 10)))
}

// UpdateEvaluateStatus 评估状态写入 + 失效缓存；落库失败仅记日志（对齐 Java
// updateEvaluateStatus L639-652 的 try/catch，失效缓存与落库同在 try 块内）。
// 票 08 的消费/恢复链路复用本方法。
func (s *Service) UpdateEvaluateStatus(ctx context.Context, sessionID int64, status VoiceEvaluateStatus, evalErr *string) {
	if err := s.repo.UpdateEvaluateStatus(ctx, sessionID, status, evalErr); err != nil {
		s.logger.Error("更新语音面试评估状态失败",
			slog.Int64("sessionId", sessionID),
			slog.String("status", string(status)),
			slog.String("error", err.Error()))
		return
	}
	s.invalidateSessionCache(ctx, sessionID)
	s.logger.Debug("语音面试评估状态已更新",
		slog.Int64("sessionId", sessionID), slog.String("status", string(status)))
}

// ===== 缓存 =====

func (s *Service) cacheSession(ctx context.Context, e *SessionEntity) {
	if err := s.cache.Save(ctx, entityToCached(e)); err != nil {
		s.logger.Warn("写语音会话缓存失败",
			slog.Int64("sessionId", e.ID), slog.String("error", err.Error()))
	}
}

func (s *Service) invalidateSessionCache(ctx context.Context, sessionID int64) {
	if err := s.cache.Invalidate(ctx, sessionID); err != nil {
		s.logger.Warn("失效语音会话缓存失败",
			slog.Int64("sessionId", sessionID), slog.String("error", err.Error()))
	}
}

// ===== 响应构造 =====

// buildSessionResponse 会话实体 → 响应 DTO；webSocketUrl 为相对路径
// /ws/voice-interview/{id}（Java buildSessionResponse L599-609 逐字）。
func buildSessionResponse(e *SessionEntity) *SessionResponseDTO {
	return &SessionResponseDTO{
		SessionID:       e.ID,
		RoleType:        e.RoleType,
		CurrentPhase:    phaseValue(e.CurrentPhase),
		Status:          statusValue(e.Status),
		StartTime:       e.StartTime,
		PlannedDuration: e.PlannedDuration,
		WebSocketURL:    fmt.Sprintf("/ws/voice-interview/%d", e.ID),
	}
}

// ===== 小工具 =====

func stringPtr(s string) *string                         { return &s }
func intPtr(i int) *int                                  { return &i }
func boolPtr(b bool) *bool                               { return &b }
func phasePtr(p VoicePhase) *VoicePhase                  { return &p }
func statusPtr(s VoiceSessionStatus) *VoiceSessionStatus { return &s }
func evalPtr(s VoiceEvaluateStatus) *VoiceEvaluateStatus { return &s }

func boolValue(b *bool) bool { return b != nil && *b }

func boolDefault(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func intValue(i *int) int {
	if i == nil {
		return 0
	}
	return *i
}

func statusValue(s *VoiceSessionStatus) string {
	if s == nil {
		return ""
	}
	return string(*s)
}

// statusEnum 守卫用的枚举读取（nil 按空枚举处理，守卫会以 400 拒绝）。
func statusEnum(s *VoiceSessionStatus) VoiceSessionStatus {
	if s == nil {
		return ""
	}
	return *s
}

func phaseValue(p *VoicePhase) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

func phaseNamePtr(p *VoicePhase) *string {
	if p == nil {
		return nil
	}
	s := string(*p)
	return &s
}

func evalStringPtr(s *VoiceEvaluateStatus) *string {
	if s == nil {
		return nil
	}
	v := string(*s)
	return &v
}

// parseSessionStatus 状态过滤值解析（大小写归一由调用方完成）。
func parseSessionStatus(s string) (VoiceSessionStatus, bool) {
	switch VoiceSessionStatus(s) {
	case VoiceStatusInProgress, VoiceStatusPaused, VoiceStatusCompleted, VoiceStatusFailed:
		return VoiceSessionStatus(s), true
	default:
		return "", false
	}
}

// parsePhase 阶段字符串解析（WS start_phase 与 REST 路径共用）。
func parsePhase(s string) (VoicePhase, bool) {
	switch VoicePhase(s) {
	case PhaseIntro, PhaseTech, PhaseProject, PhaseHR, PhaseCompleted:
		return VoicePhase(s), true
	default:
		return "", false
	}
}
