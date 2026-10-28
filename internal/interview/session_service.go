// SessionService 面试场次生命周期：创建（幂等三层 + 未完成复用 + 兜底题）、
// 查询/列表/未完成查询/删除。流程与 Java InterviewSessionService（笔记 05）逐字对齐；
// 出题本票为静态兜底题，真实出题在票 07。
package interview

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

// EvaluateEnqueuer 评估入队扩展点：末题自动交卷与提前交卷后触发（对齐 Java
// enqueueEvaluationTask = 置 evaluate_status=PENDING + 发送评估任务）。真实现由票 06
// 提供（Redis Stream XADD），本票注入 nil → 空操作。sessionID 为 interview_sessions
// 内部自增主键（作答表外键同源）。
type EvaluateEnqueuer interface {
	EnqueueEvaluation(ctx context.Context, sessionID int64) error
}

// SessionService 场次服务。
type SessionService struct {
	repo     *Repo
	cache    *SessionCache
	skills   *Service // 方向注册表（创建校验方向存在 + 兜底题取分类）
	cfg      Config
	logger   *slog.Logger
	enqueuer EvaluateEnqueuer // 评估入队扩展点，票 06 注入；nil 时为空操作
}

// NewSessionService 创建场次服务。enqueuer 传 nil 表示评估入队未接入（票 04 阶段）。
func NewSessionService(repo *Repo, cache *SessionCache, skills *Service, cfg Config, logger *slog.Logger, enqueuer EvaluateEnqueuer) *SessionService {
	return &SessionService{repo: repo, cache: cache, skills: skills, cfg: cfg, logger: logger, enqueuer: enqueuer}
}

// CreateSession POST /api/interview/sessions 入口：
// requestId 归一化（空 → 无锁无幂等直创；格式非法 → 400）；
// 非空走创建锁（等待 185s / 持有 600s）包裹的幂等创建。
func (s *SessionService) CreateSession(ctx context.Context, req CreateRequest) (*SessionDTO, error) {
	requestID, err := NormalizeRequestID(req.RequestID)
	if err != nil {
		return nil, err
	}
	if requestID == nil {
		// 对齐 Java：无 requestId 完全跳过幂等，直接创建。
		return s.createSessionInternal(ctx, req, nil)
	}

	var dto *SessionDTO
	err = s.cache.WithLock(ctx, createLockPrefix+*requestID, s.cfg.CreateLockWait, s.cfg.CreateLockLease,
		func() error {
			dto, err = s.createIdempotentSession(ctx, req, *requestID)
			return err
		})
	if err != nil {
		return nil, err
	}
	return dto, nil
}

// createIdempotentSession 幂等创建：结果键 → DB（requestId 唯一约束兜底）→ 创建，
// 顺序对齐 Java createIdempotentSession。
func (s *SessionService) createIdempotentSession(ctx context.Context, req CreateRequest, requestID string) (*SessionDTO, error) {
	cachedSessionID, err := s.cache.GetCreateResult(ctx, requestID)
	if err != nil {
		return nil, apperr.Internal("读取幂等结果失败")
	}
	if cachedSessionID != "" {
		s.logger.Info("复用缓存中的幂等创建请求", slog.String("requestId", requestID), slog.String("sessionId", cachedSessionID))
		return s.GetSession(ctx, cachedSessionID)
	}

	existing, err := s.repo.FindByRequestID(ctx, requestID)
	if err != nil && !IsNotFound(err) {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询幂等会话失败", err)
	}
	if existing != nil && existing.SessionID != "" {
		s.logger.Info("从数据库恢复幂等创建请求",
			slog.String("requestId", requestID), slog.String("sessionId", existing.SessionID))
		if err := s.cache.SetCreateResult(ctx, requestID, existing.SessionID); err != nil {
			s.logger.Warn("写幂等结果键失败", slog.String("error", err.Error()))
		}
		return s.GetSession(ctx, existing.SessionID)
	}

	created, err := s.createSessionInternal(ctx, req, &requestID)
	if err != nil {
		return nil, err
	}
	if err := s.cache.SetCreateResult(ctx, requestID, created.SessionID); err != nil {
		s.logger.Warn("写幂等结果键失败", slog.String("error", err.Error()))
	}
	return created, nil
}

