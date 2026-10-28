// 冒烟命令的纯校验逻辑（票 09）：响应解析与结构校验，不接触网络与时间，
// 便于单测覆盖失败分支。校验目标全部来自票面验收项：
//   - 题目结构：主问题数=请求值、每题有 question/category/topicSummary/追问标记
//   - 报告结构：overallScore 0-100、categoryScores 非空且覆盖题目分类、
//     逐题 feedback 非空、referenceAnswers 存在
//   - 明细：问答明细完整，含未答题路径（userAnswer 为 null）
//   - PDF：%PDF 魔数 + 非空
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wuzuxian/interview/internal/interview"
)

// resultEnvelope 统一响应包装 {code,message,data}（internal/result.Result 的解析视图）。
type resultEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// parseEnvelope 解析统一响应包装并要求业务成功（code==200）。
func parseEnvelope(body []byte) (*resultEnvelope, error) {
	var env resultEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("响应不是合法 JSON: %v", err)
	}
	if env.Code != 200 {
		return nil, fmt.Errorf("业务错误 code=%d message=%q", env.Code, env.Message)
	}
	return &env, nil
}

// validateSession 校验创建响应的题目结构（票面验收第 2 项）：
// 主问题数（isFollowUp==false）必须等于请求的 questionCount；
// 每题具备 question/category 与追问标记自洽（追问必须指向更小题号的主问题）；
// 主问题必须有 topicSummary（追问按 Java 语义无 topicSummary，归入主问题分类衍生键）。
func validateSession(sess *interview.SessionDTO, wantMain int) error {
	if sess == nil {
		return fmt.Errorf("场次响应为空")
	}
	if sess.SessionID == "" {
		return fmt.Errorf("sessionId 为空")
	}
	if len(sess.Questions) == 0 {
		return fmt.Errorf("题目列表为空")
	}
	if sess.TotalQuestions != len(sess.Questions) {
		return fmt.Errorf("totalQuestions=%d 与题目数 %d 不一致", sess.TotalQuestions, len(sess.Questions))
	}
	mains := 0
	for i, q := range sess.Questions {
		if q.QuestionIndex != i {
			return fmt.Errorf("第 %d 题 questionIndex=%d，题号应连续递增", i, q.QuestionIndex)
		}
		if strings.TrimSpace(q.Question) == "" {
			return fmt.Errorf("第 %d 题题干为空", i)
		}
		if q.Category == nil || strings.TrimSpace(*q.Category) == "" {
			return fmt.Errorf("第 %d 题缺少 category", i)
		}
		if q.IsFollowUp {
			if q.ParentQuestionIndex == nil {
				return fmt.Errorf("第 %d 题标记为追问但缺少 parentQuestionIndex", i)
			}
			p := *q.ParentQuestionIndex
			if p < 0 || p >= i {
				return fmt.Errorf("第 %d 题追问指向的 parentQuestionIndex=%d 非法（应指向更小题号的主问题）", i, p)
			}
			if sess.Questions[p].IsFollowUp {
				return fmt.Errorf("第 %d 题追问的父题 %d 也是追问", i, p)
			}
		} else {
			mains++
			if q.ParentQuestionIndex != nil {
				return fmt.Errorf("第 %d 题是主问题但携带 parentQuestionIndex", i)
			}
			if q.TopicSummary == nil || strings.TrimSpace(*q.TopicSummary) == "" {
				return fmt.Errorf("第 %d 题（主问题）缺少 topicSummary", i)
			}
		}
	}
	if mains != wantMain {
		return fmt.Errorf("主问题数 %d 与请求值 %d 不符", mains, wantMain)
	}
	return nil
}

