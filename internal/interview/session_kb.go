// 「从题目集合创建场次」入口（票 05，知识库面试复用统一场次落库）：
// 对齐 Java InterviewSessionService.createSessionFromQuestions（192-219 行逐字）——
// 题目为空 → 3003「面试题目不能为空」；DB 落 sourceType=KNOWLEDGE_BASE +
// knowledge_base_id + interview_category；Redis 缓存双写；返回 InterviewSessionDTO。
package interview

import (
	"context"
	"log/slog"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

// SessionFromQuestionsCreator 「从题目集合创建场次」跨模块接缝：知识库面试经此
// 入口落统一 interview_sessions（模块间显式小接口约定，voiceinterview 复用
// interview 接缝的同一先例）。生产实现 *SessionService。
type SessionFromQuestionsCreator interface {
	CreateSessionFromQuestions(ctx context.Context, questions []QuestionDTO,
		llmProvider, skillID, difficulty *string, knowledgeBaseID int64, interviewCategory *string) (*SessionDTO, error)
}

// CreateSessionFromQuestions 从已抽定的题目集合创建统一场次。
// 可空参数缺省值对齐 Java saveSessionInternal：llmProvider→"default"、
// skillID→java-backend、difficulty→mid；难度/方向由调用方（知识库面试服务）归一。
func (s *SessionService) CreateSessionFromQuestions(ctx context.Context, questions []QuestionDTO,
	llmProvider, skillID, difficulty *string, knowledgeBaseID int64, interviewCategory *string) (*SessionDTO, error) {
	if len(questions) == 0 {
		return nil, apperr.New(apperr.CodeInterviewQuestionNotFound, "面试题目不能为空")
	}

	sessionID := NewSessionID()
	questionsJSON, err := SerializeQuestions(questions)
	if err != nil {
		// 对齐 Java saveSessionInternal 的 JacksonException 分支文案。
		return nil, apperr.New(apperr.CodeInternalError, "保存会话失败")
	}
	entity := kbSessionEntity(sessionID, questionsJSON, len(questions), llmProvider, skillID, difficulty,
		knowledgeBaseID, interviewCategory, time.Now())
	if err := s.repo.CreateWithTx(ctx, nil, entity); err != nil {
		// 对齐 Java：落库异常向上传播，由全局兜底转 500「系统繁忙，请稍后重试」。
		return nil, apperr.Wrap(apperr.CodeInternalError, "系统繁忙，请稍后重试", err)
	}

	// 缓存双写尽力而为（读路径 DB 兜底可重建，与文字面试创建同一约定）。
	cached := CachedSession{
		SessionID:         sessionID,
		ResumeText:        "",
		KnowledgeBaseID:   entity.KnowledgeBaseID,
		InterviewCategory: interviewCategory,
		QuestionsJSON:     questionsJSON,
		CurrentIndex:      0,
		Status:            StatusCreated,
	}
	if err := s.cache.SaveSession(ctx, cached); err != nil {
		s.logger.Warn("写会话缓存失败（读路径将从 DB 恢复）",
			slog.String("sessionId", sessionID), slog.String("error", err.Error()))
	}

	s.logger.Info("知识库面试会话已创建",
		slog.String("sessionId", sessionID),
		slog.Int64("knowledgeBaseId", knowledgeBaseID),
		slog.Int("totalQuestions", len(questions)))

	return &SessionDTO{
		SessionID:            sessionID,
		ResumeText:           "",
		TotalQuestions:       len(questions),
		CurrentQuestionIndex: 0,
		Questions:            questions,
		Status:               StatusCreated,
		KnowledgeBaseID:      entity.KnowledgeBaseID,
		InterviewCategory:    interviewCategory,
	}, nil
}

// kbSessionEntity 知识库场次的实体组装（纯函数，可表驱动单测）：
// sourceType 恒 KNOWLEDGE_BASE；无 requestId/resumeId；其余列默认值与 Java
// saveSessionInternal 一致（llmProvider nil→"default"、skillID nil→默认方向、
// difficulty nil→mid，均只判 nil 不判空白，照抄 Java 三元表达式）。
func kbSessionEntity(sessionID, questionsJSON string, totalQuestions int, llmProvider, skillID, difficulty *string,
	knowledgeBaseID int64, interviewCategory *string, now time.Time) *SessionEntity {
	provider := "default"
	if llmProvider != nil {
		provider = *llmProvider
	}
	skill := DefaultSkillID
	if skillID != nil {
		skill = *skillID
	}
	diff := DefaultDifficulty
	if difficulty != nil {
		diff = *difficulty
	}
	status := StatusCreated
	return &SessionEntity{
		SessionID:         sessionID,
		RequestID:         nil,
		SkillID:           stringPtr(skill),
		Difficulty:        stringPtr(diff),
		ResumeID:          nil,
		TotalQuestions:    intPtr(totalQuestions),
		Status:            &status,
		QuestionsJSON:     stringPtr(questionsJSON),
		LLMProvider:       stringPtr(provider),
		SourceType:        sourcePtr(SourceKnowledgeBase),
		KnowledgeBaseID:   int64Ptr(knowledgeBaseID),
		InterviewCategory: interviewCategory,
		CreatedAt:         now,
	}
}

func int64Ptr(v int64) *int64 { return &v }