// createSessionInternal 内部创建，逐字对齐 Java createSessionInternal：
// 题量校验（裁决 #1）→ 未完成复用（forceCreate 绕过）→ 默认值 → 兜底题 → 落库 + 缓存双写。
func (s *SessionService) createSessionInternal(ctx context.Context, req CreateRequest, requestID *string) (*SessionDTO, error) {
	// 裁决 #1：Java 缺 @Valid 未生效，Go 必须校验；消息沿用 Java 校验注解文案。
	if req.QuestionCount < s.cfg.QuestionCountMin {
		return nil, apperr.BadRequest("题目数量最少3题")
	}
	if req.QuestionCount > s.cfg.QuestionCountMax {
		return nil, apperr.BadRequest("题目数量最多20题")
	}

	// 有简历且未强制创建时复用未完成旧场次（裁决 #12：静默返回，响应与新建不可区分）。
	if shouldReuseUnfinished(req.ResumeID, req.ForceCreate) {
		unfinished, err := s.FindUnfinishedSession(ctx, *req.ResumeID)
		if err != nil {
			return nil, err
		}
		if unfinished != nil {
			s.logger.Info("检测到未完成的面试会话，返回现有会话",
				slog.Int64("resumeId", *req.ResumeID), slog.String("sessionId", unfinished.SessionID))
			return unfinished, nil
		}
	}

	// 方向存在性校验（票 01 依赖）：默认 java-backend；不存在按票 01 约定返回 3101。
	skillID := DefaultSkillID
	if req.SkillID != nil {
		skillID = *req.SkillID
	}
	categories, ok := s.skills.Categories(skillID)
	if !ok {
		return nil, apperr.SkillNotFound(skillID)
	}
	difficulty := DefaultDifficulty
	if req.Difficulty != nil {
		difficulty = *req.Difficulty
	}

	sessionID := NewSessionID()
	questions := GenerateFallbackQuestions(categories, req.QuestionCount, ClampFollowUpCount(s.cfg.FollowUpCount))
	questionsJSON, err := SerializeQuestions(questions)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "序列化问题列表失败", err)
	}

	// 落库默认值对齐 Java persistence 层：llmProvider null→"default"、sourceType null→"NORMAL"。
	llmProvider := "default"
	if req.LLMProvider != nil {
		llmProvider = *req.LLMProvider
	}
	status := StatusCreated
	now := time.Now()

	// 对齐 Java resumeOpt.ifPresent：简历不存在时静默不关联（避免 FK 违例，语义同 JPA）。
	resumeID := req.ResumeID
	if resumeID != nil {
		exists, err := s.repo.ResumeExists(ctx, *resumeID)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternalError, "查询简历失败", err)
		}
		if !exists {
			resumeID = nil
		}
	}

	entity := &SessionEntity{
		SessionID:      sessionID,
		RequestID:      requestID,
		SkillID:        stringPtr(skillID),
		Difficulty:     stringPtr(difficulty),
		ResumeID:       resumeID,
		TotalQuestions: intPtr(len(questions)),
		Status:         &status,
		QuestionsJSON:  stringPtr(questionsJSON),
		LLMProvider:    stringPtr(llmProvider),
		SourceType:     sourcePtr(SourceNormal),
		CreatedAt:      now,
	}
	if err := s.repo.CreateWithTx(ctx, nil, entity); err != nil {
		if requestID != nil {
			// 并发兜底：唯一约束冲突时另一请求已落库，直接返回那一场次。
			if concurrent, qErr := s.repo.FindByRequestID(ctx, *requestID); qErr == nil && concurrent != nil {
				return s.GetSession(ctx, concurrent.SessionID)
			}
			return nil, apperr.Wrap(apperr.CodeInternalError, "创建面试会话失败，请重试", err)
		}
		// 无幂等键：DB 失败仅告警不阻断（对齐 Java warn-and-continue）。
		s.logger.Warn("保存面试会话到数据库失败", slog.String("error", err.Error()))
	}

	// 缓存双写：幂等请求先成功落库再写缓存，进程异常后可从 DB 恢复。
	// 缓存写失败按尽力而为处理（读路径 DB 兜底可重建），不回滚已落库数据。
	resumeText := ""
	if req.ResumeText != nil {
		resumeText = *req.ResumeText
	}
	if err := s.cache.SaveSession(ctx, CachedSession{
		SessionID:     sessionID,
		ResumeText:    resumeText,
		ResumeID:      resumeID,
		QuestionsJSON: questionsJSON,
		CurrentIndex:  0,
		Status:        StatusCreated,
	}); err != nil {
		s.logger.Warn("写会话缓存失败（读路径将从 DB 恢复）", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
	}

	return &SessionDTO{
		SessionID:      sessionID,
		ResumeText:     resumeText,
		TotalQuestions: len(questions),
		CurrentIndex:   0,
		Questions:      questions,
		Status:         StatusCreated,
	}, nil
}

