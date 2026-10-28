// 评估服务单测：问答配对状态机、分类关键词推断、评估生成（正常/空对话/异常）、
// 报告落库列映射与明细 DTO 组装。契约来源：Java VoiceInterviewEvaluationService
// 原文逐字（/home/wuzuxian/interview-guide，L96-278）。
package voiceinterview

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/interview"
)

// ===== 问答配对（pendingQuestion 状态机，Java buildQaRecords L96-152 逐字）=====

// qaWant 配对结果的断言形状。
type qaWant struct {
	index    int
	question string
	category string
	answered bool
	answer   string
}

func voicePtr(s string) *string { return stringPtr(s) }

func TestBuildQaRecords(t *testing.T) {
	tests := []struct {
		name     string
		messages []MessageEntity
		want     []qaWant
	}{
		{
			name:     "一问一答成对",
			messages: []MessageEntity{voiceMsg(5, 0, voicePtr("介绍一下你的项目"), voicePtr("我做了电商系统"))},
			want:     []qaWant{{0, "介绍一下你的项目", "项目深挖", true, "我做了电商系统"}},
		},
		{
			name: "连续提问累积且回答回填最近未答",
			messages: []MessageEntity{
				voiceMsg(5, 0, voicePtr("先讲讲项目背景"), nil),
				voiceMsg(5, 1, voicePtr("项目里最难的技术点是什么"), nil),
				voiceMsg(5, 2, nil, voicePtr("缓存一致性的处理")),
			},
			want: []qaWant{
				{0, "先讲讲项目背景", "项目深挖", false, ""},
				{1, "项目里最难的技术点是什么", "项目深挖", true, "缓存一致性的处理"},
			},
		},
		{
			name: "末尾未获回答的提问保留为未答记录",
			messages: []MessageEntity{
				voiceMsg(5, 0, voicePtr("介绍项目"), voicePtr("回答")),
				voiceMsg(5, 1, voicePtr("再讲讲索引"), nil),
			},
			want: []qaWant{
				{0, "介绍项目", "项目深挖", true, "回答"},
				{1, "再讲讲索引", "技术问题", false, ""},
			},
		},
		{
			name:     "仅用户发言归入综合类且题干为空",
			messages: []MessageEntity{voiceMsg(5, 0, nil, voicePtr("你好"))},
			want:     []qaWant{{0, "", "综合", true, "你好"}},
		},
		{
			name:     "空白文本按缺失处理（trimToNull）",
			messages: []MessageEntity{voiceMsg(5, 0, voicePtr("   "), voicePtr("内容"))},
			want:     []qaWant{{0, "", "综合", true, "内容"}},
		},
		{
			name: "回答之后的新提问成为下一轮待答",
			messages: []MessageEntity{
				voiceMsg(5, 0, voicePtr("第一个问题"), voicePtr("第一个回答")),
				voiceMsg(5, 1, voicePtr("第二个问题"), nil),
			},
			want: []qaWant{
				{0, "第一个问题", "技术问题", true, "第一个回答"},
				{1, "第二个问题", "技术问题", false, ""},
			},
		},
		{
			name:     "空消息列表产出空配对",
			messages: nil,
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildQaRecords(tt.messages)
			if len(got) != len(tt.want) {
				t.Fatalf("配对数量不符: got %d, want %d (%+v)", len(got), len(tt.want), got)
			}
			for i, w := range tt.want {
				g := got[i]
				if g.QuestionIndex != w.index {
					t.Errorf("第 %d 条 questionIndex = %d, want %d", i, g.QuestionIndex, w.index)
				}
				if g.Question != w.question {
					t.Errorf("第 %d 条 question = %q, want %q", i, g.Question, w.question)
				}
				if g.Category == nil || *g.Category != w.category {
					t.Errorf("第 %d 条 category = %v, want %q", i, g.Category, w.category)
				}
				if w.answered && (g.UserAnswer == nil || *g.UserAnswer != w.answer) {
					t.Errorf("第 %d 条应已回答 %q, got %v", i, w.answer, g.UserAnswer)
				}
				if !w.answered && g.UserAnswer != nil {
					t.Errorf("第 %d 条应未回答, got %q", i, *g.UserAnswer)
				}
				if g.IsFollowUp {
					t.Errorf("第 %d 条语音配对不产生追问标记", i)
				}
			}
		})
	}
}

