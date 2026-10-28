// saveReport 列映射单测（票 06）：报告 → interview_sessions 各 JSON 列 / 逐题回填
// interview_answers。契约来源：笔记 06 §5 逐字（InterviewPersistenceService.saveReport）。
package interview

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 契约：报告 → 会话列映射逐列核对——overall_score、overall_feedback、strengths_json、
// improvements_json、reference_answers_json（各 JSON 列为 JSON 数组文本）、
// status 强制 EVALUATED、completed_at=now（saveReport 即使异步链路进来也置 EVALUATED）。
func TestReportSessionColumnsMapping(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	columns, err := reportSessionColumns(evalSampleReport(), now)
	if err != nil {
		t.Fatalf("构建列映射失败: %v", err)
	}

	if got := columns["overall_score"]; got != 70 {
		t.Fatalf("overall_score = %v, want 70", got)
	}
	if got := columns["overall_feedback"]; got != "综合评价" {
		t.Fatalf("overall_feedback = %v", got)
	}
	if got := columns["status"]; got != StatusEvaluated {
		t.Fatalf("status 应强制 EVALUATED: %v", got)
	}
	if got := columns["completed_at"]; got != now {
		t.Fatalf("completed_at 应取 now: %v", got)
	}

	assertJSONArray(t, columns["strengths_json"], []string{"优势1"})
	assertJSONArray(t, columns["improvements_json"], []string{"改进1"})

	var refs []ReferenceAnswerDTO
	if err := json.Unmarshal([]byte(columns["reference_answers_json"].(string)), &refs); err != nil {
		t.Fatalf("reference_answers_json 不是合法 JSON: %v", err)
	}
	if len(refs) != 2 || refs[0].ReferenceAnswer != "参考1" || len(refs[0].KeyPoints) != 2 {
		t.Fatalf("reference_answers_json 映射不符: %+v", refs)
	}
}

// 契约：strengths/improvements 为空时落 JSON 空数组（报告列表恒非 null）。
func TestReportSessionColumnsEmptyLists(t *testing.T) {
	report := evalSampleReport()
	report.Strengths = nil
	report.Improvements = nil
	report.ReferenceAnswers = nil
	columns, err := reportSessionColumns(report, time.Now())
	if err != nil {
		t.Fatalf("构建列映射失败: %v", err)
	}
	for _, key := range []string{"strengths_json", "improvements_json", "reference_answers_json"} {
		if got := columns[key]; got != "[]" {
			t.Fatalf("%s 应为空数组: %v", key, got)
		}
	}
}

// 契约：逐题回填计划（Java answersToSave 逐字）——
//   - 既有行：更新 score/feedback；refAns 存在写 reference_answer；keyPoints 非空才写
//     key_points_json（空列表不触碰该列）；不动 user_answer/question/category；
//   - 未回答/缺失题：新建行（Create），userAnswer 保持 NULL（标注未回答），
//     score/feedback 来自评估。
func TestPlanAnswerBackfill(t *testing.T) {
	report := evalSampleReport()
	existing := []AnswerEntity{
		{ID: 11, SessionID: 1, QuestionIndex: intPtr(0), Question: stringPtr("题目一"),
			UserAnswer: stringPtr("A1"), Score: intPtr(0)},
	}

	writes := planAnswerBackfill(report, existing, 1)
	if len(writes) != 2 {
		t.Fatalf("应有 2 条回填: %d", len(writes))
	}

	// 既有行更新：不新建，写评分列 + 参考答案 + 要点（keyPoints 非空）。
	upd := writes[0]
	if upd.Create {
		t.Fatal("既有行不应新建")
	}
	if upd.QuestionIndex != 0 || upd.Score != 80 || upd.Feedback != "不错" {
		t.Fatalf("评分列不符: %+v", upd)
	}
	if upd.ReferenceAnswer == nil || *upd.ReferenceAnswer != "参考1" {
		t.Fatalf("reference_answer 应写入: %+v", upd.ReferenceAnswer)
	}
	if upd.KeyPointsJSON == nil || !strings.Contains(*upd.KeyPointsJSON, "要点1") {
		t.Fatalf("key_points_json 应写入: %+v", upd.KeyPointsJSON)
	}

	// 未回答/缺失题：新建行 userAnswer=NULL（标注未回答）。
	create := writes[1]
	if !create.Create {
		t.Fatal("缺失题应新建行")
	}
	if create.QuestionIndex != 1 || create.Question != "题目二" || create.Category == nil || *create.Category != "MYSQL" {
		t.Fatalf("新建行题面/分类不符: %+v", create)
	}
	if create.Score != 60 || create.Feedback != "一般" {
		t.Fatalf("新建行评分列不符: %+v", create)
	}
	if create.ReferenceAnswer == nil || *create.ReferenceAnswer != "参考2" {
		t.Fatalf("新建行 reference_answer 应写入: %+v", create.ReferenceAnswer)
	}
	// keyPoints 为空 → 不写 key_points_json（Java 非空才写的逐字语义）。
	if create.KeyPointsJSON != nil {
		t.Fatalf("keyPoints 为空不应写 key_points_json: %+v", create.KeyPointsJSON)
	}
}