// GetSession 读路径缓存优先、DB 兜底（对齐 Java getSession：详情用，不刷新 TTL 语义之外的副作用）。
func (s *SessionService) GetSession(ctx context.Context, sessionID string) (*SessionDTO, error) {
	cached, err := s.cache.GetSession(ctx, sessionID)
	if err != nil {
		s.logger.Warn("读会话缓存失败，回源 DB", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
	}
	if cached != nil {
		return cachedToDTO(cached), nil
	}

	entity, err := s.repo.FindBySessionID(ctx, sessionID)
	if IsNotFound(err) {
		return nil, apperr.InterviewSessionNotFound()
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询面试会话失败", err)
	}
	restored, err := s.restoreAndCache(ctx, entity)
	if err != nil {
		return nil, err
	}
	return cachedToDTO(restored), nil
}

// getOrRestoreSession 问答链路读路径（对齐 Java getOrRestoreSession）：缓存命中刷新 TTL；
// 未命中从 DB 恢复并重建缓存；都不存在 → 3001。与详情路径 GetSession 的差别仅在 TTL 刷新。
func (s *SessionService) getOrRestoreSession(ctx context.Context, sessionID string) (*CachedSession, error) {
	cached, err := s.cache.GetSession(ctx, sessionID)
	if err != nil {
		s.logger.Warn("读会话缓存失败，回源 DB", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
	}
	if cached != nil {
		if err := s.cache.RefreshTTL(ctx, sessionID); err != nil {
			s.logger.Warn("刷新会话缓存 TTL 失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
		}
		return cached, nil
	}

	entity, err := s.repo.FindBySessionID(ctx, sessionID)
	if IsNotFound(err) {
		return nil, apperr.InterviewSessionNotFound()
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询面试会话失败", err)
	}
	return s.restoreAndCache(ctx, entity)
}

// FindUnfinishedSession 未完成场次：缓存映射 → DB 兜底（对齐 Java findUnfinishedSession；
// 内部异常一律按无未完成处理）。找不到返回 (nil, nil)，端点层转 3001 业务错误。
func (s *SessionService) FindUnfinishedSession(ctx context.Context, resumeID int64) (*SessionDTO, error) {
	cachedID, err := s.cache.FindUnfinishedSessionID(ctx, resumeID)
	if err != nil {
		s.logger.Warn("查未完成映射失败，回源 DB", slog.Int64("resumeId", resumeID), slog.String("error", err.Error()))
	}
	if cachedID != nil {
		cached, err := s.cache.GetSession(ctx, *cachedID)
		if err == nil && cached != nil {
			return cachedToDTO(cached), nil
		}
	}

	entity, err := s.repo.FindUnfinishedByResumeID(ctx, resumeID)
	if IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, nil // 对齐 Java：恢复未完成会话的任何异常按 Optional.empty()
	}
	restored, err := s.restoreAndCache(ctx, entity)
	if err != nil {
		return nil, nil
	}
	return cachedToDTO(restored), nil
}

// FindUnfinishedOrThrow GET /sessions/unfinished/{resumeId}：无记录 → 3001
// "未找到未完成的面试会话"（裁决 #3：业务错误，data=null）。
func (s *SessionService) FindUnfinishedOrThrow(ctx context.Context, resumeID int64) (*SessionDTO, error) {
	dto, err := s.FindUnfinishedSession(ctx, resumeID)
	if err != nil {
		return nil, err
	}
	if dto == nil {
		return nil, apperr.InterviewSessionNotFoundWithMessage("未找到未完成的面试会话")
	}
	return dto, nil
}

// ListSessions GET /sessions：全部场次按 created_at desc。
func (s *SessionService) ListSessions(ctx context.Context) ([]SessionListItemDTO, error) {
	list, err := s.repo.FindAll(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询面试会话列表失败", err)
	}
	items := make([]SessionListItemDTO, 0, len(list))
	for i := range list {
		items = append(items, entityToListItem(&list[i]))
	}
	return items, nil
}

// DeleteSession DELETE /sessions/{id}：级联删作答、不存在报 3001；
// 不清理 Redis 缓存与幂等结果键（裁决 #5 照抄 Java）。
func (s *SessionService) DeleteSession(ctx context.Context, sessionID string) error {
	err := s.repo.DeleteBySessionID(ctx, sessionID)
	if IsNotFound(err) {
		return apperr.InterviewSessionNotFound()
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternalError, "删除面试会话失败", err)
	}
	s.logger.Info("已删除面试会话", slog.String("sessionId", sessionID))
	return nil
}

// ===== 逐题问答闭环（票 04，对齐 Java getCurrentQuestion / saveAnswer / submitAnswer /
// completeInterview，笔记 05 §4-§7）=====

// GetCurrentQuestion GET /sessions/{id}/question：按 currentIndex 返回当前题；
// 首次调用把状态 CREATED→IN_PROGRESS（缓存 + DB 双写，DB 失败仅告警）；
// currentIndex 越过末题返回 completed 分支（message 文案与 Java 逐字一致）。
// 注意顺序对齐 Java：先判 completed 分支，再做 CREATED 状态推进。
func (s *SessionService) GetCurrentQuestion(ctx context.Context, sessionID string) (*CurrentQuestionResponse, error) {
	session, err := s.getOrRestoreSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	questions, err := parseSessionQuestions(session.QuestionsJSON)
	if err != nil {
		return nil, err
	}

	if session.CurrentIndex >= len(questions) {
		return &CurrentQuestionResponse{
			Completed: true,
			Message:   stringPtr("所有问题已回答完毕"),
		}, nil
	}

	if next, changed := questionViewTransition(session.Status); changed {
		session.Status = next
		if err := s.cache.UpdateSessionStatus(ctx, sessionID, next); err != nil {
			s.logger.Warn("更新会话缓存状态失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
		}
		// DB 同步失败仅告警（对齐 Java try/warn）。
		if err := s.repo.UpdateStatus(ctx, sessionID, next); err != nil {
			s.logger.Warn("更新会话状态失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
		}
	}

	return &CurrentQuestionResponse{Completed: false, Question: &questions[session.CurrentIndex]}, nil
}

// SaveAnswer PUT /sessions/{id}/answers 暂存：作答回写 questions_json（缓存）+ 作答表
// upsert（DB），不动 currentIndex。无状态守卫（笔记 05 意外行为 #6：COMPLETED/EVALUATED
// 场次暂存照常执行，DB 状态被拉回 IN_PROGRESS；缓存状态仅在 CREATED 时推进）。
// DB 保存失败仅告警——暂存尽力而为（对齐 Java try/warn）。
func (s *SessionService) SaveAnswer(ctx context.Context, sessionID string, req AnswerRequest) error {
	index, err := answerIndexOf(req)
	if err != nil {
		return err
	}
	session, err := s.getOrRestoreSession(ctx, sessionID)
	if err != nil {
		return err
	}
	questions, err := parseSessionQuestions(session.QuestionsJSON)
	if err != nil {
		return err
	}
	if err := validateAnswerIndex(index, len(questions)); err != nil {
		return err
	}

	questions[index].UserAnswer = req.Answer

	// 缓存尽力而为：题目回写 + CREATED 状态推进（失败可由读路径从 DB 恢复）。
	if err := s.cache.UpdateQuestions(ctx, sessionID, questions); err != nil {
		s.logger.Warn("暂存答案写缓存失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
	}
	if _, changed := questionViewTransition(session.Status); changed {
		if err := s.cache.UpdateSessionStatus(ctx, sessionID, StatusInProgress); err != nil {
			s.logger.Warn("更新会话缓存状态失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
		}
	}

	// DB 尽力而为（对齐 Java try/warn）：作答 upsert + 状态强制 IN_PROGRESS。
	if err := s.saveAnswerRecord(ctx, sessionID, index, &questions[index], req.Answer); err != nil {
		s.logger.Warn("暂存答案到数据库失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
	} else if err := s.repo.UpdateStatus(ctx, sessionID, StatusInProgress); err != nil {
		s.logger.Warn("更新会话状态失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
	}

	s.logger.Info("会话暂存答案", slog.String("sessionId", sessionID), slog.Int("questionIndex", index))
	return nil
}

// SubmitAnswer POST /sessions/{id}/answers 提交：作答更新并回写 questions_json，
// currentIndex = questionIndex+1（裁决 #4：不校验 index==currentIndex，可乱序提交）；
// 末题提交自动交卷（状态 COMPLETED + completedAt）并触发评估入队扩展点。
// DB 持久化失败 → 3008 "保存答案失败，请稍后重试"（对齐 Java persistSubmittedAnswer）。
func (s *SessionService) SubmitAnswer(ctx context.Context, sessionID string, req AnswerRequest) (*SubmitAnswerResponse, error) {
	index, err := answerIndexOf(req)
	if err != nil {
		return nil, err
	}
	session, err := s.getOrRestoreSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	questions, err := parseSessionQuestions(session.QuestionsJSON)
	if err != nil {
		return nil, err
	}
	if err := validateAnswerIndex(index, len(questions)); err != nil {
		return nil, err
	}

	question := questions[index]
	question.UserAnswer = req.Answer
	questions[index] = question

	hasNext, next, newIndex, newStatus := submitOutcome(questions, index)

	rowID, err := s.persistSubmittedAnswer(ctx, sessionID, index, &question, req.Answer, newIndex, newStatus)
	if err != nil {
		return nil, err
	}

	// DB 已成功，缓存尽力而为更新，失败可由后续读取从数据库恢复（对齐 Java 注释语义）。
	if err := s.cache.UpdateQuestions(ctx, sessionID, questions); err != nil {
		s.logger.Warn("提交答案写缓存失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
	}
	if err := s.cache.UpdateCurrentIndex(ctx, sessionID, newIndex); err != nil {
		s.logger.Warn("更新缓存进度失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
	}
	if newStatus == StatusCompleted {
		if err := s.cache.UpdateSessionStatus(ctx, sessionID, StatusCompleted); err != nil {
			s.logger.Warn("更新会话缓存状态失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
		}
		// 末题自动交卷：评估入队扩展点（票 06 真实现）。入队失败对齐 Java 未捕获异常 → 500。
		if err := s.enqueueEvaluation(ctx, sessionID, rowID); err != nil {
			return nil, err
		}
	}

	s.logger.Info("会话提交答案",
		slog.String("sessionId", sessionID), slog.Int("questionIndex", index), slog.Int("remaining", len(questions)-newIndex))

	return &SubmitAnswerResponse{
		HasNextQuestion: hasNext,
		NextQuestion:    next,
		CurrentIndex:    newIndex,
		TotalQuestions:  len(questions),
	}, nil
}

// persistSubmittedAnswer 提交的 DB 持久化（对齐 Java persistSubmittedAnswer）：
// 作答 upsert → 进度推进 → 状态推进；场次行不存在 → 3001（BusinessException 透传语义），
// 其余失败 → 3008。返回场次内部 ID 供评估入队使用。
func (s *SessionService) persistSubmittedAnswer(ctx context.Context, sessionID string, index int, question *QuestionDTO, answer *string, newIndex int, newStatus SessionStatus) (int64, error) {
	entity, err := s.repo.FindBySessionID(ctx, sessionID)
	if IsNotFound(err) {
		return 0, apperr.InterviewSessionNotFound()
	}
	if err != nil {
		return 0, answerSaveFailed(err)
	}
	if err := s.repo.UpsertAnswer(ctx, entity.ID, index, question.Question, question.Category, answer); err != nil {
		return 0, answerSaveFailed(err)
	}
	if err := s.repo.UpdateCurrentQuestionIndex(ctx, sessionID, newIndex); err != nil {
		return 0, answerSaveFailed(err)
	}
	if err := s.repo.UpdateStatus(ctx, sessionID, newStatus); err != nil {
		return 0, answerSaveFailed(err)
	}
	return entity.ID, nil
}

// CompleteSession POST /sessions/{id}/complete 提前交卷：仅未完成状态（CREATED/
// IN_PROGRESS）允许，COMPLETED/EVALUATED → 3004 "面试已完成"。不设置 currentIndex
// （未答题 userAnswer 保持空，评估按 0 分处理，对齐 Java completeInterview）。
func (s *SessionService) CompleteSession(ctx context.Context, sessionID string) error {
	session, err := s.getOrRestoreSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if !completeStartAllowed(session.Status) {
		return apperr.New(apperr.CodeInterviewAlreadyCompleted, "面试已完成")
	}

	if err := s.cache.UpdateSessionStatus(ctx, sessionID, StatusCompleted); err != nil {
		s.logger.Warn("更新会话缓存状态失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
	}
	// DB 状态失败仅告警（对齐 Java try/warn）；评估置 PENDING 属入队扩展点职责（票 06）。
	if err := s.repo.UpdateStatus(ctx, sessionID, StatusCompleted); err != nil {
		s.logger.Warn("更新会话状态失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
	}

	entity, err := s.repo.FindBySessionID(ctx, sessionID)
	if err != nil {
		// 无 DB 记录的场次无法定位评估任务，告警后跳过入队（状态已推进，对齐 Java warn-and-continue）。
		s.logger.Warn("提前交卷查场次失败，跳过评估入队", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
		return nil
	}
	if err := s.enqueueEvaluation(ctx, sessionID, entity.ID); err != nil {
		return err
	}

	s.logger.Info("会话提前交卷，评估任务已入队", slog.String("sessionId", sessionID))
	return nil
}

// enqueueEvaluation 评估入队扩展点调用。未注入（票 04 阶段）为空操作；
// 注入后失败向上传播（对齐 Java sendEvaluateTask 未捕获 → 500）。
func (s *SessionService) enqueueEvaluation(ctx context.Context, sessionID string, rowID int64) error {
	if s.enqueuer == nil {
		return nil
	}
	if err := s.enqueuer.EnqueueEvaluation(ctx, rowID); err != nil {
		return apperr.Wrap(apperr.CodeInternalError, "评估任务入队失败", err)
	}
	return nil
}

// saveAnswerRecord 暂存的作答表 upsert：场次行不存在 → 3001（对齐 Java saveAnswer
// 抛 BusinessException，在 PUT 的 try/warn 中被吞掉、在 POST 中透传）。
func (s *SessionService) saveAnswerRecord(ctx context.Context, sessionID string, index int, question *QuestionDTO, answer *string) error {
	entity, err := s.repo.FindBySessionID(ctx, sessionID)
	if IsNotFound(err) {
		return apperr.InterviewSessionNotFound()
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternalError, "保存答案失败", err)
	}
	return s.repo.UpsertAnswer(ctx, entity.ID, index, question.Question, question.Category, answer)
}

// ===== 问答闭环纯逻辑（表驱动单测覆盖，笔记 05/08 逐字契约）=====

// questionViewTransition 取题触发的状态推进：仅 CREATED → IN_PROGRESS（首次取题）；
// 其余状态原样返回不推进。
func questionViewTransition(status SessionStatus) (SessionStatus, bool) {
	if status == StatusCreated {
		return StatusInProgress, true
	}
	return status, false
}

// submitOutcome 提交答案的末题判定与推进（裁决 #4：不校验 index==currentIndex，
// newIndex = index+1 直接覆盖——乱序提交会跳题）：非末题 IN_PROGRESS，末题 COMPLETED。
func submitOutcome(questions []QuestionDTO, index int) (hasNext bool, next *QuestionDTO, newIndex int, newStatus SessionStatus) {
	newIndex = index + 1
	hasNext = newIndex < len(questions)
	if hasNext {
		next = &questions[newIndex]
		newStatus = StatusInProgress
	} else {
		newStatus = StatusCompleted
	}
	return hasNext, next, newIndex, newStatus
}

// validateAnswerIndex 索引越界 → 3003 "无效的问题索引: N"（对齐 Java
// saveAnswer/submitAnswer 的统一守卫）。
func validateAnswerIndex(index, total int) error {
	if index < 0 || index >= total {
		return apperr.New(apperr.CodeInterviewQuestionNotFound, "无效的问题索引: "+strconv.Itoa(index))
	}
	return nil
}

// completeStartAllowed 提前交卷允许的起始状态：CREATED / IN_PROGRESS（笔记 05 §7 逐字）；
// COMPLETED / EVALUATED → 3004。
func completeStartAllowed(status SessionStatus) bool {
	return IsUnfinished(status)
}

// answerIndexOf 请求体 questionIndex 显式校验：缺失 → 400（Java 为 NPE → 500，
// 笔记 08 §2.5 建议 Go 显式校验，属有意偏离）。
func answerIndexOf(req AnswerRequest) (int, error) {
	if req.QuestionIndex == nil {
		return 0, apperr.BadRequest("questionIndex 不能为空")
	}
	return *req.QuestionIndex, nil
}

// parseSessionQuestions 场次 questions_json → 题目列表；空内容按空场处理
// （取题将命中 completed 分支）。
func parseSessionQuestions(raw string) ([]QuestionDTO, error) {
	if strings.TrimSpace(raw) == "" {
		return []QuestionDTO{}, nil
	}
	questions, err := DeserializeQuestions(raw)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "反序列化问题列表失败", err)
	}
	return questions, nil
}

// answerSaveFailed 提交持久化失败 → 3008（消息与 Java persistSubmittedAnswer 逐字一致）。
func answerSaveFailed(cause error) *apperr.AppError {
	return apperr.New(apperr.CodeInterviewAnswerSaveFailed, "保存答案失败，请稍后重试")
}

// restoreAndCache 从 DB 恢复会话并重建缓存（对齐 Java restoreSessionFromEntity：
// questions_json 反序列化 → 作答表按题号覆盖 userAnswer → 写回缓存）。
// 返回缓存结构供 getOrRestoreSession 等问答链路直接消费。
func (s *SessionService) restoreAndCache(ctx context.Context, entity *SessionEntity) (*CachedSession, error) {
	questions := []QuestionDTO{}
	if entity.QuestionsJSON != nil && *entity.QuestionsJSON != "" {
		parsed, err := DeserializeQuestions(*entity.QuestionsJSON)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternalError, "反序列化问题列表失败", err)
		}
		questions = parsed
	}

	answers, err := s.repo.AnswersBySessionID(ctx, entity.ID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询作答记录失败", err)
	}
	for _, a := range answers {
		if a.QuestionIndex == nil || a.UserAnswer == nil {
			continue
		}
		idx := *a.QuestionIndex
		if idx >= 0 && idx < len(questions) {
			questions[idx].UserAnswer = a.UserAnswer
		}
	}

	// 简历文本来自 DB 关联（不用请求参数），对齐 Java entity.getResume().getResumeText()。
	resumeText := ""
	if entity.ResumeID != nil {
		if text, err := s.repo.ResumeTextByID(ctx, *entity.ResumeID); err == nil && text != nil {
			resumeText = *text
		}
	}

	questionsJSON, err := SerializeQuestions(questions)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "序列化问题列表失败", err)
	}
	status := StatusCreated
	if entity.Status != nil {
		status = *entity.Status
	}
	currentIndex := 0
	if entity.CurrentQuestionIndex != nil {
		currentIndex = *entity.CurrentQuestionIndex
	}
	cached := CachedSession{
		SessionID:         entity.SessionID,
		ResumeText:        resumeText,
		ResumeID:          entity.ResumeID,
		KnowledgeBaseID:   entity.KnowledgeBaseID,
		InterviewCategory: entity.InterviewCategory,
		QuestionsJSON:     questionsJSON,
		CurrentIndex:      currentIndex,
		Status:            status,
	}
	if err := s.cache.SaveSession(ctx, cached); err != nil {
		s.logger.Warn("恢复会话写缓存失败", slog.String("sessionId", entity.SessionID), slog.String("error", err.Error()))
	}
	return &cached, nil
}

// cachedToDTO / entityToListItem：缓存与 DB 行到响应 DTO 的映射
// （列表字段一次建全：评估列票 06 前为 null/零值）。
func cachedToDTO(c *CachedSession) *SessionDTO {
	questions := []QuestionDTO{}
	if c.QuestionsJSON != "" {
		if parsed, err := DeserializeQuestions(c.QuestionsJSON); err == nil {
			questions = parsed
		}
	}
	return &SessionDTO{
		SessionID:         c.SessionID,
		ResumeText:        c.ResumeText,
		TotalQuestions:    len(questions),
		CurrentIndex:      c.CurrentIndex,
		Questions:         questions,
		Status:            c.Status,
		KnowledgeBaseID:   c.KnowledgeBaseID,
		InterviewCategory: c.InterviewCategory,
	}
}

func entityToListItem(e *SessionEntity) SessionListItemDTO {
	item := SessionListItemDTO{
		SessionID:         e.SessionID,
		SkillID:           e.SkillID,
		Difficulty:        e.Difficulty,
		ResumeID:          e.ResumeID,
		TotalQuestions:    e.TotalQuestions,
		KnowledgeBaseID:   e.KnowledgeBaseID,
		InterviewCategory: e.InterviewCategory,
		CreatedAt:         e.CreatedAt,
		CompletedAt:       e.CompletedAt,
	}
	if e.Status != nil {
		item.Status = stringPtr(string(*e.Status))
	}
	if e.EvaluateStatus != nil {
		item.EvaluateStatus = stringPtr(string(*e.EvaluateStatus))
	}
	item.EvaluateError = e.EvaluateError
	item.OverallScore = e.OverallScore
	if e.SourceType != nil {
		item.SourceType = stringPtr(string(*e.SourceType))
	}
	return item
}

// shouldReuseUnfinished 复用判定纯逻辑：有简历且未显式 forceCreate=true 时
// 复用未完成旧场次（对齐 Java `resumeId != null && !Boolean.TRUE.equals(forceCreate)`）。
func shouldReuseUnfinished(resumeID *int64, forceCreate *bool) bool {
	return resumeID != nil && !boolValue(forceCreate)
}

// boolValue / sourcePtr 小工具。
func boolValue(b *bool) bool { return b != nil && *b }

func sourcePtr(s SourceType) *SourceType { return &s }
