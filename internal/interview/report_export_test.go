// 报告 PDF 导出（票 08）单测：数据→视图模型映射（复用 details 语义）、
// 空值防御、状态中文映射、分类得分（票 05 计分语义），以及 service 层 3001。
package interview

import (
	"context"
	"testing"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

func exportEntity(status SessionStatus) *SessionEntity {
	e := evalTestSession(1, "s1", status, nil)
	e.OverallScore = intPtr(75)
	e.OverallFeedback = stringPtr("整体不错")
	e.StrengthsJSON = stringPtr(`["优势1","优势2"]`)
	e.ImprovementsJSON = stringPtr(`["改进1"]`)
	completed := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	e.CompletedAt = &completed
	return e
}

// 映射：逐题块字段、追问标记、空作答→nil/0、分类得分、状态文案、题数。
func TestBuildInterviewExportInputMapping(t *testing.T) {
	qs := []QuestionDTO{
		CreateFallback(0, "题目一", "java", "JAVA", nil, false, nil),
		CreateFallback(1, "题目二", "mysql", "MYSQL", nil, true, intPtr(0)), // 追问
	}
	entity := exportEntity(StatusEvaluated)
	answers := []AnswerEntity{
		{SessionID: 1, QuestionIndex: intPtr(0), Question: stringPtr("题目一"), Category: stringPtr("JAVA"),
			UserAnswer: stringPtr("A1"), Score: intPtr(80), Feedback: stringPtr("不错"),
			ReferenceAnswer: stringPtr("参考1"), KeyPointsJSON: stringPtr(`["要点A","要点B"]`)},
		// 第 2 题未作答 → buildAnswerDetailList 构造空记录。
	}

	in := buildInterviewExportInput(entity, qs, answers)

	if in.SessionID != "s1" || in.TotalQuestions != 2 || in.StatusText != "已评估" {
		t.Fatalf("头部不符: %+v", in)
	}
	if in.OverallScore == nil || *in.OverallScore != 75 {
		t.Fatalf("总分不符: %+v", in.OverallScore)
	}
	if in.OverallFeedback != "整体不错" || len(in.Strengths) != 2 || len(in.Improvements) != 1 {
		t.Fatalf("总评/优势/改进不符: %+v", in)
	}
	if in.CompletedAt == nil {
		t.Fatalf("完成时间应保留")
	}

	if len(in.Questions) != 2 {
		t.Fatalf("应有 2 题块: %d", len(in.Questions))
	}
	q1 := in.Questions[0]
	if q1.Number != 1 || q1.Category != "JAVA" || q1.UserAnswer == nil || *q1.UserAnswer != "A1" ||
		q1.Score != 80 || q1.Feedback != "不错" || q1.ReferenceAnswer != "参考1" || len(q1.KeyPoints) != 2 {
		t.Fatalf("已作答题块不符: %+v", q1)
	}
	if q1.IsFollowUp {
		t.Fatalf("题 1 不应是追问: %+v", q1)
	}
	q2 := in.Questions[1]
	if q2.Number != 2 || q2.Category != "MYSQL" || q2.UserAnswer != nil || q2.Score != 0 {
		t.Fatalf("未作答题块不符（应 nil/0 分）: %+v", q2)
	}
	if !q2.IsFollowUp {
		t.Fatalf("题 2 应标追问: %+v", q2)
	}
	if q2.ReferenceAnswer != "" || q2.Feedback != "" || len(q2.KeyPoints) != 0 {
		t.Fatalf("未作答题块字段应缺省: %+v", q2)
	}

	// 分类得分（票 05 语义：分类平均 int 截断，按分类名排序）：JAVA 80、MYSQL 0。
	if len(in.CategoryScores) != 2 {
		t.Fatalf("分类得分应 2 项: %+v", in.CategoryScores)
	}
	if in.CategoryScores[0].Category != "JAVA" || in.CategoryScores[0].Score != 80 ||
		in.CategoryScores[1].Category != "MYSQL" || in.CategoryScores[1].Score != 0 {
		t.Fatalf("分类得分不符: %+v", in.CategoryScores)
	}
}

// 状态中文映射（Java getStatusText 逐字）。
func TestStatusTextOf(t *testing.T) {
	cases := map[SessionStatus]string{
		StatusCreated: "已创建", StatusInProgress: "进行中",
		StatusCompleted: "已完成", StatusEvaluated: "已评估",
	}
	for status, want := range cases {
		if got := statusTextOf(&status); got != want {
			t.Fatalf("状态 %s → %q，应 %q", status, got, want)
		}
	}
	if got := statusTextOf(nil); got != "" {
		t.Fatalf("nil 状态应空串: %q", got)
	}
}

// 空值防御：无题目 JSON、无作答、无评估列 → 不崩溃、字段缺省。
func TestBuildInterviewExportInputEmptyDefense(t *testing.T) {
	entity := &SessionEntity{SessionID: "s0", Status: func() *SessionStatus { s := StatusCompleted; return &s }()}
	in := buildInterviewExportInput(entity, nil, nil)
	if in.SessionID != "s0" || len(in.Questions) != 0 {
		t.Fatalf("空输入不符: %+v", in)
	}
	if in.OverallScore != nil || in.OverallFeedback != "" || len(in.Strengths) != 0 ||
		len(in.Improvements) != 0 || len(in.CategoryScores) != 0 {
		t.Fatalf("空输入应字段缺省: %+v", in)
	}
	if in.StatusText != "已完成" {
		t.Fatalf("状态文案不符: %q", in.StatusText)
	}
	if in.CreatedAt.IsZero() && in.CompletedAt != nil {
		t.Fatalf("时间字段处理异常")
	}
}

// service：场次不存在 → 3001；存在 → 返回视图模型。
func TestGetInterviewExportInput(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	svc := NewReportService(store, nil, nil, nil, discardLogger())

	// 不存在 → 3001。
	_, err := svc.GetInterviewExportInput(context.Background(), "missing")
	if err == nil {
		t.Fatalf("不存在应报错")
	}
	if ae := apperr.From(err); ae.Code != apperr.CodeInterviewSessionNotFound {
		t.Fatalf("应 3001: %+v", ae)
	}

	// 存在（COMPLETED/未评估）→ 正常返回，不触发评估、不报错（Java 对齐）。
	entity := exportEntity(StatusCompleted)
	entity.OverallScore = nil // 未评估 → 无总分
	entity.QuestionsJSON = strPtr(evalSampleQuestionsJSON(t))
	store.addSession(entity)
	store.setAnswers(1, nil)

	in, err := svc.GetInterviewExportInput(context.Background(), "s1")
	if err != nil {
		t.Fatalf("导出输入获取失败: %v", err)
	}
	if in.SessionID != "s1" {
		t.Fatalf("sessionId 不符: %+v", in)
	}
	if in.OverallScore != nil {
		t.Fatalf("未评估应无总分（跳过综合评分）: %+v", in.OverallScore)
	}
	if len(in.Questions) != 2 {
		t.Fatalf("未作答应仍返回全部题目块: %d", len(in.Questions))
	}
}
