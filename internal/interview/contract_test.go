package interview

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// 契约回归测试：响应 DTO 的 JSON 键集以前端 TS 类型为黄金清单
// （interview-guide/frontend/src/types/interview.ts，浏览器消费的真契约）。
// 背景：SessionDTO 曾把 currentQuestionIndex 误发成 currentIndex（与提交答案响应/
// 内部缓存的合法同名字段混淆），前端按契约读到 undefined 后静默渲染空白、无报错。
// 该测试以"键集逐名相等"锁死契约：改名、加 omitempty、删字段都会红灯。

func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertKeys(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JSON 键集与前端契约不一致\n got: %v\nwant: %v", got, want)
	}
}

func sortedKeys(keys ...string) []string {
	sort.Strings(keys)
	return keys
}

// TestSessionDTOContract 对应前端 InterviewSession 类型。
// CurrentQuestionIndex 刻意取零值：场次初始索引就是 0，该键在零值时也必须输出。
func TestSessionDTOContract(t *testing.T) {
	got := jsonKeys(t, SessionDTO{})
	assertKeys(t, got, sortedKeys(
		"sessionId", "resumeText", "totalQuestions", "currentQuestionIndex",
		"questions", "status", "knowledgeBaseId", "interviewCategory",
	))
}

// TestQuestionDTOContract 对应前端 InterviewQuestion 类型。
// 全部指针字段取 nil：契约要求这些键以 null 输出而不是消失。
func TestQuestionDTOContract(t *testing.T) {
	got := jsonKeys(t, QuestionDTO{})
	assertKeys(t, got, sortedKeys(
		"questionIndex", "question", "type", "category", "topicSummary",
		"userAnswer", "score", "feedback", "isFollowUp", "parentQuestionIndex",
		"referenceAnswer", "keyPoints", "scoringRubric", "sourceContext",
	))
}

// SubmitAnswerResponse 的契约（含 nextQuestion 显式 null 行为）锁定在
// answer_loop_test.go 的 TestSubmitAnswerResponseContract：该类型的合法字段名
// 就是 currentIndex（Java 同名），与 SessionDTO 的 currentQuestionIndex 是两个字段。