// 契约：无对应 refAns 的题不写 reference_answer/key_points_json 两列
// （Java `if (refAns != null)` 守卫）。
func TestPlanAnswerBackfillWithoutReference(t *testing.T) {
	report := evalSampleReport()
	report.ReferenceAnswers = report.ReferenceAnswers[:1] // 只留 questionIndex=0
	writes := planAnswerBackfill(report, nil, 1)
	if writes[1].ReferenceAnswer != nil || writes[1].KeyPointsJSON != nil {
		t.Fatalf("无 refAns 不应写参考列: %+v", writes[1])
	}
}

// 契约：saveReport 落库次序 = 先会话列（含 EVALUATED）再逐题回填；
// 会话不存在仅告警不落库（Java warn + return）；序列化失败不落库（Java catch）。
func TestSaveReportWrites(t *testing.T) {
	rec := &evalTestRecorder{}
	store := newFakeEvalStore(rec)
	session := evalTestSession(1, "s1", StatusCompleted, nil)
	session.QuestionsJSON = stringPtr(evalSampleQuestionsJSON(t))
	store.addSession(session)
	store.setAnswers(1, []AnswerEntity{
		{SessionID: 1, QuestionIndex: intPtr(0), Question: stringPtr("题目一"), UserAnswer: stringPtr("A1")},
	})

	if err := saveReport(context.Background(), store, discardLogger(), "s1", evalSampleReport()); err != nil {
		t.Fatalf("saveReport 失败: %v", err)
	}
	events := rec.list()
	if len(events) != 3 || events[0] != "report-columns" {
		t.Fatalf("应先写会话列再逐题回填: %v", events)
	}
	if len(store.answerWrites) != 2 {
		t.Fatalf("应逐题回填 2 行: %d", len(store.answerWrites))
	}

	// 会话不存在：无任何写入、无错误。
	missing := newFakeEvalStore(&evalTestRecorder{})
	if err := saveReport(context.Background(), missing, discardLogger(), "ghost", evalSampleReport()); err != nil {
		t.Fatalf("会话不存在应静默返回: %v", err)
	}
	if len(missing.reportWrites) != 0 || len(missing.answerWrites) != 0 {
		t.Fatal("会话不存在不应落库")
	}
}

// 契约：评估回填新建行 userAnswer 保持 NULL（未回答标注）、评分列来自评估。
func TestUpsertAnswerEvalCreateRowShape(t *testing.T) {
	w := answerEvalWrite{
		Create: true, SessionRowID: 9, QuestionIndex: 3, Question: "题目三",
		Category: stringPtr("JAVA"), Score: 0, Feedback: "未回答评估",
	}
	row := answerEvalInsertRow(w, time.Now())
	if row.UserAnswer != nil {
		t.Fatalf("新建行 userAnswer 应为 NULL: %+v", row.UserAnswer)
	}
	if row.SessionID != 9 || *row.QuestionIndex != 3 || *row.Score != 0 {
		t.Fatalf("新建行字段不符: %+v", row)
	}
}

// assertJSONArray 断言列值为 JSON 数组且解出期望内容。
func assertJSONArray(t *testing.T, raw any, want []string) {
	t.Helper()
	text, ok := raw.(string)
	if !ok {
		t.Fatalf("列值应为字符串: %T", raw)
	}
	var got []string
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("列值不是合法 JSON 数组: %q", text)
	}
	if len(got) != len(want) {
		t.Fatalf("数组长度不符: %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("数组内容不符: %v want %v", got, want)
		}
	}
}
