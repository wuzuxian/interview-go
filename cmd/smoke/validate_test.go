// 冒烟命令纯校验逻辑的单元测试（票 09）：只测外部行为——给定响应结构断言
// 校验结果，覆盖成功与关键失败分支，保证冒烟命令的判定口径不回退。
package main

import (
	"strings"
	"testing"
	"time"

	"github.com/wuzuxian/interview/internal/interview"
)

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

// timeZero 明细校验只断言 answeredAt 非空，具体时刻无关。
var timeZero = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// sampleQuestions 构造 2 主问题 + 2 追问的题目列表（对齐 convertToQuestions 布局）。
func sampleQuestions() []interview.QuestionDTO {
	return []interview.QuestionDTO{
		{QuestionIndex: 0, Question: "主问题一", Type: strPtr("TECHNICAL"), Category: strPtr("java-core"),
			TopicSummary: strPtr("集合框架"), IsFollowUp: false},
		{QuestionIndex: 1, Question: "追问一", Type: strPtr("TECHNICAL"), Category: strPtr("java-core-1"),
			IsFollowUp: true, ParentQuestionIndex: intPtr(0)},
		{QuestionIndex: 2, Question: "主问题二", Type: strPtr("TECHNICAL"), Category: strPtr("database"),
			TopicSummary: strPtr("事务隔离"), IsFollowUp: false},
		{QuestionIndex: 3, Question: "追问二", Type: strPtr("TECHNICAL"), Category: strPtr("database-1"),
			IsFollowUp: true, ParentQuestionIndex: intPtr(2)},
	}
}

func TestParseEnvelope(t *testing.T) {
	// 成功包装解出 data。
	env, err := parseEnvelope([]byte(`{"code":200,"message":"success","data":{"x":1}}`))
	if err != nil {
		t.Fatalf("合法响应解析失败: %v", err)
	}
	if env.Code != 200 {
		t.Fatalf("code 应为 200，实际 %d", env.Code)
	}
	// 业务错误（HTTP 200 + code!=200）必须判定失败。
	if _, err := parseEnvelope([]byte(`{"code":3001,"message":"面试会话不存在","data":null}`)); err == nil {
		t.Fatal("code!=200 应返回错误")
	}
	// 非 JSON 必须判定失败。
	if _, err := parseEnvelope([]byte(`not-json`)); err == nil {
		t.Fatal("非法 JSON 应返回错误")
	}
}

func TestValidateSession(t *testing.T) {
	ok := interview.SessionDTO{SessionID: "s-1", TotalQuestions: 4, Questions: sampleQuestions()}

	// 成功：主问题数=请求值 2。
	if err := validateSession(&ok, 2); err != nil {
		t.Fatalf("合法结构应通过: %v", err)
	}

	// 主问题数与请求值不符。
	if err := validateSession(&ok, 3); err == nil || !strings.Contains(err.Error(), "主问题数") {
		t.Fatalf("主问题数不符应报错，实际 %v", err)
	}

	// 主问题缺 topicSummary。
	bad := interview.SessionDTO{SessionID: "s-1", TotalQuestions: 4, Questions: sampleQuestions()}
	bad.Questions[0].TopicSummary = nil
	if err := validateSession(&bad, 2); err == nil || !strings.Contains(err.Error(), "topicSummary") {
		t.Fatalf("缺 topicSummary 应报错，实际 %v", err)
	}

	// 追问缺 parentQuestionIndex。
	bad = interview.SessionDTO{SessionID: "s-1", TotalQuestions: 4, Questions: sampleQuestions()}
	bad.Questions[1].ParentQuestionIndex = nil
	if err := validateSession(&bad, 2); err == nil || !strings.Contains(err.Error(), "parentQuestionIndex") {
		t.Fatalf("追问缺父题号应报错，实际 %v", err)
	}

	// 追问指向的父题不存在（越界）。
	bad = interview.SessionDTO{SessionID: "s-1", TotalQuestions: 4, Questions: sampleQuestions()}
	bad.Questions[1].ParentQuestionIndex = intPtr(9)
	if err := validateSession(&bad, 2); err == nil {
		t.Fatal("父题号越界应报错")
	}

	// 题号不连续。
	bad = interview.SessionDTO{SessionID: "s-1", TotalQuestions: 4, Questions: sampleQuestions()}
	bad.Questions[2].QuestionIndex = 5
	if err := validateSession(&bad, 2); err == nil || !strings.Contains(err.Error(), "questionIndex") {
		t.Fatalf("题号不连续应报错，实际 %v", err)
	}

	// 空题目列表。
	if err := validateSession(&interview.SessionDTO{SessionID: "s"}, 2); err == nil {
		t.Fatal("空题目列表应报错")
	}
}

