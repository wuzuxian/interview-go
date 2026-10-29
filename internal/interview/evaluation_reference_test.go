// 评估 per-question 参考基线注入测试（票 05 验收点）：题目自带参考答案/评分要点/
// 评分规则拼接为 referenceContext 注入评分提示词（Java AnswerEvaluationService
// buildQuestionReferenceContext 逐字格式），优先于方向参考段；报告参考答案被题目
// 自带值覆盖。走 stub ChatCompleter 断言 LLM 收到的 prompt 与报告内容（先例
// evaluation_test.go 的脚本化替身）。
package interview

import (
	"context"
	"strings"
	"testing"
)

// 契约：buildQuestionReferenceContext 拼接格式逐字——「问题N: 题干」「参考答案: x」
// 「评分要点: a；b」「评分规则: x」，N 为 questionIndex+1；无参考基线的题跳过；
// 空白字段跳过；段间空行。
func TestBuildQuestionReferenceContext逐字(t *testing.T) {
	ref := "JMM 定义线程与主内存交互"
	rubric := "答出 happens-before 给分"
	questions := []QuestionDTO{
		{
			QuestionIndex: 0, Question: "什么是 JMM？",
			ReferenceAnswer: &ref,
			KeyPoints:       []string{"可见性", "有序性"},
			ScoringRubric:   &rubric,
		},
		{QuestionIndex: 1, Question: "无基线题"}, // 全空 → 跳过
		{
			QuestionIndex: 2, Question: "追问带部分基线",
			ReferenceAnswer: stringPtr("  "), // 空白跳过
			KeyPoints:       []string{"追问要点"},
		},
	}
	got := buildQuestionReferenceContext(questions)
	want := "问题1: 什么是 JMM？\n" +
		"参考答案: JMM 定义线程与主内存交互\n" +
		"评分要点: 可见性；有序性\n" +
		"评分规则: 答出 happens-before 给分\n" +
		"\n" +
		"问题3: 追问带部分基线\n" +
		"评分要点: 追问要点\n" +
		"\n"
	if got != want {
		t.Fatalf("referenceContext =\n%q\nwant\n%q", got, want)
	}
	if buildQuestionReferenceContext(nil) != "" {
		t.Fatal("无参考基线时应返回空串（交由方向参考段兜底）")
	}
}

// 契约：题目自带参考基线注入评分提示词（batch 与 summary 两段 prompt 的
// {referenceContext}），且优先于方向参考段（SkillReference 不出现）；
// 报告 referenceAnswers 被题目自带值覆盖。
func TestEvaluateSessionPerQuestionReferenceBaseline(t *testing.T) {
	engine := newTestEngine()
	completer := &evalStubCompleter{respond: respondSummaryOK(func(call int, user string) (string, error) {
		return batchReportJSONFor(promptIndexes(user), func(int) int { return 80 }), nil
	})}

	questions := []QuestionDTO{
		{
			QuestionIndex: 0, Question: "什么是 JMM？", Category: stringPtr("Java"),
			UserAnswer:      answered("关于内存可见性……"),
			ReferenceAnswer: stringPtr("JMM 定义线程与主内存交互"),
			KeyPoints:       []string{"可见性"},
			ScoringRubric:   stringPtr("答出 happens-before 给分"),
		},
	}
	report, err := engine.EvaluateSession(context.Background(), completer, newTestInvoker(),
		EvaluateInput{SessionID: "s1", ResumeText: "简历", Questions: questions,
			SkillReference: "方向参考段：Java 基础知识清单（不应出现）"})
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}

	if len(completer.calls) == 0 {
		t.Fatal("应产生 LLM 调用")
	}
	for i, call := range completer.calls {
		if !strings.Contains(call.user, "参考答案: JMM 定义线程与主内存交互") {
			t.Errorf("调用 %d 的 user prompt 未注入题目参考基线", i)
		}
		if strings.Contains(call.user, "方向参考段") {
			t.Errorf("调用 %d 的 user prompt 不应使用方向参考段（题目自带基线优先）", i)
		}
	}
	// 报告参考答案被题目自带值覆盖（withQuestionReferences）。
	if len(report.ReferenceAnswers) != 1 {
		t.Fatalf("referenceAnswers = %d 条, want 1", len(report.ReferenceAnswers))
	}
	if report.ReferenceAnswers[0].ReferenceAnswer != "JMM 定义线程与主内存交互" {
		t.Errorf("报告参考答案 = %q", report.ReferenceAnswers[0].ReferenceAnswer)
	}
	if len(report.ReferenceAnswers[0].KeyPoints) != 1 || report.ReferenceAnswers[0].KeyPoints[0] != "可见性" {
		t.Errorf("报告参考答案要点 = %v", report.ReferenceAnswers[0].KeyPoints)
	}
}

// 契约：题目无参考基线时回退方向参考段（既有兜底语义，票 05 的知识库面试不触发
// 但普通文字面试依赖）。
func TestEvaluateSessionFallsBackToSkillReference(t *testing.T) {
	engine := newTestEngine()
	completer := &evalStubCompleter{respond: respondSummaryOK(func(call int, user string) (string, error) {
		return batchReportJSONFor(promptIndexes(user), func(int) int { return 70 }), nil
	})}
	questions := []QuestionDTO{mainQuestion(0, "普通题", "Java", answered("答"))}
	if _, err := engine.EvaluateSession(context.Background(), completer, newTestInvoker(),
		EvaluateInput{SessionID: "s2", ResumeText: "", Questions: questions,
			SkillReference: "方向参考段：Java 基础知识清单"}); err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	found := false
	for _, call := range completer.calls {
		if strings.Contains(call.user, "方向参考段：Java 基础知识清单") {
			found = true
		}
	}
	if !found {
		t.Fatal("题目无基线时 user prompt 应使用方向参考段兜底")
	}
}
