// report / details 端点单测（票 06）：report 三分支（存量返回 / 同步兜底 / 状态错误，
// 裁决 #2）与 details 未答题空记录、answers 按题号排序（笔记 07 逐字）。
package interview

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wuzuxian/interview/internal/apperr"
)

// 契约（裁决 #2）：evaluate_status=COMPLETED 直接返回存量报告，绝不重调 LLM——
// 总分/总评语/优势/改进/参考答案取 interview_sessions 各 JSON 列，逐题取
// questions_json + interview_answers 回填列，分类得分按票 05 计分语义重算。
func TestGetReportReturnsStoredWithoutLLM(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	runner := &stubEvalRunner{report: evalSampleReport()}
	svc := NewReportService(store, runner, &fakeRefSource{}, nil, discardLogger())

	completed := EvaluateCompleted
	session := evalTestSession(1, "s1", StatusCompleted, &completed)
	session.QuestionsJSON = stringPtr(evalSampleQuestionsJSON(t))
	session.OverallScore = intPtr(70)
	session.OverallFeedback = stringPtr("综合评价")
	session.StrengthsJSON = stringPtr(`["优势1"]`)
	session.ImprovementsJSON = stringPtr(`["改进1"]`)
	session.ReferenceAnswersJSON = stringPtr(`[{"questionIndex":0,"question":"题目一","referenceAnswer":"参考1","keyPoints":["要点1"]}]`)
	store.addSession(session)
	store.setAnswers(1, []AnswerEntity{
		{SessionID: 1, QuestionIndex: intPtr(0), Question: stringPtr("题目一"), UserAnswer: stringPtr("A1"),
			Score: intPtr(80), Feedback: stringPtr("不错"), ReferenceAnswer: stringPtr("参考1")},
		// questionIndex=1 未回答：无作答行（未答题 score=0 且标注未回答）。
	})

	report, err := svc.GetReport(context.Background(), "s1")
	if err != nil {
		t.Fatalf("GetReport 失败: %v", err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("存量报告不应重调 LLM: %d", runner.callCount())
	}
	if report.SessionID != "s1" || report.OverallScore != 70 || report.OverallFeedback != "综合评价" {
		t.Fatalf("存量报告头部不符: %+v", report)
	}
	if report.TotalQuestions != 2 {
		t.Fatalf("totalQuestions 应为题目数: %d", report.TotalQuestions)
	}
	if len(report.QuestionDetails) != 2 {
		t.Fatalf("逐题明细应覆盖全部题目: %+v", report.QuestionDetails)
	}
	if report.QuestionDetails[0].UserAnswer == nil || *report.QuestionDetails[0].UserAnswer != "A1" ||
		report.QuestionDetails[0].Score != 80 || report.QuestionDetails[0].Feedback != "不错" {
		t.Fatalf("已答题明细不符: %+v", report.QuestionDetails[0])
	}
	if report.QuestionDetails[1].UserAnswer != nil || report.QuestionDetails[1].Score != 0 {
		t.Fatalf("未答题应 userAnswer=null、score=0: %+v", report.QuestionDetails[1])
	}
	// 分类得分重算：JAVA 80 分 1 题、MYSQL 0 分 1 题（按分类名稳定排序）。
	if len(report.CategoryScores) != 2 {
		t.Fatalf("分类得分不符: %+v", report.CategoryScores)
	}
	if report.CategoryScores[0].Category != "JAVA" || report.CategoryScores[0].Score != 80 {
		t.Fatalf("JAVA 分类得分不符: %+v", report.CategoryScores[0])
	}
	if report.CategoryScores[1].Category != "MYSQL" || report.CategoryScores[1].Score != 0 {
		t.Fatalf("MYSQL 分类得分不符: %+v", report.CategoryScores[1])
	}
	if len(report.ReferenceAnswers) != 1 || report.ReferenceAnswers[0].ReferenceAnswer != "参考1" {
		t.Fatalf("参考答案应取存量列: %+v", report.ReferenceAnswers)
	}
	if len(report.Strengths) != 1 || report.Strengths[0] != "优势1" {
		t.Fatalf("优势应取存量列: %+v", report.Strengths)
	}
}

// 契约（裁决 #2）：场次 EVALUATED（异步链路已置）同样直接返回存量报告，不重调 LLM。
func TestGetReportEvaluatedReturnsStored(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	runner := &stubEvalRunner{report: evalSampleReport()}
	svc := NewReportService(store, runner, nil, nil, discardLogger())

	failed := EvaluateFailed // 评估列异常也不影响存量返回判定（场次状态优先）
	session := evalTestSession(1, "s1", StatusEvaluated, &failed)
	session.QuestionsJSON = stringPtr(evalSampleQuestionsJSON(t))
	store.addSession(session)

	if _, err := svc.GetReport(context.Background(), "s1"); err != nil {
		t.Fatalf("GetReport 失败: %v", err)
	}
	if runner.callCount() != 0 {
		t.Fatalf("EVALUATED 场次不应重调 LLM: %d", runner.callCount())
	}
}

// 契约：场次 COMPLETED 但未评估 → 同步兜底评估（generateReport 语义，笔记 05 §8）：
// 同步跑引擎（输入取 DB 重放：作答覆盖、简历文本取自 resumes 关联）→ 缓存状态置
// EVALUATED → saveReport 落库 → 返回新报告。
func TestGetReportSyncFallback(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	runner := &stubEvalRunner{report: evalSampleReport()}
	refs := &fakeRefSource{text: "参考段"}
	status := &fakeStatusUpdater{}
	svc := NewReportService(store, runner, refs, status, discardLogger())

	session := evalTestSession(1, "s1", StatusCompleted, nil) // 未评估
	session.QuestionsJSON = stringPtr(evalSampleQuestionsJSON(t))
	store.addSession(session)
	store.setAnswers(1, []AnswerEntity{
		{SessionID: 1, QuestionIndex: intPtr(0), UserAnswer: stringPtr("A1")},
	})
	store.resumes[7] = "简历正文"

	report, err := svc.GetReport(context.Background(), "s1")
	if err != nil {
		t.Fatalf("GetReport 失败: %v", err)
	}
	if runner.callCount() != 1 {
		t.Fatalf("同步兜底应恰调一次引擎: %d", runner.callCount())
	}
	if runner.lastInput.ResumeText != "简历正文" {
		t.Fatalf("简历文本应取自 DB 关联: %q", runner.lastInput.ResumeText)
	}
	if runner.lastInput.Questions[0].UserAnswer == nil || *runner.lastInput.Questions[0].UserAnswer != "A1" {
		t.Fatalf("作答应从 DB 重放: %+v", runner.lastInput.Questions)
	}
	if runner.lastInput.SkillReference != "参考段" || refs.lastSkill != "java-backend" {
		t.Fatalf("参考段注入不符: %q / %s", runner.lastInput.SkillReference, refs.lastSkill)
	}
	if len(status.calls) != 1 || status.calls[0] != StatusEvaluated {
		t.Fatalf("缓存状态应置 EVALUATED: %v", status.calls)
	}
	if len(store.reportWrites) != 1 {
		t.Fatalf("saveReport 应落库: %d", len(store.reportWrites))
	}
	if report == nil || report.OverallScore != 70 {
		t.Fatalf("应返回新报告: %+v", report)
	}
	events := rec.list()
	if events[0] != "report-columns" {
		t.Fatalf("应先写会话列: %v", events)
	}
}

// 契约（经验文档教训，有意偏离 Java 的仅告警）：同步兜底评估的报告保存失败必须
// 上抛 error——失败被告警吞掉会绕过重试/FAILED 容错链路，把失败固化为假完成。
func TestGetReportSyncSaveFailureReturnsError(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	store.failReportCol = errors.New("db down")
	runner := &stubEvalRunner{report: evalSampleReport()}
	svc := NewReportService(store, runner, nil, nil, discardLogger())

	session := evalTestSession(1, "s1", StatusCompleted, nil) // 未评估 → 同步兜底
	session.QuestionsJSON = stringPtr(evalSampleQuestionsJSON(t))
	store.addSession(session)

	report, err := svc.GetReport(context.Background(), "s1")
	if err == nil {
		t.Fatal("报告保存失败应返回 error 而非仅告警")
	}
	if report != nil {
		t.Errorf("error 时不应返回报告: %+v", report)
	}
	if ae := apperr.From(err); ae.Code != apperr.CodeInternalError {
		t.Fatalf("保存失败应报内部错误: %+v", ae)
	}
}

// 契约（笔记 08 §2.6）：其余状态 → 3007 "面试尚未完成，无法生成报告"；
// 场次不存在 → 3001；引擎失败透传业务错误（3005 由执行器归一）。
func TestGetReportErrorBranches(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	runner := &stubEvalRunner{report: evalSampleReport()}
	svc := NewReportService(store, runner, nil, nil, discardLogger())

	// 未完成（CREATED / IN_PROGRESS）→ 3007。
	for _, status := range []SessionStatus{StatusCreated, StatusInProgress} {
		store.sessions = map[string]*SessionEntity{}
		store.addSession(evalTestSession(1, "s1", status, nil))
		_, err := svc.GetReport(context.Background(), "s1")
		ae := apperr.From(err)
		if ae.Code != apperr.CodeInterviewNotCompleted || ae.Message != "面试尚未完成，无法生成报告" {
			t.Fatalf("状态 %s 应报 3007: %+v", status, ae)
		}
	}

	// 场次不存在 → 3001。
	_, err := svc.GetReport(context.Background(), "ghost")
	if ae := apperr.From(err); ae.Code != apperr.CodeInterviewSessionNotFound {
		t.Fatalf("不存在应报 3001: %+v", ae)
	}

	// 同步兜底引擎失败 → 业务错误透传。
	store.sessions = map[string]*SessionEntity{}
	store.addSession(evalTestSession(1, "s1", StatusCompleted, nil))
	runner.err = apperr.New(apperr.CodeInterviewEvaluationFailed, "面试评估失败")
	_, err = svc.GetReport(context.Background(), "s1")
	if ae := apperr.From(err); ae.Code != apperr.CodeInterviewEvaluationFailed {
		t.Fatalf("引擎失败应透传 3005: %+v", ae)
	}
}

// 契约（笔记 07 buildAnswerDetailList 逐字）：details 对每道题都生成明细项，
// 未答题生成空记录——userAnswer/referenceAnswer/keyPoints/answeredAt 为 null、
// score 取题目 score（缺省 0）、feedback 取题目 feedback；answers 按题号排序。
func TestGetInterviewDetailUnansweredEmptyRecord(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	svc := NewReportService(store, &stubEvalRunner{}, nil, nil, discardLogger())

	completed := EvaluateCompleted
	session := evalTestSession(7, "s1", StatusCompleted, &completed)
	questions := []QuestionDTO{
		CreateFallback(0, "题目一", "JAVA", "Java", nil, false, nil),
		CreateFallback(1, "题目二", "MYSQL", "MySQL", nil, false, nil),
		CreateFallback(2, "题目三", "JAVA", "Java", nil, false, nil),
	}
	raw, err := SerializeQuestions(questions)
	if err != nil {
		t.Fatalf("序列化题目失败: %v", err)
	}
	session.QuestionsJSON = &raw
	session.StrengthsJSON = stringPtr(`["优势1"]`)
	store.addSession(session)
	answeredAt := session.CreatedAt
	store.setAnswers(7, []AnswerEntity{
		{SessionID: 7, QuestionIndex: intPtr(0), Question: stringPtr("题目一"), Category: stringPtr("Java"),
			UserAnswer: stringPtr("A1"), Score: intPtr(80), Feedback: stringPtr("不错"),
			ReferenceAnswer: stringPtr("参考1"), KeyPointsJSON: stringPtr(`["要点1"]`), AnsweredAt: answeredAt},
		{SessionID: 7, QuestionIndex: intPtr(2), Question: stringPtr("题目三"), Category: stringPtr("Java"),
			UserAnswer: stringPtr("A3"), Score: intPtr(0), AnsweredAt: answeredAt},
	})

	detail, err := svc.GetInterviewDetail(context.Background(), "s1")
	if err != nil {
		t.Fatalf("GetInterviewDetail 失败: %v", err)
	}
	if detail.ID != 7 || detail.SessionID != "s1" {
		t.Fatalf("头部字段不符: %+v", detail)
	}
	if detail.Status == nil || *detail.Status != "COMPLETED" ||
		detail.EvaluateStatus == nil || *detail.EvaluateStatus != "COMPLETED" {
		t.Fatalf("状态字段不符: %+v", detail)
	}
	if len(detail.Answers) != 3 {
		t.Fatalf("每题都应有明细项: %+v", detail.Answers)
	}

	// 已答题：作答表字段直出（含 keyPoints 解析、answeredAt）。
	first := detail.Answers[0]
	if first.UserAnswer == nil || *first.UserAnswer != "A1" || *first.Score != 80 ||
		first.ReferenceAnswer == nil || *first.ReferenceAnswer != "参考1" ||
		first.KeyPoints == nil || first.KeyPoints[0] != "要点1" || first.AnsweredAt == nil {
		t.Fatalf("已答题明细不符: %+v", first)
	}

	// 未答题（questionIndex=1）：空记录——标注未回答（userAnswer=null）且 score=0。
	empty := detail.Answers[1]
	if empty.QuestionIndex == nil || *empty.QuestionIndex != 1 {
		t.Fatalf("未答题题号不符: %+v", empty)
	}
	if empty.Question == nil || *empty.Question != "题目二" {
		t.Fatalf("未答题题面应取题目: %+v", empty)
	}
	if empty.UserAnswer != nil {
		t.Fatalf("未答题 userAnswer 应为 null: %+v", empty.UserAnswer)
	}
	if empty.Score == nil || *empty.Score != 0 {
		t.Fatalf("未答题 score 应为 0: %+v", empty.Score)
	}
	if empty.ReferenceAnswer != nil || empty.KeyPoints != nil || empty.AnsweredAt != nil {
		t.Fatalf("未答题参考答案/要点/时间应为 null: %+v", empty)
	}

	// answers 按题号排序。
	for i := 1; i < len(detail.Answers); i++ {
		if *detail.Answers[i-1].QuestionIndex >= *detail.Answers[i].QuestionIndex {
			t.Fatalf("answers 应按题号升序: %+v", detail.Answers)
		}
	}

	// questions / strengths 原样透传。
	var rawQuestions []map[string]any
	if err := json.Unmarshal(detail.Questions, &rawQuestions); err != nil || len(rawQuestions) != 3 {
		t.Fatalf("questions 列应原样透传: %s", detail.Questions)
	}
	if len(detail.Strengths) != 1 || detail.Strengths[0] != "优势1" {
		t.Fatalf("strengths 解析不符: %+v", detail.Strengths)
	}
}

// 契约（笔记 07）：questions 列为空时回退到仅显示已回答的题目；不存在 → 3001。
func TestGetInterviewDetailFallbackAndNotFound(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	svc := NewReportService(store, &stubEvalRunner{}, nil, nil, discardLogger())

	session := evalTestSession(7, "s1", StatusCompleted, nil) // questions_json 为空
	store.addSession(session)
	store.setAnswers(7, []AnswerEntity{
		{SessionID: 7, QuestionIndex: intPtr(1), Question: stringPtr("题目B"), UserAnswer: stringPtr("B")},
		{SessionID: 7, QuestionIndex: intPtr(0), Question: stringPtr("题目A"), UserAnswer: stringPtr("A")},
	})
	detail, err := svc.GetInterviewDetail(context.Background(), "s1")
	if err != nil {
		t.Fatalf("GetInterviewDetail 失败: %v", err)
	}
	if len(detail.Answers) != 2 || *detail.Answers[0].QuestionIndex != 0 {
		t.Fatalf("回退路径应只含已答题且按题号排序: %+v", detail.Answers)
	}

	if _, err := svc.GetInterviewDetail(context.Background(), "ghost"); err == nil {
		t.Fatal("不存在应报 3001")
	} else if ae := apperr.From(err); ae.Code != apperr.CodeInterviewSessionNotFound {
		t.Fatalf("不存在应报 3001: %+v", ae)
	}
}
