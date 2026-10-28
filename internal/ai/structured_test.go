// 结构化输出调用器单测：本地 JSON 修复算法（未转义引号修复，Java
// repairUnescapedQuotesInJsonStrings 移植）与 InvokeStructured 的修复路径契约——
// 修复在单次调用内完成、不消耗重试次数；无法修复时保留原始解析错误走重试循环。
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/wuzuxian/interview/internal/apperr"
)

// scriptedCompleter 按调用次序返回脚本化响应，记录调用次数。
type scriptedCompleter struct {
	respond func(call int) (string, error)
	calls   int
}

func (s *scriptedCompleter) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	s.calls++
	if s.respond == nil {
		return nil, errors.New("stub 未配置响应")
	}
	content, err := s.respond(s.calls)
	if err != nil {
		return nil, err
	}
	return &schema.Message{Content: content}, nil
}

// ===== 修复算法直接单测 =====

func TestRepairUnescapedQuotesEscapesInnerQuotes(t *testing.T) {
	in := `{"question":"什么是"锁"机制","score":8}`
	want := `{"question":"什么是\"锁\"机制","score":8}`
	if got := repairUnescapedQuotesInJSONStrings(in); got != want {
		t.Fatalf("修复结果不对\nwant %s\ngot  %s", want, got)
	}
	var out struct {
		Question string `json:"question"`
		Score    int    `json:"score"`
	}
	if err := json.Unmarshal([]byte(want), &out); err != nil {
		t.Fatalf("修复后仍无法解析: %v", err)
	}
	if out.Question != `什么是"锁"机制` || out.Score != 8 {
		t.Errorf("修复后语义被破坏: %+v", out)
	}
}

func TestRepairUnescapedQuotesLeavesValidJSONUntouched(t *testing.T) {
	cases := []string{
		`{"a":"he said \"hi\"","b":[1,2],"c":{"d":"x"},"e":"line\nbreak"}`,
		`{"question":"普通中文题目","score":10}`,
		`{"a":"冒号: 逗号, 中括号] 大括号}"}`,
		`{"a":"x" ,"b":1}`,
		`[{"a":""},{"b":"尾部引号\""}]`,
	}
	for _, in := range cases {
		if got := repairUnescapedQuotesInJSONStrings(in); got != in {
			t.Errorf("合法 JSON 不应被改动\nin   %s\n got %s", in, got)
		}
	}
}

func TestRepairUnescapedQuotesUnrepairableReturnsOriginal(t *testing.T) {
	// 无法修复（截断/非 JSON/引号无法定位）时必须原样返回，由调用方保留原始解析错误。
	cases := []string{
		`{"a": "unterminated`,
		`not json at all`,
		``,
		`   `,
	}
	for _, in := range cases {
		if got := repairUnescapedQuotesInJSONStrings(in); got != in {
			t.Errorf("无法修复应原样返回: in %q got %q", in, got)
		}
	}
	// 修复后仍不合法：算法会改动字符串，但调用方应保留第一次的解析错误（invoke 侧断言）。
	if got := repairUnescapedQuotesInJSONStrings(`{"a":"b"c":"d"}`); got == `{"a":"b"c":"d"}` {
		t.Errorf("该输入应被算法改动（改后仍不合法，供 invoke 测试用）")
	}
}

// ===== InvokeStructured 修复路径 =====

type repairProbe struct {
	Question string `json:"question"`
	Score    int    `json:"score"`
}

// 契约：单次调用返回含未转义引号的 JSON 时，本地修复后即解析成功，
// 不消耗重试（整个调用只发生一次 LLM 调用）。
func TestInvokeStructuredRepairsUnescapedQuotesWithoutRetry(t *testing.T) {
	stub := &scriptedCompleter{respond: func(int) (string, error) {
		return `{"question":"他说"你好"了吗","score":9}`, nil
	}}

	out, err := InvokeStructured[repairProbe](context.Background(),
		NewStructuredInvoker(DefaultStructuredConfig(), nil), stub,
		"sys", "usr", "{question:string, score:int}", "修复路径", 3050)
	if err != nil {
		t.Fatalf("修复后应解析成功: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("修复应在一次调用内完成，不消耗重试: calls=%d", stub.calls)
	}
	if out.Question != `他说"你好"了吗` || out.Score != 9 {
		t.Errorf("修复后语义被破坏: %+v", out)
	}
}

// 契约：无法修复时保留原始解析错误语义（消息含“JSON 解析失败”），
// 仍按 MaxAttempts 重试后返回业务错误。
func TestInvokeStructuredUnrepairableKeepsOriginalError(t *testing.T) {
	stub := &scriptedCompleter{respond: func(int) (string, error) {
		return `{"a":"b"c":"d"}`, nil // 修复会改动但仍不合法
	}}

	_, err := InvokeStructured[repairProbe](context.Background(),
		NewStructuredInvoker(DefaultStructuredConfig(), nil), stub,
		"sys", "usr", "{question:string, score:int}", "不可修复", 3050)
	if err == nil {
		t.Fatal("不可修复输入应返回错误")
	}
	if stub.calls != 2 {
		t.Errorf("默认 MaxAttempts=2：calls=%d", stub.calls)
	}
	var ae *apperr.AppError
	if !errors.As(err, &ae) {
		t.Fatalf("应返回业务错误: %v", err)
	}
	if !strings.Contains(ae.Error(), "JSON 解析失败") {
		t.Errorf("错误应保留原始解析失败原因: %v", err)
	}
}
