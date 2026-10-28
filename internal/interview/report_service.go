// 报告/明细节拍（票 06）：GET /sessions/{id}/report 与 GET /sessions/{id}/details。
// report 三分支按裁决 #2：已评估（evaluate_status=COMPLETED 或场次 EVALUATED）直接
// 返回存量报告（不重调 LLM）；场次 COMPLETED 但未评估走同步兜底评估（generateReport
// 语义，笔记 05 §8）；其余状态业务错误（笔记 08 §2.6）。details 组装按笔记 07 逐字。
package interview

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"

	"github.com/wuzuxian/interview/internal/apperr"
)

// SessionStatusUpdater 同步兜底路径的缓存状态推进（对齐 Java generateReport
// 先推缓存 EVALUATED；生产实现 *SessionCache）。
type SessionStatusUpdater interface {
	UpdateSessionStatus(ctx context.Context, sessionID string, status SessionStatus) error
}

// ReportService 报告/明细端点服务。
type ReportService struct {
	store  EvalStore
	eval   EvaluationRunner
	refs   ReferenceSource
	status SessionStatusUpdater
	logger *slog.Logger
}

// NewReportService 创建报告服务。status/refs 可为 nil。
func NewReportService(store EvalStore, eval EvaluationRunner, refs ReferenceSource,
	status SessionStatusUpdater, logger *slog.Logger) *ReportService {
	return &ReportService{store: store, eval: eval, refs: refs, status: status, logger: logger}
}

// GetReport GET /sessions/{id}/report（裁决 #2）：
//   - evaluate_status=COMPLETED 或场次 EVALUATED → 存量报告（组装 DB 列，不重调 LLM）；
//   - 场次 COMPLETED 但未评估 → 同步兜底评估（同步跑引擎、落库、置 EVALUATED）；
//   - 其余状态 → 3007 "面试尚未完成，无法生成报告"；场次不存在 → 3001。
func (s *ReportService) GetReport(ctx context.Context, sessionID string) (*ReportDTO, error) {
	entity, err := s.store.FindBySessionID(ctx, sessionID)
	if IsNotFound(err) {
		return nil, apperr.InterviewSessionNotFound()
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询面试会话失败", err)
	}

	status := StatusCreated
	if entity.Status != nil {
		status = *entity.Status
	}
	evaluated := (entity.EvaluateStatus != nil && *entity.EvaluateStatus == EvaluateCompleted) ||
		status == StatusEvaluated
	if evaluated {
		questions, err := replayQuestions(ctx, s.store, entity)
		if err != nil {
			return nil, err
		}
		return s.storedReport(ctx, entity, questions), nil
	}
	if status != StatusCompleted {
		return nil, apperr.New(apperr.CodeInterviewNotCompleted, "面试尚未完成，无法生成报告")
	}
	return s.generateReportSync(ctx, entity)
}

// generateReportSync 同步兜底评估（对齐 Java generateReport，笔记 05 §8）：
// 同步跑引擎 → 缓存状态置 EVALUATED → saveReport 落库（保存失败仅告警）。
func (s *ReportService) generateReportSync(ctx context.Context, entity *SessionEntity) (*ReportDTO, error) {
	s.logger.Info("生成面试报告", slog.String("sessionId", entity.SessionID))

	questions, err := replayQuestions(ctx, s.store, entity)
	if err != nil {
		return nil, err
	}
	skillReference := ""
	if s.refs != nil {
		skillReference = s.refs.BuildEvaluationReferenceSectionSafe(evalSkillID(entity))
	}
	report, err := s.eval.Evaluate(ctx, EvaluateInput{
		SessionID:      entity.SessionID,
		ResumeText:     resumeTextOf(ctx, s.store, entity),
		Questions:      questions,
		SkillReference: skillReference,
	}, entity.LLMProvider)
	if err != nil {
		return nil, err
	}

	if s.status != nil {
		if err := s.status.UpdateSessionStatus(ctx, entity.SessionID, StatusEvaluated); err != nil {
			s.logger.Warn("更新会话缓存状态失败", slog.String("sessionId", entity.SessionID), slog.String("error", err.Error()))
		}
	}
	if err := saveReport(ctx, s.store, s.logger, entity.SessionID, report); err != nil {
		s.logger.Warn("保存报告到数据库失败", slog.String("sessionId", entity.SessionID), slog.String("error", err.Error()))
	}
	return report, nil
}

// storedReport 存量报告组装（裁决 #2 的 Go 侧定义）：总分/总评语/优势/改进/参考答案
// 取 interview_sessions 各 JSON 列，逐题取 questions_json + interview_answers 回填列，
// 分类得分按票 05 计分语义重算（分类平均 int 截断 + 题数，按分类名稳定排序）。
func (s *ReportService) storedReport(ctx context.Context, entity *SessionEntity, questions []QuestionDTO) *ReportDTO {
	answers, err := s.store.AnswersBySessionID(ctx, entity.ID)
	if err != nil {
		s.logger.Warn("查询作答记录失败", slog.String("sessionId", entity.SessionID), slog.String("error", err.Error()))
		answers = nil
	}
	return buildStoredReport(entity, questions, answers)
}