// ===== 分类关键词推断（Java inferCategory L156-162 逐字）=====

func TestInferCategory(t *testing.T) {
	tests := []struct {
		name   string
		aiText *string
		want   string
	}{
		{name: "空文本归综合", aiText: nil, want: "综合"},
		{name: "含项目归项目深挖", aiText: voicePtr("介绍一下你做的项目"), want: "项目深挖"},
		{name: "含实习归项目深挖", aiText: voicePtr("实习期间做了什么"), want: "项目深挖"},
		{name: "含工作经历归项目深挖", aiText: voicePtr("讲讲你的工作经历"), want: "项目深挖"},
		{name: "含自我介绍归自我介绍", aiText: voicePtr("请先做个自我介绍"), want: "自我介绍"},
		{name: "含介绍一下自己归自我介绍", aiText: voicePtr("简单介绍一下自己"), want: "自我介绍"},
		{name: "含职业规划归HR问题", aiText: voicePtr("你的职业规划是什么"), want: "HR问题"},
		{name: "含为什么归HR问题", aiText: voicePtr("为什么选择后端方向"), want: "HR问题"},
		{name: "含优缺点归HR问题", aiText: voicePtr("说说你的优缺点"), want: "HR问题"},
		{name: "无命中归技术问题", aiText: voicePtr("讲讲 MySQL 索引原理"), want: "技术问题"},
		{name: "关键词按顺序先命中项目", aiText: voicePtr("自我介绍里的项目经历为什么重要"), want: "项目深挖"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inferCategory(tt.aiText); got != tt.want {
				t.Errorf("inferCategory(%v) = %q, want %q", tt.aiText, got, tt.want)
			}
		})
	}
}

// ===== 评估生成（generateEvaluation 编排，Java L52-94）=====

