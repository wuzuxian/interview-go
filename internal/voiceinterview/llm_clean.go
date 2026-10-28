// LLM 文本处理纯函数，与 Java DashscopeLlmService（L202-248）逐字对齐：
// 实时文本清洗 normalizeRealtimeText（去 Markdown/列表标记、压空白）、语音化清洗
// optimizeForVoice（超 120 字截断到句界）、终止标点检测 hasTerminalSince、
// LLM 异常四类友好话术 mapLLMErrorToUserMessage（L184-200）。
// 所有长度/下标一律按字符（rune）计，对齐 Java String 的 char 语义。
package voiceinterview

import (
	"regexp"
	"strings"
)

// voiceTerminalPunctuation 句边界终止标点（Java TERMINAL_PUNCTUATION 逐字）。
const voiceTerminalPunctuation = "。！？；!?;."

// Java mapLlmErrorToUserMessage 的兜底话术。
const msgLLMUnavailable = "抱歉，AI 服务暂时不可用，请稍后重试"

// 清洗正则（Java replaceAll 参数逐字）。
var (
	voiceListPrefixPattern = regexp.MustCompile(`(?m)^\s*[-*+]\s*`)
	voiceWhitespacePattern = regexp.MustCompile(`\s+`)
)

// normalizeRealtimeText 实时文本清洗（Java normalizeRealtimeText L228-239 逐字）：
// 空白返回空串 → 去 **/```/` → 去行首列表标记 → 连续空白压成单空格 → 去首尾空白。
func normalizeRealtimeText(content string) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	s := strings.ReplaceAll(content, "**", "")
	s = strings.ReplaceAll(s, "```", "")
	s = strings.ReplaceAll(s, "`", "")
	s = voiceListPrefixPattern.ReplaceAllString(s, "")
	s = voiceWhitespacePattern.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// optimizeForVoice 语音化清洗（Java optimizeForVoice L202-226 逐字）：
// 清洗后空白返回「请继续。」；不超上限原样返回；超限截断到上限并在句界
// （截断段内最后一个终止标点且位置过半）收尾，否则追加省略号。
// 上限取 max(80, maxChars)（Java Math.max(80, aiQuestionMaxChars)）。
func optimizeForVoice(content string, maxChars int) string {
	normalized := normalizeRealtimeText(content)
	if normalized == "" {
		return "请继续。"
	}

	limit := maxChars
	if limit < 80 {
		limit = 80
	}
	runes := []rune(normalized)
	if len(runes) <= limit {
		return normalized
	}

	truncated := runes[:limit]
	lastTerminal := -1
	for i := len(truncated) - 1; i >= 0; i-- {
		if strings.ContainsRune(voiceTerminalPunctuation, truncated[i]) {
			lastTerminal = i
			break
		}
	}
	if lastTerminal >= limit/2 {
		return string(runes[:lastTerminal+1])
	}
	return string(truncated) + "…"
}

// hasTerminalSince token 中是否出现终止标点（Java hasTerminalSince L241-248）。
func hasTerminalSince(token string) bool {
	for _, r := range token {
		if strings.ContainsRune(voiceTerminalPunctuation, r) {
			return true
		}
	}
	return false
}

// mapLLMErrorToUserMessage LLM 异常 → 用户友好话术（Java mapLlmErrorToUserMessage
// L184-200 逐字）：沿错误消息匹配 403/ACCESS_DENIED/Authentication、timeout、
// 429/rate limit/quota、connection/network 四类，其余兜底。
func mapLLMErrorToUserMessage(err error) string {
	if err == nil {
		return msgLLMUnavailable
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "403") || strings.Contains(msg, "ACCESS_DENIED") ||
		strings.Contains(msg, "Authentication"):
		return "AI 服务认证失败，请检查 API Key 配置"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "Timeout"):
		return "AI 服务响应超时，请稍后重试"
	case strings.Contains(msg, "429") || strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "quota"):
		return "AI 服务调用频率超限，请稍后重试"
	case strings.Contains(msg, "connection") || strings.Contains(msg, "network"):
		return "AI 服务网络连接失败，请检查网络"
	default:
		return msgLLMUnavailable
	}
}

// trimVoiceToNull 对齐 Java VoiceInterviewMessageEntity.trimToNull：空白 → nil，
// 其余去首尾空白。
func trimVoiceToNull(text string) *string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// formatTurnLines DIALOGUE 消息 → 「面试官：/候选人：」历史行（Java
// VoiceContextCompressor.formatRecent L237-267 语义逐条：未答问题挂起直到下一轮
// 出现，空白文本按缺失处理）。本票供原始最近窗口格式化，票 07 压缩加载复用同语义。
func formatTurnLines(msgs []MessageEntity) []string {
	lines := []string{}
	pendingQuestion := ""
	for _, m := range msgs {
		aiText := trimVoiceToNull(valueOfVoice(m.AiGeneratedText))
		userText := trimVoiceToNull(valueOfVoice(m.UserRecognizedText))
		if aiText == nil && userText == nil {
			continue
		}
		if pendingQuestion != "" {
			lines = append(lines, "面试官："+pendingQuestion)
			pendingQuestion = ""
			if userText != nil {
				lines = append(lines, "候选人："+*userText)
			}
			if aiText != nil {
				pendingQuestion = *aiText
			}
			continue
		}
		switch {
		case aiText != nil && userText != nil:
			lines = append(lines, "面试官："+*aiText, "候选人："+*userText)
		case aiText != nil:
			pendingQuestion = *aiText
		case userText != nil:
			lines = append(lines, "候选人："+*userText)
		}
	}
	if pendingQuestion != "" {
		lines = append(lines, "面试官："+pendingQuestion)
	}
	return lines
}

func valueOfVoice(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
