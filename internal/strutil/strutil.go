// Package strutil 跨模块共用的字符串小工具（原 internal/interview、internal/resume、
// internal/vector 各自重复的 truncateRunes 收敛于此）。
package strutil

// TruncateRunes 按字符数截断，避免切出残缺的 UTF-8 序列。
func TruncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