func TestGenerateEvaluationNormal(t *testing.T) {
	f := newEvalChainFixture()
	session := evalChainSession(5, evalPtr(VoiceEvaluateProcess))
	f.store.addSession(session)
	f.store.setDialogues(5, []MessageEntity{
		voiceMsg(5, 0, voicePtr("介绍一下你的项目"), voicePtr("我做了电商系统")),
	})
	f.runner.report = sampleVoiceReport(5)

	if err := f.svc.GenerateEvaluation(context.Background(), 5); err != nil {
		t.Fatalf("生成评估失败: %v", err)
	}

	if f.runner.calls != 1 {
		t.Fatalf("评估引擎应被调用 1 次, got %d", f.runner.calls)
	}
	input := f.runner.lastInput
	if input.SessionID != "5" {
		t.Errorf("引擎入参 sessionId = %q, want \"5\"", input.SessionID)
	}
	if input.ResumeText != "" {
		t.Errorf("语音评估不注入简历文本, got %q", input.ResumeText)
	}
	// 方向参考段原样注入（截断由评估引擎内部既有规则负责，Java evaluate L130-134）。
	if input.SkillReference != f.refs.text {
		t.Errorf("引擎入参参考段 = %q, want %q", input.SkillReference, f.refs.text)
	}
	if f.refs.lastSkill != "java-backend" {
		t.Errorf("参考段应按场次方向定位, got skillID %q", f.refs.lastSkill)
	}
	if f.runner.lastProvider == nil || *f.runner.lastProvider != "test-provider" {
		t.Errorf("引擎应按场次 llmProvider 解析, got %v", f.runner.lastProvider)
	}
	if len(input.Questions) != 1 || input.Questions[0].Question != "介绍一下你的项目" {
		t.Errorf("引擎入参题目应来自问答配对, got %+v", input.Questions)
	}

	entity, ok := f.store.evaluations[5]
	if !ok {
		t.Fatal("评估报告应落 voice_interview_evaluations")
	}
	if entity.OverallScore == nil || *entity.OverallScore != 72 {
		t.Errorf("overall_score = %v, want 72", entity.OverallScore)
	}
	if entity.OverallFeedback == nil || *entity.OverallFeedback != "总体反馈" {
		t.Errorf("overall_feedback = %v", entity.OverallFeedback)
	}
	if entity.SessionID == nil || *entity.SessionID != 5 {
		t.Errorf("session_id = %v", entity.SessionID)
	}
	if entity.InterviewerRole == nil || *entity.InterviewerRole != "java-backend" {
		t.Errorf("interviewer_role 应取场次 roleType, got %v", entity.InterviewerRole)
	}
	if entity.InterviewDate != session.StartTime {
		t.Errorf("interview_date 应取场次 startTime, got %v", entity.InterviewDate)
	}
	// JSON 列形状与文字面试报告逐题/参考列表一致（Java writeValueAsString(questionItems)）。
	var questions []interview.QuestionEvaluationDTO
	if err := json.Unmarshal([]byte(*entity.QuestionEvaluationsJSON), &questions); err != nil {
		t.Fatalf("question_evaluations_json 应为逐题评估列表: %v", err)
	}
	if len(questions) != 2 {
		t.Errorf("question_evaluations_json 应含 2 题, got %d", len(questions))
	}
	var refs []interview.ReferenceAnswerDTO
	if err := json.Unmarshal([]byte(*entity.ReferenceAnswersJSON), &refs); err != nil {
		t.Fatalf("reference_answers_json 应为参考答案列表: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("reference_answers_json 应含 2 条, got %d", len(refs))
	}
	var strengths, improvements []string
	if err := json.Unmarshal([]byte(*entity.StrengthsJSON), &strengths); err != nil || len(strengths) != 1 {
		t.Errorf("strengths_json 形状不符: %v %v", strengths, err)
	}
	if err := json.Unmarshal([]byte(*entity.ImprovementsJSON), &improvements); err != nil || len(improvements) != 1 {
		t.Errorf("improvements_json 形状不符: %v %v", improvements, err)
	}
}

func TestGenerateEvaluationEmptyDialogue(t *testing.T) {
	f := newEvalChainFixture()
	session := evalChainSession(5, evalPtr(VoiceEvaluateProcess))
	f.store.addSession(session)

	if err := f.svc.GenerateEvaluation(context.Background(), 5); err != nil {
		t.Fatalf("空对话评估不应报错: %v", err)
	}
	if f.runner.calls != 0 {
		t.Errorf("空对话不应调用评估引擎, got %d 次", f.runner.calls)
	}
	entity, ok := f.store.evaluations[5]
	if !ok {
		t.Fatal("空对话应落占位空评估")
	}
	if entity.OverallScore != nil {
		t.Errorf("空评估 overall_score 应为 null, got %v", *entity.OverallScore)
	}
	if entity.OverallFeedback == nil || *entity.OverallFeedback != "本次语音面试未形成有效对话记录，暂无可评估内容。" {
		t.Errorf("空评估总评语逐字不符: %v", entity.OverallFeedback)
	}
	if entity.QuestionEvaluationsJSON == nil || *entity.QuestionEvaluationsJSON != "[]" {
		t.Errorf("空评估逐题列表应为 [], got %v", entity.QuestionEvaluationsJSON)
	}
	if entity.StrengthsJSON == nil || *entity.StrengthsJSON != "[]" {
		t.Errorf("空评估优势应为 [], got %v", entity.StrengthsJSON)
	}
	if entity.ImprovementsJSON == nil || *entity.ImprovementsJSON != "[\"请先完成至少一轮有效问答后再生成评估。\"]" {
		t.Errorf("空评估改进建议逐字不符: %v", entity.ImprovementsJSON)
	}
	if entity.ReferenceAnswersJSON == nil || *entity.ReferenceAnswersJSON != "[]" {
		t.Errorf("空评估参考答案应为 [], got %v", entity.ReferenceAnswersJSON)
	}
	if entity.InterviewerRole == nil || *entity.InterviewerRole != "java-backend" {
		t.Errorf("空评估 interviewer_role = %v", entity.InterviewerRole)
	}
}

func TestGenerateEvaluationSessionMissing(t *testing.T) {
	f := newEvalChainFixture()
	err := f.svc.GenerateEvaluation(context.Background(), 5)
	var ae *apperr.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("应返回业务错误, got %v", err)
	}
	if ae.Code != apperr.CodeVoiceSessionNotFound {
		t.Errorf("错误码 = %d, want 10001", ae.Code)
	}
	if ae.Message != "语音面试会话不存在: 5" {
		t.Errorf("错误消息 = %q, want %q", ae.Message, "语音面试会话不存在: 5")
	}
}