// GetInterviewDetail GET /sessions/{id}/details（笔记 07 逐字）：只读 DB，
// interview_sessions JSON 列 + interview_answers 组装；未答题生成空记录；
// answers 按题号排序。场次不存在 → 3001。
func (s *ReportService) GetInterviewDetail(ctx context.Context, sessionID string) (*InterviewDetailDTO, error) {
	entity, err := s.store.FindBySessionID(ctx, sessionID)
	if IsNotFound(err) {
		return nil, apperr.InterviewSessionNotFound()
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询面试会话失败", err)
	}
	answers, err := s.store.AnswersBySessionID(ctx, entity.ID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询作答记录失败", err)
	}
	questions := []QuestionDTO{}
	if entity.QuestionsJSON != nil && *entity.QuestionsJSON != "" {
		if parsed, perr := DeserializeQuestions(*entity.QuestionsJSON); perr == nil {
			questions = parsed
		}
	}
	return buildInterviewDetail(entity, questions, answers), nil
}

// ===== 明细组装（Java InterviewHistoryService.buildAnswerDetailList / MapStruct 逐字）=====

// buildInterviewDetail 实体 + 题目 + 作答 → 明细 DTO。
func buildInterviewDetail(entity *SessionEntity, questions []QuestionDTO, answers []AnswerEntity) *InterviewDetailDTO {
	detail := &InterviewDetailDTO{
		ID:               entity.ID,
		SessionID:        entity.SessionID,
		TotalQuestions:   entity.TotalQuestions,
		EvaluateError:    entity.EvaluateError,
		OverallScore:     entity.OverallScore,
		KnowledgeBaseID:  entity.KnowledgeBaseID,
		OverallFeedback:  entity.OverallFeedback,
		CreatedAt:        entity.CreatedAt,
		CompletedAt:      entity.CompletedAt,
		Questions:        rawJSONList(entity.QuestionsJSON),
		Strengths:        parseStringList(entity.StrengthsJSON),
		Improvements:     parseStringList(entity.ImprovementsJSON),
		ReferenceAnswers: rawJSONList(entity.ReferenceAnswersJSON),
		Answers:          buildAnswerDetailList(questions, answers),
	}
	if entity.Status != nil {
		detail.Status = stringPtr(string(*entity.Status))
	}
	if entity.EvaluateStatus != nil {
		detail.EvaluateStatus = stringPtr(string(*entity.EvaluateStatus))
	}
	if entity.SourceType != nil {
		detail.SourceType = stringPtr(string(*entity.SourceType))
	}
	return detail
}

