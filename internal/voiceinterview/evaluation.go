// 语音评估服务（票 08）：消费 voice:evaluate:stream 后的评估生成与报告查询，
// 与 Java VoiceInterviewEvaluationService（279 行）逐字对齐——
// DIALOGUE 消息 → 问答配对（pendingQuestion 状态机 + inferCategory 关键词推断，
// L96-162）→ 复用文字面试评估引擎（interview.EvaluationRunner 接缝：批次、结构化
// 输出、降级共用 app.interview.evaluation 配置段；按方向注入评估参考资料，截断由
// 引擎既有规则负责）→ 报告落 voice_interview_evaluations（JSON 列）。
// 无对话场次生成占位空评估（L192-214）；明细 DTO 组装对齐 buildDetailDTO（L216-272）。
package voiceinterview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/interview"
)

// ErrEvaluationNotFound 评估行不存在（FindEvaluationBySessionID 返回，
// GET evaluation 的 COMPLETED 分支映射 10006）。
var ErrEvaluationNotFound = errors.New("voiceinterview: 评估结果不存在")

// EvalStore 语音评估链路的数据接缝（生产实现 *Repo，测试注入内存实现）——
// 沿 internal/interview EvalStore 的接缝模式，方法语义与 Repo 同名方法一致。
type EvalStore interface {
	FindSessionByID(ctx context.Context, sessionID int64) (*SessionEntity, error)
	FindDialogueMessages(ctx context.Context, sessionID int64) ([]MessageEntity, error)
	UpdateEvaluateStatus(ctx context.Context, sessionID int64, status VoiceEvaluateStatus, evalErr *string) error
	FindEvaluationBySessionID(ctx context.Context, sessionID int64) (*EvaluationEntity, error)
	SaveEvaluation(ctx context.Context, e *EvaluationEntity) error
	SaveEmptyEvaluation(ctx context.Context, e *EvaluationEntity) error
}

// VoiceEvaluator 评估执行接缝（消费者消费；生产实现 *EvaluationService，
// 测试注入 stub——沿 internal/interview 的 EvaluationRunner 消费方式）。
type VoiceEvaluator interface {
	GenerateEvaluation(ctx context.Context, sessionID int64) error
}

// EvaluationService 语音评估服务。
type EvaluationService struct {
	store  EvalStore
	eval   interview.EvaluationRunner
	refs   interview.ReferenceSource
	logger *slog.Logger
}

// EvaluationServiceDeps 评估服务依赖聚合。
type EvaluationServiceDeps struct {
	Store  EvalStore
	Eval   interview.EvaluationRunner // 文字面试评估引擎接缝（批次/结构化输出/降级共用）
	Refs   interview.ReferenceSource  // 方向评估参考段（Java InterviewSkillService 对应物）；可 nil
	Logger *slog.Logger
}

// NewEvaluationService 创建评估服务。
func NewEvaluationService(deps EvaluationServiceDeps) *EvaluationService {
	return &EvaluationService{store: deps.Store, eval: deps.Eval, refs: deps.Refs, logger: deps.Logger}
}

// GenerateEvaluation 生成语音面试评估（由异步消费者调用）。
// LLM 调用在事务外执行，仅 DB 写入单独提交（对齐 Java generateEvaluation L52-94）。
func (s *EvaluationService) GenerateEvaluation(ctx context.Context, sessionID int64) error {
	s.logger.Info("开始生成语音面试评估", slog.Int64("sessionId", sessionID))

	session, err := s.store.FindSessionByID(ctx, sessionID)
	if errors.Is(err, ErrSessionNotFound) {
		return apperr.VoiceSessionNotFoundWithMessage("语音面试会话不存在: " + strconv.FormatInt(sessionID, 10))
	}
	if err != nil {
		return wrapGenerateError(err)
	}

	messages, err := s.store.FindDialogueMessages(ctx, sessionID)
	if err != nil {
		return wrapGenerateError(err)
	}
	if len(messages) == 0 {
		s.logger.Warn("语音面试会话无对话记录，生成空评估结果", slog.Int64("sessionId", sessionID))
		return s.saveEmptyEvaluation(ctx, sessionID, session)
	}

	questions := buildQaRecords(messages)
	referenceContext := ""
	if s.refs != nil {
		referenceContext = s.refs.BuildEvaluationReferenceSectionSafe(stringValue(session.SkillID))
	}
	report, err := s.eval.Evaluate(ctx, interview.EvaluateInput{
		SessionID:      strconv.FormatInt(sessionID, 10),
		ResumeText:     "",
		Questions:      questions,
		SkillReference: referenceContext,
	}, session.LLMProvider)
	if err != nil {
		return wrapGenerateError(err)
	}
	return s.saveEvaluation(ctx, session, report)
}

