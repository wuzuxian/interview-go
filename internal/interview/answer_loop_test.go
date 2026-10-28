// 逐题问答闭环纯逻辑单测（spec 测试决策：只测外部行为契约，DB/HTTP 层不写自动化集成测试）。
// 覆盖：状态机流转（CREATED→IN_PROGRESS→COMPLETED 单向）、末题判定、暂存/提交索引守卫、
// 作答表 upsert 语义、completed 分支 DTO 契约、非法状态与非法索引错误码（笔记 05/08 逐字）。
package interview

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wuzuxian/interview/internal/apperr"
)

// ===== 状态机流转 =====

// 契约：取题只把 CREATED 推进为 IN_PROGRESS（首次取题），其余状态原样保留——
// 状态机单向 CREATED → IN_PROGRESS → COMPLETED → EVALUATED，取题不回退、不跳级。
func TestQuestionViewTransition(t *testing.T) {
	cases := []struct {
		name      string
		in        SessionStatus
		want      SessionStatus
		wantFired bool
	}{
		{"CREATED 首次取题推进 IN_PROGRESS", StatusCreated, StatusInProgress, true},
		{"IN_PROGRESS 重复取题不推进", StatusInProgress, StatusInProgress, false},
		{"COMPLETED 取题不推进", StatusCompleted, StatusCompleted, false},
		{"EVALUATED 取题不推进", StatusEvaluated, StatusEvaluated, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fired := questionViewTransition(tc.in)
			if got != tc.want || fired != tc.wantFired {
				t.Fatalf("questionViewTransition(%s) = (%s, %v), want (%s, %v)",
					tc.in, got, fired, tc.want, tc.wantFired)
			}
		})
	}
}

