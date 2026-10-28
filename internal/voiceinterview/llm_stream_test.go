// 流式出句单测（Java chatStreamSentences L71-158 语义）：stub 流模型以 eino
// schema.Pipe 造流（eino-notes.md 第 3 条），覆盖句边界切分、节流推送、尾段补发、
// 错误传播与空回复兜底。
package voiceinterview

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

// scriptedStreamModel 脚本化流式模型替身：Stream 时按序发送 tokens（可选间隔），
// err 非 nil 时以错误收尾；block 非 nil 时发完 tokens 挂起直至关闭。
type scriptedStreamModel struct {
	mu     sync.Mutex
	tokens []string
	gap    time.Duration
	err    error
	block  chan struct{}

	muIn   sync.Mutex
	inputs []*schema.Message
}

func (m *scriptedStreamModel) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	m.muIn.Lock()
	m.inputs = append(m.inputs, input...)
	m.muIn.Unlock()

	sr, sw := schema.Pipe[*schema.Message](0)
	err := m.err
	tokens := m.tokens
	gap := m.gap
	block := m.block
	go func() {
		defer sw.Close()
		if err != nil {
			sw.Send(nil, err)
			return
		}
		for _, tk := range tokens {
			select {
			case <-ctx.Done():
				sw.Send(nil, ctx.Err())
				return
			default:
			}
			sw.Send(&schema.Message{Content: tk}, nil)
			if gap > 0 {
				time.Sleep(gap)
			}
		}
		if block != nil {
			<-block
		}
	}()
	return sr, nil
}

func (m *scriptedStreamModel) lastInput() []*schema.Message {
	m.muIn.Lock()
	defer m.muIn.Unlock()
	return append([]*schema.Message(nil), m.inputs...)
}

func collectStrings() (func(string), *[]string) {
	var mu sync.Mutex
	var out []string
	return func(s string) {
		mu.Lock()
		out = append(out, s)
		mu.Unlock()
	}, &out
}

func snapshot(list *[]string) []string {
	return append([]string(nil), *list...)
}

func TestStreamSentences句边界切分(t *testing.T) {
	model := &scriptedStreamModel{tokens: []string{"你好。", "今天我们聊聊", "项目经历，好吗？", "先从这里", "开始。"}}
	onSentence, sentences := collectStrings()

	reply, err := streamSentences(context.Background(), model, streamParams{
		SystemPrompt: "sys", UserPrompt: "usr",
		PushInterval: 80 * time.Millisecond, MinCharsDelta: 12, MaxChars: 120,
		OnSentence: onSentence,
	})
	if err != nil {
		t.Fatalf("流式出句不应报错: %v", err)
	}

	want := []string{"你好。", "今天我们聊聊项目经历，好吗？", "先从这里开始。"}
	got := snapshot(sentences)
	if len(got) != len(want) {
		t.Fatalf("句数不符: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 句不符: got %q, want %q", i, got[i], want[i])
		}
	}
	if reply != "你好。今天我们聊聊项目经历，好吗？先从这里开始。" {
		t.Fatalf("完整回复不符: %q", reply)
	}
}

func TestStreamSentences节流推送形状(t *testing.T) {
	model := &scriptedStreamModel{tokens: thirtyTokens(), gap: 5 * time.Millisecond}
	onToken, tokens := collectStrings()

	reply, err := streamSentences(context.Background(), model, streamParams{
		SystemPrompt: "sys", UserPrompt: "usr",
		PushInterval: 80 * time.Millisecond, MinCharsDelta: 4, MaxChars: 120,
		OnToken: onToken,
	})
	if err != nil {
		t.Fatalf("流式出句不应报错: %v", err)
	}

	got := snapshot(tokens)
	if len(got) < 2 {
		t.Fatalf("节流窗口内应产生多次部分推送: %d 次", len(got))
	}
	for i, tk := range got {
		if tk == "" {
			t.Fatalf("第 %d 次推送为空（空白不推送）", i)
		}
	}
	if got[len(got)-1] != reply {
		t.Fatalf("末次推送应为完整清洗文本: %q vs %q", got[len(got)-1], reply)
	}
}

// thirtyTokens 30 个增量 token（每个 4 字符，累计 120 字符）。
func thirtyTokens() []string {
	out := make([]string, 30)
	for i := range out {
		out[i] = "甲乙丙丁"
	}
	return out
}