// wrapGenerateError 异常归一（对齐 Java L79-85：业务错误原样透传，
// 其余包装 10004 "生成评估失败: {msg}"）。
func wrapGenerateError(err error) error {
	var ae *apperr.AppError
	if errors.As(err, &ae) {
		return ae
	}
	return apperr.New(apperr.CodeVoiceEvaluationFailed, "生成评估失败: "+err.Error())
}

// GetEvaluationDetail 读取评估明细（GET/POST evaluation 的 COMPLETED 分支，
// Java getEvaluation L88-94）。行不存在 → 10006 "评估结果不存在: {id}"。
func (s *EvaluationService) GetEvaluationDetail(ctx context.Context, sessionID int64) (*VoiceEvaluationDetailDTO, error) {
	entity, err := s.store.FindEvaluationBySessionID(ctx, sessionID)
	if errors.Is(err, ErrEvaluationNotFound) {
		return nil, apperr.New(apperr.CodeVoiceEvaluationNotFound,
			"评估结果不存在: "+strconv.FormatInt(sessionID, 10))
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternalError, "查询语音评估结果失败", err)
	}
	detail, err := buildVoiceEvaluationDetail(entity)
	if err != nil {
		return nil, err
	}
	return detail, nil
}

// ===== 问答配对（Java buildQaRecords L96-152 逐字）=====

// pendingQuestion 待答状态（Java record PendingQuestion(question, category)）。
type pendingQuestion struct {
	question string
	category string
}

// buildQaRecords DIALOGUE 消息流 → 通用问答记录（interview.QuestionDTO，
// 供文字面试评估引擎直接消费；语音配对不产生追问标记）：
// 连续提问归入待答状态机，回答回填最近未答问题；仅用户发言归"综合"。
func buildQaRecords(messages []MessageEntity) []interview.QuestionDTO {
	records := []interview.QuestionDTO{}
	index := 0
	var pending *pendingQuestion

	for _, msg := range messages {
		aiText := trimToNull(msg.AiGeneratedText)
		userText := trimToNull(msg.UserRecognizedText)

		if pending != nil && userText != nil {
			records = append(records, voiceQuestion(index, pending.question, pending.category, userText))
			index++
			pending = nil
			if aiText != nil {
				pending = &pendingQuestion{*aiText, inferCategory(aiText)}
			}
			continue
		}

		if pending != nil {
			records = append(records, voiceQuestion(index, pending.question, pending.category, nil))
			index++
			pending = nil
		}

		if aiText != nil && userText != nil {
			records = append(records, voiceQuestion(index, *aiText, inferCategory(aiText), userText))
			index++
		} else if aiText != nil {
			pending = &pendingQuestion{*aiText, inferCategory(aiText)}
		} else if userText != nil {
			records = append(records, voiceQuestion(index, "", "综合", userText))
			index++
		}
	}

	if pending != nil {
		records = append(records, voiceQuestion(index, pending.question, pending.category, nil))
	}
	return records
}

// voiceQuestion 单条配对结果（Java QaRecord 四参构造：followUp=false、parent=nil）。
func voiceQuestion(index int, question, category string, userAnswer *string) interview.QuestionDTO {
	return interview.QuestionDTO{
		QuestionIndex: index,
		Question:      question,
		Category:      stringPtr(category),
		UserAnswer:    userAnswer,
	}
}

// trimToNull Java VoiceInterviewMessageEntity.trimToNull：null/空白 → nil，其余 trim。
func trimToNull(text *string) *string {
	if text == nil || strings.TrimSpace(*text) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(*text)
	return &trimmed
}