func TestValidateReport(t *testing.T) {
	ok := interview.ReportDTO{
		SessionID: "s-1", OverallScore: 75,
		CategoryScores: []interview.CategoryScoreDTO{
			{Category: "java-core", Score: 80, QuestionCount: 2},
			{Category: "database", Score: 70, QuestionCount: 2},
		},
		QuestionDetails: []interview.QuestionEvaluationDTO{
			{QuestionIndex: 0, Question: "q0", Category: "java-core", Score: 80, Feedback: "不错"},
			{QuestionIndex: 1, Question: "q1", Category: "database", Score: 70, Feedback: "还行"},
		},
		ReferenceAnswers: []interview.ReferenceAnswerDTO{
			{QuestionIndex: 0, Question: "q0", ReferenceAnswer: "参考一"},
			{QuestionIndex: 1, Question: "q1", ReferenceAnswer: "参考二"},
		},
	}

	if err := validateReport(&ok); err != nil {
		t.Fatalf("合法报告应通过: %v", err)
	}

	// 总分越界（0-100）。
	bad := ok
	bad.OverallScore = 101
	if err := validateReport(&bad); err == nil || !strings.Contains(err.Error(), "0-100") {
		t.Fatalf("总分越界应报错，实际 %v", err)
	}

	// categoryScores 为空。
	bad = ok
	bad.CategoryScores = nil
	if err := validateReport(&bad); err == nil || !strings.Contains(err.Error(), "categoryScores") {
		t.Fatalf("空分类得分应报错，实际 %v", err)
	}

	// 题目分类未被 categoryScores 覆盖。
	bad = ok
	bad.QuestionDetails = append([]interview.QuestionEvaluationDTO{}, ok.QuestionDetails...)
	bad.QuestionDetails[1].Category = "network"
	if err := validateReport(&bad); err == nil || !strings.Contains(err.Error(), "未被") {
		t.Fatalf("分类未覆盖应报错，实际 %v", err)
	}

	// 逐题 feedback 为空。
	bad = ok
	bad.QuestionDetails = append([]interview.QuestionEvaluationDTO{}, ok.QuestionDetails...)
	bad.QuestionDetails[0].Feedback = "  "
	if err := validateReport(&bad); err == nil || !strings.Contains(err.Error(), "feedback") {
		t.Fatalf("空 feedback 应报错，实际 %v", err)
	}

	// referenceAnswers 缺失。
	bad = ok
	bad.ReferenceAnswers = nil
	if err := validateReport(&bad); err == nil || !strings.Contains(err.Error(), "referenceAnswers") {
		t.Fatalf("缺参考答案应报错，实际 %v", err)
	}
}

func TestReportWarnings(t *testing.T) {
	// 参考答案文本为空只告警不阻断（LLM 偶发缺失）。
	rep := interview.ReportDTO{
		OverallFeedback: "总体良好",
		ReferenceAnswers: []interview.ReferenceAnswerDTO{
			{QuestionIndex: 0, ReferenceAnswer: ""},
			{QuestionIndex: 1, ReferenceAnswer: "参考二"},
		},
	}
	if err := validateReport(&interview.ReportDTO{
		OverallScore:   50,
		CategoryScores: []interview.CategoryScoreDTO{{Category: "c", Score: 50, QuestionCount: 2}},
		QuestionDetails: []interview.QuestionEvaluationDTO{
			{QuestionIndex: 0, Question: "q0", Category: "c", Feedback: "f"},
			{QuestionIndex: 1, Question: "q1", Category: "c", Feedback: "f"},
		},
		ReferenceAnswers: rep.ReferenceAnswers,
	}); err != nil {
		t.Fatalf("空参考答案文本不应阻断: %v", err)
	}
	warnings := reportWarnings(&rep)
	if len(warnings) != 1 {
		t.Fatalf("应有 1 条空参考答案告警，实际 %v", warnings)
	}
}