// validateReport 校验报告结构（票面验收第 2 项）：
// overallScore 0-100、categoryScores 非空且覆盖全部题目分类、逐题 feedback 非空、
// referenceAnswers 存在（逐题参考答案为空降级为告警，见 reportWarnings）。
func validateReport(rep *interview.ReportDTO) error {
	if rep == nil {
		return fmt.Errorf("报告为空")
	}
	if rep.OverallScore < 0 || rep.OverallScore > 100 {
		return fmt.Errorf("overallScore=%d 超出 0-100", rep.OverallScore)
	}
	if len(rep.CategoryScores) == 0 {
		return fmt.Errorf("categoryScores 为空")
	}
	scored := map[string]bool{}
	for _, cs := range rep.CategoryScores {
		if strings.TrimSpace(cs.Category) == "" {
			return fmt.Errorf("categoryScores 存在空分类名")
		}
		if cs.QuestionCount <= 0 {
			return fmt.Errorf("分类 %q 的 questionCount=%d 非法", cs.Category, cs.QuestionCount)
		}
		scored[cs.Category] = true
	}
	if len(rep.QuestionDetails) == 0 {
		return fmt.Errorf("questionDetails 为空")
	}
	for _, qd := range rep.QuestionDetails {
		if strings.TrimSpace(qd.Question) == "" {
			return fmt.Errorf("第 %d 题题干为空", qd.QuestionIndex)
		}
		if strings.TrimSpace(qd.Feedback) == "" {
			return fmt.Errorf("第 %d 题 feedback 为空", qd.QuestionIndex)
		}
		if !scored[qd.Category] {
			return fmt.Errorf("第 %d 题分类 %q 未被 categoryScores 覆盖", qd.QuestionIndex, qd.Category)
		}
	}
	if len(rep.ReferenceAnswers) == 0 {
		return fmt.Errorf("referenceAnswers 缺失")
	}
	if len(rep.ReferenceAnswers) != len(rep.QuestionDetails) {
		return fmt.Errorf("referenceAnswers 条数 %d 与题目数 %d 不符",
			len(rep.ReferenceAnswers), len(rep.QuestionDetails))
	}
	return nil
}

// reportWarnings 报告软性告警（不阻断冒烟，仅提示）：逐题参考答案文本为空、
// 总评语为空。这些字段由 LLM 生成，偶发缺失不应判定链路失败，但值得人工复核。
func reportWarnings(rep *interview.ReportDTO) []string {
	warnings := []string{}
	if strings.TrimSpace(rep.OverallFeedback) == "" {
		warnings = append(warnings, "overallFeedback 为空（LLM 偶发缺失，建议人工复核）")
	}
	for _, ra := range rep.ReferenceAnswers {
		if strings.TrimSpace(ra.ReferenceAnswer) == "" {
			warnings = append(warnings,
				fmt.Sprintf("第 %d 题参考答案文本为空（LLM 偶发缺失，建议人工复核）", ra.QuestionIndex))
		}
	}
	return warnings
}

// validateDetails 校验问答明细（票面步骤 h）：
// 明细条数与总题数一致；已答题有 userAnswer 与 answeredAt；
// 跳过未答的那题 userAnswer 为 null（覆盖未答题路径）；逐题 feedback 非空。
func validateDetails(detail *interview.InterviewDetailDTO, sessionID string, totalQuestions, skippedIndex int) error {
	if detail == nil {
		return fmt.Errorf("明细为空")
	}
	if detail.SessionID != sessionID {
		return fmt.Errorf("明细 sessionId=%q 与 %q 不符", detail.SessionID, sessionID)
	}
	if len(detail.Answers) != totalQuestions {
		return fmt.Errorf("明细条数 %d 与总题数 %d 不符", len(detail.Answers), totalQuestions)
	}
	seenSkipped := false
	for i, a := range detail.Answers {
		if a.QuestionIndex == nil || *a.QuestionIndex != i {
			return fmt.Errorf("明细第 %d 条 questionIndex 异常（应按题号升序且连续）", i)
		}
		if a.Question == nil || strings.TrimSpace(*a.Question) == "" {
			return fmt.Errorf("第 %d 题明细缺少题干", i)
		}
		if a.Feedback == nil || strings.TrimSpace(*a.Feedback) == "" {
			return fmt.Errorf("第 %d 题明细 feedback 为空", i)
		}
		if i == skippedIndex {
			seenSkipped = true
			if a.UserAnswer != nil {
				return fmt.Errorf("第 %d 题应为未作答（userAnswer 应为 null），实际有作答", i)
			}
			continue
		}
		if a.UserAnswer == nil || strings.TrimSpace(*a.UserAnswer) == "" {
			return fmt.Errorf("第 %d 题缺少作答内容", i)
		}
		if a.AnsweredAt == nil {
			return fmt.Errorf("第 %d 题缺少 answeredAt", i)
		}
	}
	if !seenSkipped {
		return fmt.Errorf("未找到应跳过的第 %d 题（未答题路径未覆盖）", skippedIndex)
	}
	return nil
}

// validatePDF 校验导出的 PDF（票面验收第 3 项）：非空、%PDF 魔数、%%EOF 收尾。
func validatePDF(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("PDF 内容为空")
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return fmt.Errorf("缺少 %%PDF 魔数（前 8 字节: %q）", excerpt(data, 8))
	}
	if !bytes.Contains(data, []byte("%%EOF")) {
		return fmt.Errorf("缺少 %%EOF 收尾标记")
	}
	return nil
}

// excerpt 截取字节片段用于错误信息（按 rune 截断避免切碎多字节字符）。
func excerpt(b []byte, max int) string {
	s := string(b)
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