func TestGenerateEvaluationEngineFailureWrapped(t *testing.T) {
	f := newEvalChainFixture()
	f.store.addSession(evalChainSession(5, evalPtr(VoiceEvaluateProcess)))
	f.store.setDialogues(5, []MessageEntity{
		voiceMsg(5, 0, voicePtr("问题"), voicePtr("回答")),
	})
	f.runner.err = errEvalBoom

	err := f.svc.GenerateEvaluation(context.Background(), 5)
	var ae *apperr.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("应返回业务错误, got %v", err)
	}
	if ae.Code != apperr.CodeVoiceEvaluationFailed {
		t.Errorf("错误码 = %d, want 10004", ae.Code)
	}
	if ae.Message != "生成评估失败: boom" {
		t.Errorf("错误消息 = %q, want %q", ae.Message, "生成评估失败: boom")
	}
}

// ===== 明细 DTO（buildDetailDTO，Java L216-272 逐字）=====

func TestGetEvaluationDetailMergesReferences(t *testing.T) {
	f := newEvalChainFixture()
	session := evalChainSession(5, evalPtr(VoiceEvaluateProcess))
	f.store.addSession(session)
	f.store.setDialogues(5, []MessageEntity{
		voiceMsg(5, 0, voicePtr("介绍一下你的项目"), voicePtr("回答")),
	})
	f.runner.report = sampleVoiceReport(5)
	if err := f.svc.GenerateEvaluation(context.Background(), 5); err != nil {
		t.Fatalf("生成评估失败: %v", err)
	}

	detail, err := f.svc.GetEvaluationDetail(context.Background(), 5)
	if err != nil {
		t.Fatalf("读取明细失败: %v", err)
	}
	if detail.SessionID != 5 {
		t.Errorf("sessionId = %d, want 5", detail.SessionID)
	}
	if detail.TotalQuestions != 2 {
		t.Errorf("totalQuestions = %d, want 2", detail.TotalQuestions)
	}
	if detail.OverallScore == nil || *detail.OverallScore != 72 {
		t.Errorf("overallScore = %v, want 72", detail.OverallScore)
	}
	if detail.OverallFeedback == nil || *detail.OverallFeedback != "总体反馈" {
		t.Errorf("overallFeedback = %v", detail.OverallFeedback)
	}
	if len(detail.Strengths) != 1 || len(detail.Improvements) != 1 {
		t.Errorf("strengths/improvements 形状不符: %v / %v", detail.Strengths, detail.Improvements)
	}
	if len(detail.Answers) != 2 {
		t.Fatalf("answers 应含 2 条, got %d", len(detail.Answers))
	}
	a0 := detail.Answers[0]
	if a0.QuestionIndex != 0 || a0.Question != "介绍一下你的项目" || a0.Category != "项目深挖" {
		t.Errorf("answers[0] 题面不符: %+v", a0)
	}
	if a0.UserAnswer == nil || *a0.UserAnswer != "我的回答" {
		t.Errorf("answers[0].userAnswer = %v, want 我的回答", a0.UserAnswer)
	}
	if a0.Score != 80 || a0.Feedback != "反馈一" {
		t.Errorf("answers[0] 评分不符: %d %q", a0.Score, a0.Feedback)
	}
	if a0.ReferenceAnswer == nil || *a0.ReferenceAnswer != "参考一" {
		t.Errorf("answers[0].referenceAnswer = %v, want 参考一", a0.ReferenceAnswer)
	}
	if len(a0.KeyPoints) != 1 || a0.KeyPoints[0] != "要点一" {
		t.Errorf("answers[0].keyPoints = %v", a0.KeyPoints)
	}
	a1 := detail.Answers[1]
	if a1.UserAnswer != nil {
		t.Errorf("answers[1] 未回答 userAnswer 应为 null, got %q", *a1.UserAnswer)
	}
	if a1.ReferenceAnswer == nil || *a1.ReferenceAnswer != "参考二" {
		t.Errorf("answers[1].referenceAnswer = %v, want 参考二", a1.ReferenceAnswer)
	}
	if a1.KeyPoints == nil {
		t.Errorf("answers[1].keyPoints 应保留参考答案里的空列表, got nil")
	}
}

