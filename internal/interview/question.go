// 题目 DTO 与静态兜底题生成：从 Java 版 InterviewQuestionDTO /
// InterviewQuestionService.generateFallbackQuestions（笔记 02）逐字移植。
package interview

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/wuzuxian/interview/internal/apperr"
)

// QuestionDTO 对应 Java record InterviewQuestionDTO，JSON 字段名逐字一致
// （lowerCamelCase record 组件名）：questionIndex/question/type/category/topicSummary/
// userAnswer/score/feedback/isFollowUp/parentQuestionIndex/referenceAnswer/keyPoints/
// scoringRubric/sourceContext。未赋值字段序列化为 null（指针），isFollowUp/score 为
// Java 原生类型语义恒出现。
type QuestionDTO struct {
	QuestionIndex       int      `json:"questionIndex"`
	Question            string   `json:"question"`
	Type                *string  `json:"type"`
	Category            *string  `json:"category"`
	TopicSummary        *string  `json:"topicSummary"`
	UserAnswer          *string  `json:"userAnswer"`
	Score               *int     `json:"score"`
	Feedback            *string  `json:"feedback"`
	IsFollowUp          bool     `json:"isFollowUp"`
	ParentQuestionIndex *int     `json:"parentQuestionIndex"`
	ReferenceAnswer     *string  `json:"referenceAnswer"`
	KeyPoints           []string `json:"keyPoints"`
	ScoringRubric       *string  `json:"scoringRubric"`
	SourceContext       *string  `json:"sourceContext"`
}

// CreateFallback 构造兜底主问题/追问，对应 Java InterviewQuestionDTO.create(index, question, type,
// category, topicSummary, isFollowUp, parentQuestionIndex)：其余字段全部为空。
func CreateFallback(index int, question, typ, category string, topicSummary *string, isFollowUp bool, parentQuestionIndex *int) QuestionDTO {
	return QuestionDTO{
		QuestionIndex:       index,
		Question:            question,
		Type:                stringPtr(typ),
		Category:            stringPtr(category),
		TopicSummary:        topicSummary,
		UserAnswer:          nil,
		Score:               nil,
		Feedback:            nil,
		IsFollowUp:          isFollowUp,
		ParentQuestionIndex: parentQuestionIndex,
		ReferenceAnswer:     nil,
		KeyPoints:           nil,
		ScoringRubric:       nil,
		SourceContext:       nil,
	}
}

func stringPtr(s string) *string { return &s }
func intPtr(i int) *int          { return &i }

// GenericFallbackQuestions Java GENERIC_FALLBACK_QUESTIONS 全 6 条逐字
// （InterviewQuestionService.java:66-73），格式 {题目, 分类key, 分类label}。
var GenericFallbackQuestions = [6][3]string{
	{"请描述一个你主导解决的技术难题，你的分析思路是什么？", "GENERAL", "综合能力"},
	{"你在做技术方案选型时，通常考虑哪些因素？请举例说明。", "GENERAL", "综合能力"},
	{"请分享一次你处理线上故障的经历，从发现到修复的完整过程。", "GENERAL", "综合能力"},
	{"你如何保证代码质量？介绍你实践过的有效手段。", "GENERAL", "综合能力"},
	{"描述一个你做过的技术优化案例，优化的动机、方案和效果。", "GENERAL", "综合能力"},
	{"你在团队协作中遇到过最大的分歧是什么？如何解决的？", "GENERAL", "综合能力"},
}

// BuildFollowUpCategory 对齐 Java buildFollowUpCategory（463-468 行）。
func BuildFollowUpCategory(category string, order int) string {
	base := category
	if strings.TrimSpace(base) == "" {
		base = "追问"
	}
	return base + "（追问" + strconv.Itoa(order) + "）"
}

// BuildDefaultFollowUp 对齐 Java buildDefaultFollowUp（470-473 行）。
func BuildDefaultFollowUp(mainQuestion string, order int) string {
	if order == 1 {
		return "基于\"" + mainQuestion + "\"，请结合你亲自做过的一个真实场景展开说明。"
	}
	return "基于\"" + mainQuestion + "\"，如果线上出现异常，你会如何定位并给出修复方案？"
}

// GenerateFallbackQuestions 对齐 Java generateFallbackQuestions（372-407 行，逐字语义）：
// 方向有分类时按分类模板轮转生成（含追问）；无分类时用 GENERIC_FALLBACK_QUESTIONS
// 前 min(count, 6) 条（含追问）。
func GenerateFallbackQuestions(categories []CategoryDTO, count, followUpCount int) []QuestionDTO {
	questions := make([]QuestionDTO, 0, count*(1+followUpCount))
	index := 0

	if len(categories) > 0 {
		generated := 0
		for generated < count {
			cat := categories[generated%len(categories)]
			question := "请谈谈你在\"" + cat.Label + "\"方向的技术理解和实践经验。"
			questions = append(questions, CreateFallback(index, question, cat.Key, cat.Label, nil, false, nil))
			mainIndex := index
			index++
			for j := 0; j < followUpCount; j++ {
				questions = append(questions, CreateFallback(
					index, BuildDefaultFollowUp(question, j+1),
					cat.Key, BuildFollowUpCategory(cat.Label, j+1), nil, true, intPtr(mainIndex)))
				index++
			}
			generated++
		}
		return questions
	}

	for i := 0; i < minInt(count, len(GenericFallbackQuestions)); i++ {
		q := GenericFallbackQuestions[i]
		questions = append(questions, CreateFallback(index, q[0], q[1], q[2], nil, false, nil))
		mainIndex := index
		index++
		for j := 0; j < followUpCount; j++ {
			questions = append(questions, CreateFallback(
				index, BuildDefaultFollowUp(q[0], j+1),
				q[1], BuildFollowUpCategory(q[2], j+1), nil, true, intPtr(mainIndex)))
			index++
		}
	}
	return questions
}

// ClampFollowUpCount 对齐 Java 第 109 行：Math.max(0, Math.min(followUpCount, 2))。
func ClampFollowUpCount(n int) int {
	if n < 0 {
		return 0
	}
	if n > 2 {
		return 2
	}
	return n
}

// SerializeQuestions questions_json 序列化契约：题目数组 JSON（与 Java Jackson record 序列化同形）。
func SerializeQuestions(qs []QuestionDTO) (string, error) {
	b, err := json.Marshal(qs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DeserializeQuestions questions_json 反序列化。
func DeserializeQuestions(s string) ([]QuestionDTO, error) {
	var qs []QuestionDTO
	if err := json.Unmarshal([]byte(s), &qs); err != nil {
		return nil, err
	}
	return qs, nil
}

// ===== 会话标识与幂等键 =====

// NewSessionID 对齐 Java 两处一致的表达式：
// UUID.randomUUID().toString().replace("-", "").substring(0, 16)。
func NewSessionID() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
}

// requestIdPattern Java normalizeRequestId 的 [A-Za-z0-9_-]{8,64}。
var requestIdPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// NormalizeRequestID 对齐 Java normalizeRequestId（InterviewSessionService.java:221-231）：
// 空白返回 nil（完全跳过幂等）；非空先 trim 再校验格式，不匹配抛 BAD_REQUEST "requestId 格式不正确"。
func NormalizeRequestID(requestID string) (*string, error) {
	if strings.TrimSpace(requestID) == "" {
		return nil, nil
	}
	normalized := strings.TrimSpace(requestID)
	if !requestIdPattern.MatchString(normalized) {
		return nil, apperr.BadRequest("requestId 格式不正确")
	}
	return &normalized, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
