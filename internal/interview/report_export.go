// 报告 PDF 导出（票 08）：GET /sessions/{id}/export。
//
// 数据获取复用票 06 的 details 路径（entity + questions + answers，笔记 07），
// 组装 pdfexport.InterviewReportInput。导出行为对齐 Java PdfExportService：
// 纯 DB 驱动、状态无关——COMPLETED/未评估场次照常导出（无总分则跳过"综合评分"节，
// 不触发同步评估、不报错），未完成场次亦导出（笔记 07 §5）。与 GET /report 的
// "COMPLETED 走同步兜底评估"（裁决 #2）不同：导出不评估。
package interview

import (
	"context"
	"log/slog"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/pdfexport"
)

// GetInterviewExportInput 组装报告 PDF 视图模型（复用 details 数据路径）。
// 场次不存在 → 3001；其余查询失败 → 500。answers 查询失败仅告警（不阻断导出）。
func (s *ReportService) GetInterviewExportInput(ctx context.Context, sessionID string) (*pdfexport.InterviewReportInput, error) {
	entity, err := s.store.FindBySessionID(ctx, sessionID)
	if IsNotFound(err) {
		return nil, apperr.InterviewSessionNotFound()
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询面试会话失败", err)
	}

	questions := []QuestionDTO{}
	if entity.QuestionsJSON != nil && *entity.QuestionsJSON != "" {
		if parsed, perr := DeserializeQuestions(*entity.QuestionsJSON); perr == nil {
			questions = parsed
		}
	}
	answers, err := s.store.AnswersBySessionID(ctx, entity.ID)
	if err != nil {
		s.logger.Warn("查询作答记录失败", slog.String("sessionId", entity.SessionID), slog.String("error", err.Error()))
		answers = nil
	}
	return buildInterviewExportInput(entity, questions, answers), nil
}

// buildInterviewExportInput entity + questions + answers → PDF 视图模型（纯函数）。
// 问答详情复用 buildAnswerDetailList 语义（全部题目、未答空记录），分类得分复用
// categoryScoresOf（票 05 计分语义），追问标记由题目 isFollowUp 回填。
func buildInterviewExportInput(entity *SessionEntity, questions []QuestionDTO, answers []AnswerEntity) *pdfexport.InterviewReportInput {
	details := buildAnswerDetailList(questions, answers)

	followUp := make(map[int]bool, len(questions))
	for _, q := range questions {
		followUp[q.QuestionIndex] = q.IsFollowUp
	}

	blocks := make([]pdfexport.InterviewQuestionBlock, 0, len(details))
	for i := range details {
		d := &details[i]
		idx := detailIndex(*d)
		number := idx + 1
		if idx < 0 {
			number = i + 1
		}
		blocks = append(blocks, pdfexport.InterviewQuestionBlock{
			Number:          number,
			Category:        derefString(d.Category),
			IsFollowUp:      followUp[idx],
			Question:        derefString(d.Question),
			UserAnswer:      d.UserAnswer,
			Score:           derefInt(d.Score),
			Feedback:        derefString(d.Feedback),
			ReferenceAnswer: derefString(d.ReferenceAnswer),
			KeyPoints:       d.KeyPoints,
		})
	}

	return &pdfexport.InterviewReportInput{
		SessionID:       entity.SessionID,
		TotalQuestions:  exportTotalQuestions(entity, questions),
		StatusText:      statusTextOf(entity.Status),
		CreatedAt:       entity.CreatedAt,
		CompletedAt:     entity.CompletedAt,
		OverallScore:    entity.OverallScore,
		OverallFeedback: derefString(entity.OverallFeedback),
		Strengths:       parseStringList(entity.StrengthsJSON),
		Improvements:    parseStringList(entity.ImprovementsJSON),
		CategoryScores:  exportCategoryScores(details),
		Questions:       blocks,
	}
}

// exportTotalQuestions 题数：优先 total_questions 列，缺失回退题目数（与报告口径一致）。
func exportTotalQuestions(entity *SessionEntity, questions []QuestionDTO) int {
	if entity.TotalQuestions != nil {
		return *entity.TotalQuestions
	}
	return len(questions)
}

// exportCategoryScores 分类得分（复用票 05 categoryScoresOf 计分语义）。
func exportCategoryScores(details []AnswerDetailDTO) []pdfexport.InterviewCategoryScore {
	evals := make([]QuestionEvaluationDTO, 0, len(details))
	for i := range details {
		d := &details[i]
		evals = append(evals, QuestionEvaluationDTO{
			QuestionIndex: derefInt(d.QuestionIndex),
			Question:      derefString(d.Question),
			Category:      derefString(d.Category),
			UserAnswer:    d.UserAnswer,
			Score:         derefInt(d.Score),
			Feedback:      derefString(d.Feedback),
		})
	}
	scores := categoryScoresOf(evals)
	out := make([]pdfexport.InterviewCategoryScore, 0, len(scores))
	for _, cs := range scores {
		out = append(out, pdfexport.InterviewCategoryScore{
			Category:      cs.Category,
			Score:         cs.Score,
			QuestionCount: cs.QuestionCount,
		})
	}
	return out
}

// statusTextOf 状态中文映射（Java getStatusText 逐字，笔记 07）。
func statusTextOf(status *SessionStatus) string {
	if status == nil {
		return ""
	}
	switch *status {
	case StatusCreated:
		return "已创建"
	case StatusInProgress:
		return "进行中"
	case StatusCompleted:
		return "已完成"
	case StatusEvaluated:
		return "已评估"
	}
	return ""
}

// derefInt 指针解引用，nil → 0（空值防御）。
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