func TestValidateDetails(t *testing.T) {
	// 4 题中第 2 题（index 1）未作答。
	ok := interview.InterviewDetailDTO{
		SessionID: "s-1",
		Answers: []interview.AnswerDetailDTO{
			{QuestionIndex: intPtr(0), Question: strPtr("q0"), UserAnswer: strPtr("答0"),
				Score: intPtr(80), Feedback: strPtr("f0"), AnsweredAt: &timeZero},
			{QuestionIndex: intPtr(1), Question: strPtr("q1"), UserAnswer: nil,
				Score: intPtr(0), Feedback: strPtr("f1"), AnsweredAt: nil},
			{QuestionIndex: intPtr(2), Question: strPtr("q2"), UserAnswer: strPtr("答2"),
				Score: intPtr(70), Feedback: strPtr("f2"), AnsweredAt: &timeZero},
			{QuestionIndex: intPtr(3), Question: strPtr("q3"), UserAnswer: strPtr("答3"),
				Score: intPtr(60), Feedback: strPtr("f3"), AnsweredAt: &timeZero},
		},
	}
	if err := validateDetails(&ok, "s-1", 4, 1); err != nil {
		t.Fatalf("合法明细应通过: %v", err)
	}

	// 被跳过题出现作答 → 未答题路径未覆盖。
	bad := ok
	bad.Answers = append([]interview.AnswerDetailDTO{}, ok.Answers...)
	bad.Answers[1].UserAnswer = strPtr("不该有")
	if err := validateDetails(&bad, "s-1", 4, 1); err == nil || !strings.Contains(err.Error(), "未作答") {
		t.Fatalf("跳过题有作答应报错，实际 %v", err)
	}

	// 已答题缺 answeredAt。
	bad = ok
	bad.Answers = append([]interview.AnswerDetailDTO{}, ok.Answers...)
	bad.Answers[0].AnsweredAt = nil
	if err := validateDetails(&bad, "s-1", 4, 1); err == nil || !strings.Contains(err.Error(), "answeredAt") {
		t.Fatalf("缺 answeredAt 应报错，实际 %v", err)
	}

	// 明细条数与总题数不符。
	if err := validateDetails(&ok, "s-1", 5, 1); err == nil || !strings.Contains(err.Error(), "不符") {
		t.Fatalf("条数不符应报错，实际 %v", err)
	}

	// sessionId 不符。
	if err := validateDetails(&ok, "other", 4, 1); err == nil || !strings.Contains(err.Error(), "sessionId") {
		t.Fatalf("sessionId 不符应报错，实际 %v", err)
	}
}

func TestValidatePDF(t *testing.T) {
	if err := validatePDF([]byte("%PDF-1.7\n...content...\n%%EOF\n")); err != nil {
		t.Fatalf("合法 PDF 应通过: %v", err)
	}
	if err := validatePDF(nil); err == nil {
		t.Fatal("空内容应报错")
	}
	if err := validatePDF([]byte("<html>error</html>")); err == nil || !strings.Contains(err.Error(), "PDF") {
		t.Fatalf("缺魔数应报错，实际 %v", err)
	}
	if err := validatePDF([]byte("%PDF-1.7\nno trailer")); err == nil || !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("缺 %%EOF 应报错，实际 %v", err)
	}
}

func TestNewRequestID(t *testing.T) {
	// 幂等键须匹配服务端 requestIdPattern：[A-Za-z0-9_-]{8,64}。
	id := newRequestID()
	if len(id) < 8 || len(id) > 64 {
		t.Fatalf("requestId 长度非法: %q", id)
	}
	for _, r := range id {
		ok := r == '-' || r == '_' ||
			(r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		if !ok {
			t.Fatalf("requestId 含非法字符 %q: %q", r, id)
		}
	}
	if newRequestID() == id {
		t.Fatal("requestId 应随机唯一")
	}
}

func TestCannedAnswer(t *testing.T) {
	// 模拟作答必须是有效真实答案（非空、非"不知道"式 0 分答案）。
	for i := 0; i < 10; i++ {
		a := cannedAnswer(i)
		if strings.TrimSpace(a) == "" || strings.Contains(a, "不知道") {
			t.Fatalf("第 %d 个模拟答案非法: %q", i, a)
		}
	}
}
