package resume

import "strings"

// 合法优先级与兜底文案。
// 前端 AnalysisPanel 的硬性契约（不可改前端，必须在后端保证）：
//  1. suggestions[].priority 必须是 "高"/"中"/"低" 之一，否则该条被分组逻辑静默丢弃；
//  2. suggestions[].recommendation 必须是非空字符串——模板中 `{s.recommendation || s}`
//     在 recommendation 为空时会把整个对象当 React 子节点渲染，直接抛
//     "Objects are not valid as a React child" 使页面崩溃。
const (
	defaultSuggestionPriority = "中"
	emptyRecommendationText   = "本次分析未生成具体建议，请结合问题描述进行针对性改进。"
	emptyIssueText            = "改进建议"
)

// NormalizeSuggestions 归一化 LLM 输出/历史库中的建议列表：
// 去除首尾空白、修正 category/priority 缺省、保证 recommendation 非空，
// 同时丢弃既无 issue 也无 recommendation 的空条目。
// 写入路径（评分落库）与读取路径（详情/最新结果 DTO）统一走这里，
// 对修复前已入库的脏数据同样生效。
func NormalizeSuggestions(in []Suggestion) []Suggestion {
	out := make([]Suggestion, 0, len(in))
	for _, s := range in {
		s.Category = strings.TrimSpace(s.Category)
		s.Priority = normalizePriority(s.Priority)
		s.Issue = strings.TrimSpace(s.Issue)
		s.Recommendation = strings.TrimSpace(s.Recommendation)

		if s.Issue == "" && s.Recommendation == "" {
			continue
		}
		if s.Category == "" {
			s.Category = "其他"
		}
		if s.Issue == "" {
			s.Issue = emptyIssueText
		}
		if s.Recommendation == "" {
			s.Recommendation = emptyRecommendationText
		}
		out = append(out, s)
	}
	return out
}

// normalizePriority 归一化优先级：接受常见变体（high/medium/low、大小写），
// 无法识别时归为"中"，避免被前端分组逻辑丢弃。
func normalizePriority(p string) string {
	switch strings.TrimSpace(strings.ToLower(p)) {
	case "高", "high", "critical", "p0":
		return "高"
	case "中", "medium", "mid", "normal", "p1":
		return "中"
	case "低", "low", "p2":
		return "低"
	default:
		return defaultSuggestionPriority
	}
}
