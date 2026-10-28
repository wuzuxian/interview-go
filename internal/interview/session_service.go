// SessionService 面试场次生命周期：创建（幂等三层 + 未完成复用 + 真实出题 + 兜底降级）、
// 查询/列表/未完成查询/删除。流程与 Java InterviewSessionService（笔记 05）逐字对齐；
// 出题来源为票 07 的真实 LLM 生成（双路并行），静态兜底题保留为最终降级路径。
package interview

import (
	"context"
	"log/slog"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

// SessionService 场次服务。
type SessionService struct {
	repo      *Repo
	cache     *SessionCache
	skills    *Service         // 方向注册表（创建校验方向存在）
	questions *QuestionService // 真实出题（票 07：双路并行 + 兜底降级）
	cfg       Config
	logger    *slog.Logger
}

// NewSessionService 创建场次服务。
func NewSessionService(repo *Repo, cache *SessionCache, skills *Service, questions *QuestionService, cfg Config, logger *slog.Logger) *SessionService {
	return &SessionService{repo: repo, cache: cache, skills: skills, questions: questions, cfg: cfg, logger: logger}
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
	// custom + customCategories 走临时构建（票 07），预设校验放行（对齐 Java resolveSkill 分支）。
	skillID := DefaultSkillID
	if req.SkillID != nil {
		skillID = *req.SkillID
	}
	customMode := skillID == CustomSkillID && len(req.CustomCategories) > 0
	if !customMode {
		if _, ok := s.skills.Categories(skillID); !ok {
			return nil, apperr.SkillNotFound(skillID)
		}
	}
	difficulty := DefaultDifficulty
	if req.Difficulty != nil {
		difficulty = *req.Difficulty
	}

	sessionID := NewSessionID()

	// 历史去重输入（对齐 Java createSessionInternal：generateQuestionsBySkill 之前查询，
	// 通用模式按 skillId 查询，有简历时按 resumeId + skillId 精确匹配）。
	historical, err := s.repo.GetHistoricalQuestions(ctx, skillID, req.ResumeID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "加载历史题目失败", err)
	}
	s.logger.Info("加载历史题目",
		slog.String("skillId", skillID),
		slog.Int64("resumeId", intValue64(req.ResumeID)),
		slog.Int("historical", len(historical)))

	// 真实 LLM 出题（票 07；兜底题在生成器内部作为最终降级路径）。
	questions, err := s.questions.GenerateQuestionsBySkill(ctx, QuestionGenParams{
		LLMProvider:      req.LLMProvider,
		SkillID:          skillID,
		Difficulty:       difficulty,
		ResumeText:       req.ResumeText,
		QuestionCount:    req.QuestionCount,
		Historical:       historical,
		CustomCategories: req.CustomCategories,
		JDText:           req.JDText,
	})
	if err != nil {
		return nil, err
	}
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

// GetSession 读路径缓存优先、DB 兜底（对齐 Java getSession）。
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
	dto, err := s.restoreAndCache(ctx, entity)
	if err != nil {
		return nil, nil
	}
	return dto, nil
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

// restoreAndCache 从 DB 恢复会话并重建缓存（对齐 Java restoreSessionFromEntity：
// questions_json 反序列化 → 作答表按题号覆盖 userAnswer → 写回缓存）。
func (s *SessionService) restoreAndCache(ctx context.Context, entity *SessionEntity) (*SessionDTO, error) {
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
	return cachedToDTO(&cached), nil
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

func intValue64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