// buildAnswerDetailList 所有题目都返回，未回答的构造空记录（Java 逐字）：
// userAnswer/referenceAnswer/keyPoints/answeredAt 为 null，score 取题目 score ?? 0、
// feedback 取题目 feedback。无题目数据时回退到仅显示已回答的题目。answers 按题号排序。
func buildAnswerDetailList(questions []QuestionDTO, answers []AnswerEntity) []AnswerDetailDTO {
	if len(questions) == 0 {
		out := make([]AnswerDetailDTO, 0, len(answers))
		for i := range answers {
			out = append(out, answerDetailFromEntity(&answers[i]))
		}
		sort.SliceStable(out, func(i, j int) bool {
			return detailIndex(out[i]) < detailIndex(out[j])
		})
		return out
	}

	byIndex := map[int]*AnswerEntity{}
	for i := range answers {
		if answers[i].QuestionIndex == nil {
			continue
		}
		if _, ok := byIndex[*answers[i].QuestionIndex]; !ok {
			byIndex[*answers[i].QuestionIndex] = &answers[i] // 重复取第一个（Java toMap merge）
		}
	}

	out := make([]AnswerDetailDTO, 0, len(questions))
	for _, question := range questions {
		if answer, ok := byIndex[question.QuestionIndex]; ok {
			out = append(out, answerDetailFromEntity(answer))
			continue
		}
		score := 0
		if question.Score != nil {
			score = *question.Score
		}
		out = append(out, AnswerDetailDTO{
			QuestionIndex:   intPtr(question.QuestionIndex),
			Question:        stringPtr(question.Question),
			Category:        question.Category,
			UserAnswer:      nil, // 未回答
			Score:           intPtr(score),
			Feedback:        question.Feedback,
			ReferenceAnswer: nil,
			KeyPoints:       nil,
			AnsweredAt:      nil,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return detailIndex(out[i]) < detailIndex(out[j])
	})
	return out
}

// answerDetailFromEntity 作答行 → 明细项（MapStruct toAnswerDetailDTO 逐字）。
func answerDetailFromEntity(a *AnswerEntity) AnswerDetailDTO {
	answeredAt := a.AnsweredAt
	return AnswerDetailDTO{
		QuestionIndex:   a.QuestionIndex,
		Question:        a.Question,
		Category:        a.Category,
		UserAnswer:      a.UserAnswer,
		Score:           a.Score,
		Feedback:        a.Feedback,
		ReferenceAnswer: a.ReferenceAnswer,
		KeyPoints:       parseStringList(a.KeyPointsJSON),
		AnsweredAt:      &answeredAt,
	}
}

func detailIndex(d AnswerDetailDTO) int {
	if d.QuestionIndex == nil {
		return -1
	}
	return *d.QuestionIndex
}

// ===== 存量报告组装 =====

// buildStoredReport 从 DB 列重建报告 DTO（裁决 #2 存量返回路径）。
func buildStoredReport(entity *SessionEntity, questions []QuestionDTO, answers []AnswerEntity) *ReportDTO {
	byIndex := map[int]*AnswerEntity{}
	for i := range answers {
		if answers[i].QuestionIndex == nil {
			continue
		}
		if _, ok := byIndex[*answers[i].QuestionIndex]; !ok {
			byIndex[*answers[i].QuestionIndex] = &answers[i]
		}
	}

	details := []QuestionEvaluationDTO{}
	for _, question := range questions {
		detail := QuestionEvaluationDTO{
			QuestionIndex: question.QuestionIndex,
			Question:      question.Question,
			Category:      derefString(question.Category),
			UserAnswer:    nil,
			Score:         0,
			Feedback:      "",
		}
		if answer, ok := byIndex[question.QuestionIndex]; ok {
			detail.UserAnswer = answer.UserAnswer
			if answer.Score != nil {
				detail.Score = *answer.Score
			}
			detail.Feedback = derefString(answer.Feedback)
		}
		details = append(details, detail)
	}

	totalQuestions := len(questions)
	if totalQuestions == 0 && entity.TotalQuestions != nil {
		totalQuestions = *entity.TotalQuestions
	}
	overallScore := 0
	if entity.OverallScore != nil {
		overallScore = *entity.OverallScore
	}
	report := &ReportDTO{
		SessionID:        entity.SessionID,
		TotalQuestions:   totalQuestions,
		OverallScore:     overallScore,
		CategoryScores:   categoryScoresOf(details),
		QuestionDetails:  details,
		OverallFeedback:  derefString(entity.OverallFeedback),
		Strengths:        parseStringList(entity.StrengthsJSON),
		Improvements:     parseStringList(entity.ImprovementsJSON),
		ReferenceAnswers: parseReferenceAnswers(entity.ReferenceAnswersJSON),
	}
	if report.Strengths == nil {
		report.Strengths = []string{}
	}
	if report.Improvements == nil {
		report.Improvements = []string{}
	}
	if report.ReferenceAnswers == nil {
		report.ReferenceAnswers = []ReferenceAnswerDTO{}
	}
	return report
}

// categoryScoresOf 分类得分（票 05 计分语义：分类平均 int 截断 + 题数，按分类名稳定排序）。
func categoryScoresOf(details []QuestionEvaluationDTO) []CategoryScoreDTO {
	scoresByCategory := map[string][]int{}
	for _, detail := range details {
		scoresByCategory[detail.Category] = append(scoresByCategory[detail.Category], detail.Score)
	}
	categories := make([]string, 0, len(scoresByCategory))
	for category := range scoresByCategory {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	scores := []CategoryScoreDTO{}
	for _, category := range categories {
		scores = append(scores, CategoryScoreDTO{
			Category:      category,
			Score:         intAvg(scoresByCategory[category]),
			QuestionCount: len(scoresByCategory[category]),
		})
	}
	return scores
}

// ===== JSON 列解析（Java InterviewHistoryService.parseJson 语义：null/非法 → null）=====

// rawJSONList JSON 数组列原样透传（Java List<Object> 序列化等价）；非法/非数组 → nil。
func rawJSONList(raw *string) json.RawMessage {
	if raw == nil {
		return nil
	}
	var probe []json.RawMessage
	if err := json.Unmarshal([]byte(*raw), &probe); err != nil {
		return nil
	}
	return json.RawMessage(*raw)
}

// parseStringList 字符串数组列解析；null/非法 → nil（JSON null）。
func parseStringList(raw *string) []string {
	if raw == nil {
		return nil
	}
	var list []string
	if err := json.Unmarshal([]byte(*raw), &list); err != nil {
		return nil
	}
	return list
}

// parseReferenceAnswers 参考答案列解析；null/非法 → nil。
func parseReferenceAnswers(raw *string) []ReferenceAnswerDTO {
	if raw == nil {
		return nil
	}
	var list []ReferenceAnswerDTO
	if err := json.Unmarshal([]byte(*raw), &list); err != nil {
		return nil
	}
	return list
}