// inferCategory 提问分类关键词推断（Java inferCategory L156-162 逐字）。
func inferCategory(aiText *string) string {
	if aiText == nil {
		return "综合"
	}
	if strings.Contains(*aiText, "项目") || strings.Contains(*aiText, "实习") || strings.Contains(*aiText, "工作经历") {
		return "项目深挖"
	}
	if strings.Contains(*aiText, "自我介绍") || strings.Contains(*aiText, "介绍一下自己") {
		return "自我介绍"
	}
	if strings.Contains(*aiText, "职业规划") || strings.Contains(*aiText, "为什么") || strings.Contains(*aiText, "优缺点") {
		return "HR问题"
	}
	return "技术问题"
}

// ===== 报告落库（Java saveEvaluationTransactional L164-190）=====

// saveEvaluation 报告落 voice_interview_evaluations（JSON 列 + roleType/startTime 冗余列）。
func (s *EvaluationService) saveEvaluation(ctx context.Context, session *SessionEntity, report *interview.ReportDTO) error {
	entity, err := evaluationEntityFromReport(session, report)
	if err != nil {
		s.logger.Error("保存评估结果失败",
			slog.Int64("sessionId", session.ID), slog.String("error", err.Error()))
		return apperr.New(apperr.CodeVoiceEvaluationFailed, "保存评估失败: "+err.Error())
	}
	if err := s.store.SaveEvaluation(ctx, entity); err != nil {
		return apperr.New(apperr.CodeVoiceEvaluationFailed, "保存评估失败: "+err.Error())
	}
	s.logger.Info("评估结果已保存",
		slog.Int64("sessionId", session.ID), slog.Int("score", report.OverallScore))
	return nil
}

// evaluationEntityFromReport 报告 → 评估行（列映射契约）：
// JSON 列 = 引擎报告的逐题评估/参考答案/优势/改进（与文字面试 ReportDTO 的
// JSON 字段名逐字一致），interviewer_role=roleType、interview_date=startTime。
func evaluationEntityFromReport(session *SessionEntity, report *interview.ReportDTO) (*EvaluationEntity, error) {
	marshal := func(v any) (*string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		s := string(b)
		return &s, nil
	}
	questions := report.QuestionDetails
	if questions == nil {
		questions = []interview.QuestionEvaluationDTO{}
	}
	refs := report.ReferenceAnswers
	if refs == nil {
		refs = []interview.ReferenceAnswerDTO{}
	}
	strengths := report.Strengths
	if strengths == nil {
		strengths = []string{}
	}
	improvements := report.Improvements
	if improvements == nil {
		improvements = []string{}
	}

	qJSON, err := marshal(questions)
	if err != nil {
		return nil, err
	}
	sJSON, err := marshal(strengths)
	if err != nil {
		return nil, err
	}
	iJSON, err := marshal(improvements)
	if err != nil {
		return nil, err
	}
	rJSON, err := marshal(refs)
	if err != nil {
		return nil, err
	}

	return &EvaluationEntity{
		SessionID:               &session.ID,
		OverallScore:            intPtr(report.OverallScore),
		OverallFeedback:         stringPtr(report.OverallFeedback),
		QuestionEvaluationsJSON: qJSON,
		StrengthsJSON:           sJSON,
		ImprovementsJSON:        iJSON,
		ReferenceAnswersJSON:    rJSON,
		InterviewerRole:         stringPtr(session.RoleType),
		InterviewDate:           session.StartTime,
	}, nil
}

// ===== 空评估（Java saveEmptyEvaluationTransactional L192-214 逐字）=====

// saveEmptyEvaluation 占位空评估：合法报告（totalQuestions=0）而非失败；
// 既有评估行被更新（Java findBySessionId().orElseGet(build)）。
func (s *EvaluationService) saveEmptyEvaluation(ctx context.Context, sessionID int64, session *SessionEntity) error {
	entity := &EvaluationEntity{
		SessionID:               &sessionID,
		OverallScore:            nil,
		OverallFeedback:         stringPtr("本次语音面试未形成有效对话记录，暂无可评估内容。"),
		QuestionEvaluationsJSON: stringPtr("[]"),
		StrengthsJSON:           stringPtr("[]"),
		ImprovementsJSON:        stringPtr("[\"请先完成至少一轮有效问答后再生成评估。\"]"),
		ReferenceAnswersJSON:    stringPtr("[]"),
		InterviewerRole:         stringPtr(session.RoleType),
		InterviewDate:           session.StartTime,
	}
	if err := s.store.SaveEmptyEvaluation(ctx, entity); err != nil {
		return apperr.New(apperr.CodeVoiceEvaluationFailed, "保存空评估失败: "+err.Error())
	}
	s.logger.Info("空评估结果已保存", slog.Int64("sessionId", sessionID))
	return nil
}