// 契约：提前交卷仅允许未完成状态（CREATED/IN_PROGRESS，笔记 05 §7 逐字）；
// COMPLETED/EVALUATED 再交卷 → 3004 "面试已完成"。
func TestCompleteStartAllowed(t *testing.T) {
	cases := []struct {
		name   string
		status SessionStatus
		want   bool
	}{
		{"CREATED 可交卷", StatusCreated, true},
		{"IN_PROGRESS 可交卷", StatusInProgress, true},
		{"COMPLETED 已完成拒绝", StatusCompleted, false},
		{"EVALUATED 已评估拒绝", StatusEvaluated, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := completeStartAllowed(tc.status); got != tc.want {
				t.Fatalf("completeStartAllowed(%s) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}

	ae := apperr.New(apperr.CodeInterviewAlreadyCompleted, "面试已完成")
	if ae.Code != 3004 || ae.Message != "面试已完成" {
		t.Fatalf("重复交卷错误码不符: [%d %s], want [3004 面试已完成]", ae.Code, ae.Message)
	}
}

// 契约：全流程状态机推演——CREATED 起步，首次取题转 IN_PROGRESS，
// 逐题提交后恰在末题转 COMPLETED；此后取题命中 completed 分支、交卷被拒。
// （用纯函数串起服务层的实际推进路径，与 GetCurrentQuestion/SubmitAnswer 内部逻辑同源。）
func TestStateMachineFullFlow(t *testing.T) {
	questions := GenerateFallbackQuestions(nil, 3, 0)
	status := StatusCreated
	currentIndex := 0

	// 第一次取题：CREATED → IN_PROGRESS。
	next, fired := questionViewTransition(status)
	if !fired || next != StatusInProgress {
		t.Fatalf("首次取题应推进 IN_PROGRESS: got (%s, %v)", next, fired)
	}
	status = next

	for i := 0; i < len(questions); i++ {
		if status != StatusInProgress {
			t.Fatalf("第 %d 题作答时应为 IN_PROGRESS: %s", i, status)
		}
		if err := validateAnswerIndex(i, len(questions)); err != nil {
			t.Fatalf("合法索引不应报错: %v", err)
		}
		hasNext, _, newIndex, newStatus := submitOutcome(questions, i)
		if newIndex != i+1 {
			t.Fatalf("newIndex 应为 index+1: got %d want %d", newIndex, i+1)
		}
		if hasNext != (newIndex < len(questions)) {
			t.Fatalf("hasNext 判定不符 at %d", i)
		}
		if newStatus == StatusCompleted {
			status = newStatus // 末题提交自动交卷
		}
		currentIndex = newIndex
	}

	if status != StatusCompleted || currentIndex != len(questions) {
		t.Fatalf("末题提交后应 COMPLETED 且 currentIndex==len: got (%s, %d)", status, currentIndex)
	}
	if completeStartAllowed(status) {
		t.Fatal("COMPLETED 后提前交卷应被拒绝")
	}
}

// ===== 末题判定（裁决 #4：不校验 index==currentIndex，newIndex = index+1）=====

// 契约：非末题 → hasNext=true、next 指向 index+1、状态 IN_PROGRESS；
// 末题 → hasNext=false、next=nil、状态 COMPLETED（自动交卷）。
func TestSubmitOutcomeLastQuestionDetection(t *testing.T) {
	questions := GenerateFallbackQuestions(nil, 3, 0)
	cases := []struct {
		name          string
		index         int
		wantHasNext   bool
		wantNextIndex int // -1 表示 next 为 nil
		wantNewIndex  int
		wantStatus    SessionStatus
	}{
		{"首题提交还有下题", 0, true, 1, 1, StatusInProgress},
		{"中间题提交还有下题", 1, true, 2, 2, StatusInProgress},
		{"末题提交自动交卷", 2, false, -1, 3, StatusCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hasNext, next, newIndex, newStatus := submitOutcome(questions, tc.index)
			if hasNext != tc.wantHasNext || newIndex != tc.wantNewIndex || newStatus != tc.wantStatus {
				t.Fatalf("submitOutcome(index=%d) = (%v, %v, %d, %s)",
					tc.index, hasNext, next != nil, newIndex, newStatus)
			}
			if tc.wantNextIndex == -1 {
				if next != nil {
					t.Fatalf("末题 next 应为 nil: %+v", next)
				}
			} else if next == nil || next.QuestionIndex != tc.wantNextIndex {
				t.Fatalf("next 应指向第 %d 题: %+v", tc.wantNextIndex, next)
			}
		})
	}

	// 单题场次：提交唯一一题即完成。
	single := GenerateFallbackQuestions(nil, 1, 0)
	hasNext, next, newIndex, newStatus := submitOutcome(single, 0)
	if hasNext || next != nil || newIndex != 1 || newStatus != StatusCompleted {
		t.Fatalf("单题场次应直接 COMPLETED: (%v, %v, %d, %s)", hasNext, next, newIndex, newStatus)
	}
}

// ===== 索引守卫与请求体校验 =====

// 契约：索引越界（<0 或 >=len）→ 3003 "无效的问题索引: N"，消息携带原值（逐字）。
func TestValidateAnswerIndex(t *testing.T) {
	cases := []struct {
		name    string
		index   int
		total   int
		wantErr bool
		wantMsg string
	}{
		{"首题合法", 0, 3, false, ""},
		{"末题合法", 2, 3, false, ""},
		{"负数非法", -1, 3, true, "无效的问题索引: -1"},
		{"越上界非法", 3, 3, true, "无效的问题索引: 3"},
		{"空场次任何索引非法", 0, 0, true, "无效的问题索引: 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAnswerIndex(tc.index, tc.total)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，got nil")
				}
				ae := apperr.From(err)
				if ae.Code != apperr.CodeInterviewQuestionNotFound || ae.Message != tc.wantMsg {
					t.Fatalf("错误不符: [%d %s], want [3003 %s]", ae.Code, ae.Message, tc.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("不期望报错: %v", err)
			}
		})
	}
}

// 契约：questionIndex 缺失 → 400 显式校验（Java 为 NPE→500，笔记 08 §2.5 建议 Go 显式校验）；
// 提供时取值透传。
func TestAnswerIndexOf(t *testing.T) {
	idx := 2
	got, err := answerIndexOf(AnswerRequest{QuestionIndex: &idx})
	if err != nil || got != 2 {
		t.Fatalf("合法 index 透传不符: (%d, %v)", got, err)
	}

	_, err = answerIndexOf(AnswerRequest{})
	if err == nil {
		t.Fatal("缺失 questionIndex 应报错")
	}
	ae := apperr.From(err)
	if ae.Code != apperr.CodeBadRequest || ae.Message != "questionIndex 不能为空" {
		t.Fatalf("缺失 questionIndex 错误不符: [%d %s]", ae.Code, ae.Message)
	}
}

// ===== 作答表 upsert 语义（验收：重复暂存/提交不产生重复行，answered_at 更新）=====

// 契约（新建分支）：score 恒 0、feedback 空、question_index 与场次内部 ID 就位、
// answered_at 取写入时刻——对齐 Java saveAnswer(..., 0, null)。
func TestAnswerRowForInsert(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local)
	answer := "我的答案"
	category := "综合能力"
	row := answerRowForInsert(77, 3, "题目原文", &category, &answer, now)

	if row.SessionID != 77 || row.QuestionIndex == nil || *row.QuestionIndex != 3 {
		t.Fatalf("新建行定位字段不符: sessionId=%d index=%v", row.SessionID, row.QuestionIndex)
	}
	if row.Score == nil || *row.Score != 0 {
		t.Fatalf("score 应为 0（评估时才回填）: %v", row.Score)
	}
	if row.Feedback != nil {
		t.Fatalf("feedback 应为空: %v", row.Feedback)
	}
	if row.Question == nil || *row.Question != "题目原文" || row.UserAnswer != &answer || row.Category != &category {
		t.Fatalf("作答内容字段不符: %+v", row)
	}
	if !row.AnsweredAt.Equal(now) {
		t.Fatalf("answered_at 应为写入时刻: %v", row.AnsweredAt)
	}
}

