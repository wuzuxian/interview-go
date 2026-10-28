// 评估报告落库与会话重放（票 06）：saveReport 列映射（对齐 Java
// InterviewPersistenceService.saveReport，笔记 06 §5 逐字）+ 消费者/同步兜底共用的
// DB 重放（questions_json + interview_answers + resumes 关联简历文本，笔记 06 §4）。
package interview

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

// EvalStore 评估链路的数据接缝（生产实现 *Repo，测试注入内存实现）。
// 方法语义与 Repo 同名方法一致。
type EvalStore interface {
	FindByID(ctx context.Context, id int64) (*SessionEntity, error)
	FindBySessionID(ctx context.Context, sessionID string) (*SessionEntity, error)
	AnswersBySessionID(ctx context.Context, sessionRowID int64) ([]AnswerEntity, error)
	ResumeTextByID(ctx context.Context, resumeID int64) (*string, error)
	UpdateEvaluateStatus(ctx context.Context, sessionID string, status EvaluateStatus, evalError *string) error
	UpdateStatus(ctx context.Context, sessionID string, status SessionStatus) error
	UpdateReportColumns(ctx context.Context, sessionID string, columns map[string]any) error
	UpsertAnswerEval(ctx context.Context, w answerEvalWrite) error
}

// truncateEvaluateError 评估错误列截断（对齐 Java truncateError：超过 500 截断）。
// 按 rune 截断避免切碎多字节字符（列宽 500 为字符语义）。
func truncateEvaluateError(message string) string {
	const maxEvaluateErrorChars = 500
	r := []rune(message)
	if len(r) > maxEvaluateErrorChars {
		return string(r[:maxEvaluateErrorChars])
	}
	return message
}

// truncateEvaluateErrorPtr 指针版：nil 原样（清空列），非空截断。
func truncateEvaluateErrorPtr(message *string) *string {
	if message == nil {
		return nil
	}
	return stringPtr(truncateEvaluateError(*message))
}

// reportSessionColumns saveReport 的 interview_sessions 列映射（纯函数，列映射契约）：
// overall_score / overall_feedback / strengths_json / improvements_json /
// reference_answers_json / status=EVALUATED / completed_at=now（Java 逐字）。
func reportSessionColumns(report *ReportDTO, now time.Time) (map[string]any, error) {
	strengths := report.Strengths
	if strengths == nil {
		strengths = []string{}
	}
	improvements := report.Improvements
	if improvements == nil {
		improvements = []string{}
	}
	references := report.ReferenceAnswers
	if references == nil {
		references = []ReferenceAnswerDTO{}
	}
	strengthsJSON, err := json.Marshal(strengths)
	if err != nil {
		return nil, err
	}
	improvementsJSON, err := json.Marshal(improvements)
	if err != nil {
		return nil, err
	}
	referencesJSON, err := json.Marshal(references)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"overall_score":          report.OverallScore,
		"overall_feedback":       report.OverallFeedback,
		"strengths_json":         string(strengthsJSON),
		"improvements_json":      string(improvementsJSON),
		"reference_answers_json": string(referencesJSON),
		"status":                 StatusEvaluated,
		"completed_at":           now,
	}, nil
}

// answerEvalWrite saveReport 单题回填目标（对齐 Java answersToSave 元素语义）：
// Create=true 新建行（userAnswer 保持 NULL，标注未回答）；否则仅更新评分列
// （不动 user_answer/question/category/answered_at）。ReferenceAnswer / KeyPointsJSON
// 为 nil 表示本次不写该列（对齐 Java refAns 判空、keyPoints 非空才写的逐字语义）。
type answerEvalWrite struct {
	Create          bool
	SessionRowID    int64
	QuestionIndex   int
	Question        string
	Category        *string
	Score           int
	Feedback        string
	ReferenceAnswer *string
	KeyPointsJSON   *string
}

