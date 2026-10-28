// 流式出句（Java DashscopeLlmService.chatStreamSentences L71-158 逐字语义）：
// eino 流式增量累积中检测句边界（终止标点 。！？；!?;.），逐句回调；实时文本按
// 节流（间隔 ≥ pushInterval 且增量 ≥ minCharsDelta）推送；流末补发尾段句子并以
// optimizeForVoice 收尾推送。返回完整优化后的回复文本。
package voiceinterview

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
)

// streamParams 流式出句参数（Java 侧取自 VoiceInterviewProperties 的三个阈值 +
// 两个回调 + prompt 上下文）。
type streamParams struct {
	SystemPrompt  string
	UserPrompt    string
	PushInterval  time.Duration // Config.LLMStreamPushInterval（Java aiStreamPushIntervalMs）
	MinCharsDelta int           // Config.LLMStreamMinCharsDelta（Java aiStreamMinCharsDelta）
	MaxChars      int           // Config.AIQuestionMaxChars（optimizeForVoice 上限）
	OnToken       func(string)  // 实时文本（清洗后全量前缀）；可 nil
	OnSentence    func(string)  // 完整句回调；可 nil
}

// streamSentences 执行一次流式调用（Java chatStreamSentences L76-158）。
// 模型级错误向上传播（调用方映射四类话术）；不主动超时（与 Java 一致，
// 依赖 provider 客户端超时）。
func streamSentences(ctx context.Context, model StreamChatModel, sp streamParams) (string, error) {
	sr, err := model.Stream(ctx, []*schema.Message{
		schema.SystemMessage(sp.SystemPrompt),
		schema.UserMessage(sp.UserPrompt),
	})
	if err != nil {
		return "", err
	}
	defer sr.Close()

	var raw strings.Builder
	lastEmitNanos := time.Now()
	lastEmitLength := 0
	lastSentenceEnd := 0

	// Java Math.max(80, aiStreamPushIntervalMs) / Math.max(4, aiStreamMinCharsDelta)。
	emitInterval := sp.PushInterval
	if emitInterval < 80*time.Millisecond {
		emitInterval = 80 * time.Millisecond
	}
	minCharsDelta := sp.MinCharsDelta
	if minCharsDelta < 4 {
		minCharsDelta = 4
	}

	for {
		chunk, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		token := chunk.Content
		if token == "" {
			continue
		}
		raw.WriteString(token)

		// 检测句子边界，回调 onSentence（Java L100-111）。
		if sp.OnSentence != nil && hasTerminalSince(token) {
			normalized := normalizeRealtimeText(raw.String())
			currentEnd := len([]rune(normalized))
			if currentEnd > lastSentenceEnd {
				sentence := strings.TrimSpace(string([]rune(normalized)[lastSentenceEnd:currentEnd]))
				if sentence != "" {
					sp.OnSentence(sentence)
				}
				lastSentenceEnd = currentEnd
			}
		}

		// 实时文本推送（Java L113-130）：节流双阈值 + 空文本不推。
		if sp.OnToken == nil {
			continue
		}
		now := time.Now()
		elapsed := now.Sub(lastEmitNanos)
		currentLength := len([]rune(raw.String()))
		if elapsed < emitInterval || currentLength-lastEmitLength < minCharsDelta {
			continue
		}
		normalized := normalizeRealtimeText(raw.String())
		if normalized == "" {
			continue
		}
		sp.OnToken(normalized)
		lastEmitNanos = now
		lastEmitLength = len([]rune(normalized))
	}

	// 发送最后一段（可能不以终止标点结尾，Java L134-143）。
	if sp.OnSentence != nil {
		normalized := normalizeRealtimeText(raw.String())
		runes := []rune(normalized)
		if len(runes) > lastSentenceEnd {
			remaining := strings.TrimSpace(string(runes[lastSentenceEnd:]))
			if remaining != "" {
				sp.OnSentence(remaining)
			}
		}
	}

	optimized := optimizeForVoice(raw.String(), sp.MaxChars)
	if sp.OnToken != nil && strings.TrimSpace(optimized) != "" {
		sp.OnToken(optimized)
	}
	return optimized, nil
}