// 契约（更新分支）：只刷新作答内容与 answered_at，列集合不含 id/session_id/question_index
// ——upsert 命中既有行时不会改写定位列，也就不会产生新行。
func TestAnswerUpdateColumns(t *testing.T) {
	first := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local)
	second := first.Add(time.Minute)
	answer := "更新后的答案"

	cols := answerUpdateColumns("题目原文", nil, &answer, second)

	for _, k := range []string{"question", "category", "user_answer", "score", "feedback", "answered_at"} {
		if _, ok := cols[k]; !ok {
			t.Fatalf("更新列缺 %q: %v", k, cols)
		}
	}
	for _, k := range []string{"id", "session_id", "question_index"} {
		if _, ok := cols[k]; ok {
			t.Fatalf("更新列不应触碰定位列 %q: %v", k, cols)
		}
	}
	gotScore, _ := cols["score"].(*int)
	if gotScore == nil || *gotScore != 0 {
		t.Fatalf("重复提交应把 score 重置回 0（对齐 Java setScore(0)）: %v", cols["score"])
	}
	if cols["feedback"] != nil {
		t.Fatalf("重复提交应清空 feedback: %v", cols["feedback"])
	}
	if !cols["answered_at"].(time.Time).Equal(second) {
		t.Fatalf("answered_at 应被刷新: %v", cols["answered_at"])
	}
}

// ===== completed 分支 DTO 契约（前端零改动切换的根基，笔记 05 §4 / 08 §2.4）=====