// planAnswerBackfill saveReport 逐题回填计划（纯函数，列映射契约测试对象）：
// 对齐 Java InterviewPersistenceService.saveReport 的 answersToSave 构建——
// report.questionDetails 逐题：既有行更新（score/feedback/参考答案/要点），
// 未回答/缺失题新建行（userAnswer=NULL、score/feedback 来自评估）。
// existing 为该场次既有作答行（按题号索引，重复取第一个，对齐 Java toMap merge）。
func planAnswerBackfill(report *ReportDTO, existing []AnswerEntity, sessionRowID int64) []answerEvalWrite {
	byIndex := map[int]*AnswerEntity{}
	for i := range existing {
		if existing[i].QuestionIndex == nil {
			continue
		}
		if _, ok := byIndex[*existing[i].QuestionIndex]; !ok {
			byIndex[*existing[i].QuestionIndex] = &existing[i]
		}
	}
	refByIndex := map[int]ReferenceAnswerDTO{}
	for _, ref := range report.ReferenceAnswers {
		if _, ok := refByIndex[ref.QuestionIndex]; !ok {
			refByIndex[ref.QuestionIndex] = ref
		}
	}

	writes := make([]answerEvalWrite, 0, len(report.QuestionDetails))
	for _, eval := range report.QuestionDetails {
		w := answerEvalWrite{
			SessionRowID:  sessionRowID,
			QuestionIndex: eval.QuestionIndex,
			Score:         eval.Score,
			Feedback:      eval.Feedback,
		}
		if _, ok := byIndex[eval.QuestionIndex]; !ok {
			// 未回答的题目：创建新记录（Java 逐字注释：userAnswer=null 标注未回答）。
			w.Create = true
			w.Question = eval.Question
			w.Category = stringPtr(eval.Category)
		}
		if ref, ok := refByIndex[eval.QuestionIndex]; ok {
			w.ReferenceAnswer = stringPtr(ref.ReferenceAnswer)
			if len(ref.KeyPoints) > 0 {
				if keyPointsJSON, err := json.Marshal(ref.KeyPoints); err == nil {
					w.KeyPointsJSON = stringPtr(string(keyPointsJSON))
				}
			}
		}
		writes = append(writes, w)
	}
	return writes
}

// saveReport 报告落库（对齐 Java InterviewPersistenceService.saveReport）：
// 会话列（含状态强制 EVALUATED、completed_at=now）→ 逐题回填作答表；
// 序列化失败仅记日志不落库（Java catch JacksonException）；会话不存在 warn + return；
// DB 失败向上传播（消费路径触发重试，同步兜底路径仅告警）。
func saveReport(ctx context.Context, store EvalStore, logger *slog.Logger, sessionID string, report *ReportDTO) error {
	entity, err := store.FindBySessionID(ctx, sessionID)
	if err != nil {
		if IsNotFound(err) {
			logger.Warn("会话不存在", slog.String("sessionId", sessionID))
			return nil
		}
		return err
	}

	columns, err := reportSessionColumns(report, time.Now())
	if err != nil {
		logger.Error("序列化报告失败", slog.String("sessionId", sessionID), slog.String("error", err.Error()))
		return nil
	}
	if err := store.UpdateReportColumns(ctx, sessionID, columns); err != nil {
		return err
	}

	existing, err := store.AnswersBySessionID(ctx, entity.ID)
	if err != nil {
		return err
	}
	for _, w := range planAnswerBackfill(report, existing, entity.ID) {
		if err := store.UpsertAnswerEval(ctx, w); err != nil {
			return err
		}
	}
	logger.Info("面试报告已保存",
		slog.String("sessionId", sessionID), slog.Int("score", report.OverallScore),
		slog.Int("answers", len(report.QuestionDetails)))
	return nil
}

// replayQuestions 从 DB 重放题目与作答（对齐 Java processBusiness / restoreSessionFromEntity）：
// questions_json 反序列化 → 按题号用作答表覆盖 userAnswer（含 NULL 覆盖，Java
// withAnswer 语义——未回答行会把题目上的作答清空）。
func replayQuestions(ctx context.Context, store EvalStore, entity *SessionEntity) ([]QuestionDTO, error) {
	questions := []QuestionDTO{}
	if entity.QuestionsJSON != nil && *entity.QuestionsJSON != "" {
		parsed, err := DeserializeQuestions(*entity.QuestionsJSON)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternalError, "反序列化问题列表失败", err)
		}
		questions = parsed
	}
	answers, err := store.AnswersBySessionID(ctx, entity.ID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询作答记录失败", err)
	}
	for _, a := range answers {
		if a.QuestionIndex == nil {
			continue
		}
		idx := *a.QuestionIndex
		if idx >= 0 && idx < len(questions) {
			questions[idx].UserAnswer = a.UserAnswer
		}
	}
	return questions, nil
}

// resumeTextOf 关联简历正文（评估输入取自 DB 关联，不用请求参数，对齐 Java
// session.getResume().getResumeText()；无简历/正文缺失 → ""）。
func resumeTextOf(ctx context.Context, store EvalStore, entity *SessionEntity) string {
	if entity.ResumeID == nil {
		return ""
	}
	text, err := store.ResumeTextByID(ctx, *entity.ResumeID)
	if err != nil || text == nil {
		return ""
	}
	return *text
}

// derefString 指针解引用（nil → ""）。
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// evalSkillID 场次方向 ID（评估参考段兜底用）。
func evalSkillID(entity *SessionEntity) string {
	if entity.SkillID == nil {
		return ""
	}
	return *entity.SkillID
}