// ===== 明细组装（Java buildDetailDTO L216-272 逐字）=====

// buildVoiceEvaluationDetail 评估行 → 明细 DTO：逐题评估按 questionIndex 关联
// 参考答案（重复索引取第一条，Java Collectors.toMap merge (a,b)->a）。
func buildVoiceEvaluationDetail(entity *EvaluationEntity) (*VoiceEvaluationDetailDTO, error) {
	var questionItems []interview.QuestionEvaluationDTO
	if err := unmarshalJSONColumn(entity.QuestionEvaluationsJSON, &questionItems); err != nil {
		return nil, wrapDetailError(err)
	}
	var strengths []string
	if err := unmarshalJSONColumn(entity.StrengthsJSON, &strengths); err != nil {
		return nil, wrapDetailError(err)
	}
	var improvements []string
	if err := unmarshalJSONColumn(entity.ImprovementsJSON, &improvements); err != nil {
		return nil, wrapDetailError(err)
	}
	var refAnswers []interview.ReferenceAnswerDTO
	if err := unmarshalJSONColumn(entity.ReferenceAnswersJSON, &refAnswers); err != nil {
		return nil, wrapDetailError(err)
	}

	refMap := map[int]interview.ReferenceAnswerDTO{}
	for _, r := range refAnswers {
		if _, ok := refMap[r.QuestionIndex]; !ok {
			refMap[r.QuestionIndex] = r
		}
	}

	answers := make([]VoiceAnswerDetailDTO, 0, len(questionItems))
	for _, q := range questionItems {
		ref, hasRef := refMap[q.QuestionIndex]
		detail := VoiceAnswerDetailDTO{
			QuestionIndex: q.QuestionIndex,
			Question:      q.Question,
			Category:      q.Category,
			UserAnswer:    q.UserAnswer,
			Score:         q.Score,
			Feedback:      q.Feedback,
		}
		if hasRef {
			detail.ReferenceAnswer = stringPtr(ref.ReferenceAnswer)
			detail.KeyPoints = ref.KeyPoints
		}
		answers = append(answers, detail)
	}

	return &VoiceEvaluationDetailDTO{
		SessionID:       intValue0(entity.SessionID),
		TotalQuestions:  len(answers),
		OverallScore:    entity.OverallScore,
		OverallFeedback: entity.OverallFeedback,
		Strengths:       strengths,
		Improvements:    improvements,
		Answers:         answers,
	}, nil
}

// unmarshalJSONColumn JSON 列反序列化；列为空对齐 Java readValue(null) 抛错路径。
func unmarshalJSONColumn(raw *string, out any) error {
	if raw == nil || *raw == "" {
		return fmt.Errorf("评估 JSON 列为空")
	}
	return json.Unmarshal([]byte(*raw), out)
}

// wrapDetailError 明细构建失败归一（Java L267-271：10004 "构建评估结果失败: {msg}"）。
func wrapDetailError(err error) error {
	return apperr.New(apperr.CodeVoiceEvaluationFailed, "构建评估结果失败: "+err.Error())
}

// stringValue 指针解引用（nil → ""）。
func stringValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// intValue0 指针解引用（nil → 0）。
func intValue0(i *int64) int64 {
	if i == nil {
		return 0
	}
	return *i
}

// truncateVoiceErrMsg evaluate_error 消息截断 500（对齐 Java truncateError：
// 超过 500 截断；按 rune 切避免切碎多字节字符，列宽 500 为字符语义）。
func truncateVoiceErrMsg(message string) string {
	const maxEvaluateErrorChars = 500
	runes := []rune(message)
	if len(runes) > maxEvaluateErrorChars {
		return string(runes[:maxEvaluateErrorChars])
	}
	return message
}