// 契约：未完成分支 JSON 恰为 {"completed": false, "question": {...}}——无 message 键；
// question 为完整 InterviewQuestionDTO 形状。
func TestCurrentQuestionResponseUncompletedContract(t *testing.T) {
	questions := GenerateFallbackQuestions(nil, 1, 1)
	resp := &CurrentQuestionResponse{Completed: false, Question: &questions[0]}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("未完成分支应恰有两个键: %v", m)
	}
	if v, ok := m["completed"]; !ok || v != false {
		t.Fatalf("completed 应为 false: %v", m["completed"])
	}
	q, ok := m["question"].(map[string]any)
	if !ok {
		t.Fatalf("question 应为对象: %v", m["question"])
	}
	// 完整 InterviewQuestionDTO 键集（14 个字段一次建全）。
	wantKeys := []string{
		"questionIndex", "question", "type", "category", "topicSummary", "userAnswer",
		"score", "feedback", "isFollowUp", "parentQuestionIndex", "referenceAnswer",
		"keyPoints", "scoringRubric", "sourceContext",
	}
	if len(q) != len(wantKeys) {
		t.Fatalf("question 键数量不符: got %d want %d", len(q), len(wantKeys))
	}
	for _, k := range wantKeys {
		if _, ok := q[k]; !ok {
			t.Fatalf("question 缺字段 %q", k)
		}
	}
	if _, has := m["message"]; has {
		t.Fatal("未完成分支不应有 message 键")
	}
}

// 契约：完成分支 JSON 恰为 {"completed": true, "message": "所有问题已回答完毕"}——
// 无 question 键，message 文案与 Java 逐字一致。
func TestCurrentQuestionResponseCompletedContract(t *testing.T) {
	resp := &CurrentQuestionResponse{Completed: true, Message: stringPtr("所有问题已回答完毕")}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("完成分支应恰有两个键: %v", m)
	}
	if v, ok := m["completed"]; !ok || v != true {
		t.Fatalf("completed 应为 true: %v", m["completed"])
	}
	if v, _ := m["message"].(string); v != "所有问题已回答完毕" {
		t.Fatalf("message 文案与 Java 不一致: %q", m["message"])
	}
	if _, has := m["question"]; has {
		t.Fatal("完成分支不应有 question 键")
	}
}

// 契约：SubmitAnswerResponse 字段名与 Java record 逐字一致；末题时 nextQuestion 为 null
// （显式 null，与 Java Jackson 默认行为一致，不用 omitempty）。
func TestSubmitAnswerResponseContract(t *testing.T) {
	b, err := json.Marshal(SubmitAnswerResponse{HasNextQuestion: false, CurrentIndex: 3, TotalQuestions: 3})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	wantKeys := []string{"hasNextQuestion", "nextQuestion", "currentIndex", "totalQuestions"}
	if len(m) != len(wantKeys) {
		t.Fatalf("键数量不符: %v", m)
	}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Fatalf("缺字段 %q", k)
		}
	}
	if v, ok := m["nextQuestion"]; !ok || v != nil {
		t.Fatalf("末题 nextQuestion 应存在且为 null: %v %v", ok, v)
	}

	// 非末题：nextQuestion 携带下一题完整形状。
	questions := GenerateFallbackQuestions(nil, 2, 0)
	b, _ = json.Marshal(SubmitAnswerResponse{HasNextQuestion: true, NextQuestion: &questions[1], CurrentIndex: 1, TotalQuestions: 2})
	var resp SubmitAnswerResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if !resp.HasNextQuestion || resp.NextQuestion == nil || resp.NextQuestion.QuestionIndex != 1 {
		t.Fatalf("非末题响应不符: %+v", resp)
	}
}

// ===== questions_json 回写契约（作答回写 userAnswer，与 Java withAnswer 一致）=====

// 契约：暂存/提交把 userAnswer 写回 questions_json 中对应题号；未作答题保持 null；
// currentIndex 不被暂存触碰（由提交/取题推进）。
func TestQuestionsJSONUserAnswerWriteBack(t *testing.T) {
	questions := GenerateFallbackQuestions(nil, 2, 0)
	if err := validateAnswerIndex(0, len(questions)); err != nil {
		t.Fatalf("守卫不应拦截: %v", err)
	}
	questions[0].UserAnswer = stringPtr("第 0 题答案")

	raw, err := SerializeQuestions(questions)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	back, err := DeserializeQuestions(raw)
	if err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if back[0].UserAnswer == nil || *back[0].UserAnswer != "第 0 题答案" {
		t.Fatalf("userAnswer 应回写至 questions_json: %+v", back[0])
	}
	if back[1].UserAnswer != nil {
		t.Fatalf("未作答题 userAnswer 应保持 null: %+v", back[1])
	}
}