func TestStreamSentences节流间隔下界(t *testing.T) {
	model := &scriptedStreamModel{tokens: thirtyTokens(), gap: 5 * time.Millisecond}
	onToken, tokens := collectStrings()

	start := time.Now()
	if _, err := streamSentences(context.Background(), model, streamParams{
		SystemPrompt: "sys", UserPrompt: "usr",
		PushInterval: 80 * time.Millisecond, MinCharsDelta: 4, MaxChars: 120,
		OnToken: onToken,
	}); err != nil {
		t.Fatalf("流式出句不应报错: %v", err)
	}
	elapsed := time.Since(start)

	got := snapshot(tokens)
	// 30 token × 5ms = 150s 交付时长内，80ms 节流至多允许
	// (elapsed/80ms)+1 次推送；断言未超发（节流生效）。
	maxPushes := int(elapsed/(80*time.Millisecond)) + 2
	if len(got) > maxPushes {
		t.Fatalf("节流未生效: %d 次推送超上界 %d (elapsed=%v)", len(got), maxPushes, elapsed)
	}
	if len(got) < 2 {
		t.Fatalf("应至少推送两次（部分+final）: %d", len(got))
	}
}

func TestStreamSentences错误传播(t *testing.T) {
	model := &scriptedStreamModel{err: errString("Error code: 403 - ACCESS_DENIED")}
	onSentence, sentences := collectStrings()

	_, err := streamSentences(context.Background(), model, streamParams{
		PushInterval: 80 * time.Millisecond, MinCharsDelta: 12, MaxChars: 120,
		OnSentence: onSentence,
	})
	if err == nil {
		t.Fatal("模型错误应向上传播")
	}
	if got := mapLLMErrorToUserMessage(err); got != "AI 服务认证失败，请检查 API Key 配置" {
		t.Fatalf("错误应映射认证话术: %q", got)
	}
	if len(snapshot(sentences)) != 0 {
		t.Fatal("出错时不应产生句子")
	}
}

func TestStreamSentences无终止标点整段补发(t *testing.T) {
	model := &scriptedStreamModel{tokens: []string{"嗯嗯", "，好的，", "那我先说结论"}}
	onSentence, sentences := collectStrings()

	reply, err := streamSentences(context.Background(), model, streamParams{
		PushInterval: 80 * time.Millisecond, MinCharsDelta: 12, MaxChars: 120,
		OnSentence: onSentence,
	})
	if err != nil {
		t.Fatalf("流式出句不应报错: %v", err)
	}
	got := snapshot(sentences)
	if len(got) != 1 || got[0] != "嗯嗯，好的，那我先说结论" {
		t.Fatalf("尾段应整段补发为一句: %v", got)
	}
	if reply != "嗯嗯，好的，那我先说结论" {
		t.Fatalf("完整回复不符: %q", reply)
	}
}

func TestStreamSentences空回复走请继续(t *testing.T) {
	model := &scriptedStreamModel{}
	onToken, tokens := collectStrings()
	onSentence, sentences := collectStrings()

	reply, err := streamSentences(context.Background(), model, streamParams{
		PushInterval: 80 * time.Millisecond, MinCharsDelta: 12, MaxChars: 120,
		OnToken: onToken, OnSentence: onSentence,
	})
	if err != nil {
		t.Fatalf("空流不应报错: %v", err)
	}
	if reply != "请继续。" {
		t.Fatalf("空回复应兜底「请继续。」: %q", reply)
	}
	if got := snapshot(sentences); len(got) != 0 {
		t.Fatalf("空回复不应产生句子: %v", got)
	}
	if got := snapshot(tokens); len(got) != 1 || got[0] != "请继续。" {
		t.Fatalf("final 推送应为「请继续。」: %v", got)
	}
}

func TestStreamSentences请求消息形状(t *testing.T) {
	model := &scriptedStreamModel{tokens: []string{"你好。"}}
	_, err := streamSentences(context.Background(), model, streamParams{
		SystemPrompt: "系统提示", UserPrompt: "用户提示",
		PushInterval: 80 * time.Millisecond, MinCharsDelta: 12, MaxChars: 120,
	})
	if err != nil {
		t.Fatalf("流式出句不应报错: %v", err)
	}
	inputs := model.lastInput()
	if len(inputs) != 2 {
		t.Fatalf("请求应为 system+user 两条消息: %d", len(inputs))
	}
	if inputs[0].Role != schema.System || inputs[0].Content != "系统提示" {
		t.Fatalf("第一条应为 system: %+v", inputs[0])
	}
	if inputs[1].Role != schema.User || inputs[1].Content != "用户提示" {
		t.Fatalf("第二条应为 user: %+v", inputs[1])
	}
}

// 编译期守卫：io.EOF 读取循环按 eino-notes.md 用法（防止误删 import）。
var _ = io.EOF