func TestGetEvaluationDetailEmpty(t *testing.T) {
	f := newEvalChainFixture()
	f.store.addSession(evalChainSession(5, evalPtr(VoiceEvaluateCompleted)))
	if err := f.svc.GenerateEvaluation(context.Background(), 5); err != nil {
		t.Fatalf("空评估生成失败: %v", err)
	}
	detail, err := f.svc.GetEvaluationDetail(context.Background(), 5)
	if err != nil {
		t.Fatalf("读取空评估明细失败: %v", err)
	}
	if detail.TotalQuestions != 0 {
		t.Errorf("空评估 totalQuestions = %d, want 0", detail.TotalQuestions)
	}
	if len(detail.Answers) != 0 {
		t.Errorf("空评估 answers 应为空列表, got %+v", detail.Answers)
	}
	if len(detail.Improvements) != 1 || detail.Improvements[0] != "请先完成至少一轮有效问答后再生成评估。" {
		t.Errorf("空评估 improvements 逐字不符: %v", detail.Improvements)
	}
	if detail.OverallScore != nil {
		t.Errorf("空评估 overallScore 应为 null, got %v", *detail.OverallScore)
	}
}

func TestGetEvaluationDetailNotFound(t *testing.T) {
	f := newEvalChainFixture()
	_, err := f.svc.GetEvaluationDetail(context.Background(), 5)
	var ae *apperr.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("应返回业务错误, got %v", err)
	}
	if ae.Code != apperr.CodeVoiceEvaluationNotFound {
		t.Errorf("错误码 = %d, want 10006", ae.Code)
	}
	if ae.Message != "评估结果不存在: 5" {
		t.Errorf("错误消息 = %q, want %q", ae.Message, "评估结果不存在: 5")
	}
}

// ===== evaluate_error 截断（列宽 500，Java truncateError）=====

func TestTruncateVoiceErrMsgLimitsTo500Runes(t *testing.T) {
	long := make([]rune, 600)
	for i := range long {
		long[i] = '错'
	}
	got := truncateVoiceErrMsg(string(long))
	if len([]rune(got)) != 500 {
		t.Errorf("截断后应剩 500 字符, got %d", len([]rune(got)))
	}
	if truncateVoiceErrMsg("") != "" {
		t.Error("空串应原样返回")
	}
	if truncateVoiceErrMsg("短错误") != "短错误" {
		t.Error("未超长应原样返回")
	}
}
