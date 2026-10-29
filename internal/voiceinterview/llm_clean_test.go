// LLM 文本处理纯函数单测（表驱动 + 中文用例名）：实时文本清洗（normalizeRealtimeText）、
// 语音化清洗与截断（optimizeForVoice，Java DashscopeLlmService L202-248 逐字）、
// 终止标点检测、LLM 错误话术四类映射（Java mapLlmErrorToUserMessage L184-200 逐字）、
// 历史行格式化（Java VoiceContextCompressor.formatRecent L237-267 语义）。
package voiceinterview

import (
	"strings"
	"testing"
)

func TestNormalizeRealtimeText清洗规则(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空串返回空", "", ""},
		{"纯空白返回空", "   \n\t ", ""},
		{"去加粗星号", "**重点**内容", "重点内容"},
		{"去代码围栏", "```java\ncode\n```", "java code"},
		{"去行内反引号", "使用`HashMap`即可", "使用HashMap即可"},
		{"去行首列表标记", "- 第一点\n* 第二点\n+ 第三点", "第一点 第二点 第三点"},
		{"压缩连续空白", "你好   世界\n\t继续", "你好 世界 继续"},
		{"普通文本原样", "请问你的项目经历如何？", "请问你的项目经历如何？"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRealtimeText(tc.in); got != tc.want {
				t.Fatalf("normalizeRealtimeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// runesN 生成 n 个重复 rune 的字符串。
func runesN(r rune, n int) string {
	return strings.Repeat(string(r), n)
}

func TestOptimizeForVoice语音化截断(t *testing.T) {
	t.Run("空白回复返回请继续", func(t *testing.T) {
		if got := optimizeForVoice("   ", 120); got != "请继续。" {
			t.Fatalf("空白回复应为「请继续。」: %q", got)
		}
	})
	t.Run("短回复清洗后原样返回", func(t *testing.T) {
		if got := optimizeForVoice("**你好。**  请介绍项目。", 120); got != "你好。 请介绍项目。" {
			t.Fatalf("短回复应清洗后原样: %q", got)
		}
	})
	t.Run("超限截断到最后一句边界", func(t *testing.T) {
		// 100 个字 + 。 + 20 个字 = 121 字 > 120：截前 120 字，最后终止标点在
		// 第 100 位（≥ 120/2=60）→ 保留到该句号，共 101 字。
		content := runesN('a', 100) + "。" + runesN('b', 20)
		got := optimizeForVoice(content, 120)
		if got != runesN('a', 100)+"。" {
			t.Fatalf("应截断到句界: got 长度 %d", len([]rune(got)))
		}
	})
	t.Run("句界过半不足时追加省略号", func(t *testing.T) {
		// 终止标点在第 10 位（< 60）→ 截前 120 字 + …。
		content := runesN('x', 10) + "。" + runesN('y', 119)
		got := optimizeForVoice(content, 120)
		if got != runesN('x', 10)+"。"+runesN('y', 109)+"…" {
			t.Fatalf("句界不足一半应追加省略号: 尾字符=%q 长度=%d",
				[]rune(got)[len([]rune(got))-1], len([]rune(got)))
		}
	})
	t.Run("上限低于80按80处理", func(t *testing.T) {
		content := runesN('x', 100) // 无终止标点
		got := optimizeForVoice(content, 30)
		if got != runesN('x', 80)+"…" {
			t.Fatalf("maxChars<80 应按 80 截断: 实际长度 %d", len([]rune(got)))
		}
	})
	t.Run("恰好等于上限不截断", func(t *testing.T) {
		content := runesN('z', 120)
		if got := optimizeForVoice(content, 120); got != content {
			t.Fatalf("恰好 120 字应原样返回")
		}
	})
}

func TestHasTerminalSince终止标点检测(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{"中文句号", "你好。", true},
		{"中文问号", "是什么？", true},
		{"中文叹号", "不错！", true},
		{"中文分号", "第一；", true},
		{"英文句点", "ok.", true},
		{"英文问叹分号", "really?! yes;", true},
		{"无终止标点", "继续说，然后", false},
		{"空串", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasTerminalSince(tc.token); got != tc.want {
				t.Fatalf("hasTerminalSince(%q) = %v, want %v", tc.token, got, tc.want)
			}
		})
	}
}

func TestMapLLMErrorToUserMessage四类话术(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want string
	}{
		{"认证失败403", "Error code: 403 - ACCESS_DENIED", "AI 服务认证失败，请检查 API Key 配置"},
		{"认证失败Authentication", "Authentication error", "AI 服务认证失败，请检查 API Key 配置"},
		{"超时小写", "request timeout after 30s", "AI 服务响应超时，请稍后重试"},
		{"超时大写", "Timeout waiting for response", "AI 服务响应超时，请稍后重试"},
		{"限流429", "Error code: 429 - Too many requests", "AI 服务调用频率超限，请稍后重试"},
		{"限流rate limit", "rate limit exceeded", "AI 服务调用频率超限，请稍后重试"},
		{"限流quota", "quota exhausted", "AI 服务调用频率超限，请稍后重试"},
		{"网络connection", "connection refused", "AI 服务网络连接失败，请检查网络"},
		{"网络network", "network unreachable", "AI 服务网络连接失败，请检查网络"},
		{"未知错误兜底", "Provider 'x' 不存在", "抱歉，AI 服务暂时不可用，请稍后重试"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapLLMErrorToUserMessage(errString(tc.err)); got != tc.want {
				t.Fatalf("话术不匹配: got %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("空错误兜底", func(t *testing.T) {
		if got := mapLLMErrorToUserMessage(nil); got != "抱歉，AI 服务暂时不可用，请稍后重试" {
			t.Fatalf("nil 错误应兜底: %q", got)
		}
	})
}

type errString string

func (e errString) Error() string { return string(e) }

func TestFormatTurnLines历史行格式化(t *testing.T) {
	msg := func(ai, user string) MessageEntity {
		m := MessageEntity{MessageType: MessageDialogue}
		if ai != "" {
			m.AiGeneratedText = &ai
		}
		if user != "" {
			m.UserRecognizedText = &user
		}
		return m
	}
	cases := []struct {
		name string
		msgs []MessageEntity
		want []string
	}{
		{"空历史", nil, nil},
		{"问答完整轮", []MessageEntity{msg("介绍一下项目", "我做了电商系统")},
			[]string{"面试官：介绍一下项目", "候选人：我做了电商系统"}},
		{"仅AI开场待回答", []MessageEntity{msg("先自我介绍吧", "")},
			[]string{"面试官：先自我介绍吧"}},
		{"未答问题后接新一轮", []MessageEntity{msg("第一问", ""), msg("追问", "回答")},
			[]string{"面试官：第一问", "候选人：回答", "面试官：追问"}},
		{"仅用户话语", []MessageEntity{msg("", "补充一句")},
			[]string{"候选人：补充一句"}},
		{"空白文本按缺失处理", []MessageEntity{msg("  ", "答")},
			[]string{"候选人：答"}},
		{"挂起期间双空行先冲刷挂起问题", []MessageEntity{msg("第一问", ""), msg("", ""), msg("", "答一")},
			[]string{"面试官：第一问", "候选人：答一"}},
		{"双空行无挂起时跳过", []MessageEntity{msg("", ""), msg("问", "答")},
			[]string{"面试官：问", "候选人：答"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTurnLines(tc.msgs)
			if len(got) != len(tc.want) {
				t.Fatalf("行数不符: got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("第 %d 行不符: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
