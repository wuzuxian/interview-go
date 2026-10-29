// 知识库题库控制器（票 04，Java KnowledgeBaseInterviewController 题库 8 接口逐字对齐）：
// generate 限流 GLOBAL=2/IP=2（@RateLimit 注解数值）；其余接口无注解不加中间件。
// 全部走统一 Result 封装；generate 参数校验对齐 Java @Valid 消息与声明序。
package knowledgebase

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/ratelimit"
	"github.com/wuzuxian/interview/internal/result"
)

// QuestionController 题库路由控制器。
type QuestionController struct {
	svc *QuestionService
	rl  *ratelimit.Limiter
}

// NewQuestionController 创建控制器。
func NewQuestionController(svc *QuestionService, rl *ratelimit.Limiter) *QuestionController {
	return &QuestionController{svc: svc, rl: rl}
}

// RegisterRoutes 注册题库路由（方法与路径对齐 Java 注解；questions 为 :id 的静态
// 兄弟段，先例 module 内 categories/search 等同级静态路由）。
func (ctl *QuestionController) RegisterRoutes(g *gin.RouterGroup) {
	h := g.Group("/knowledgebase")
	h.POST("/:id/questions/generate", ctl.rl.Middleware(
		ratelimit.Global(2, time.Second),
		ratelimit.PerIP(2, time.Second),
	), ctl.generateQuestions)
	h.GET("/:id/questions", ctl.listQuestions)
	h.GET("/:id/questions/categories", ctl.listCategories)
	h.GET("/:id/questions/generation-status", ctl.generationStatus)
	h.POST("/:id/questions", ctl.createQuestion)
	q := h.Group("/questions")
	q.PUT("/:questionId", ctl.updateQuestion)
	q.PUT("/:questionId/status", ctl.updateQuestionStatus)
	q.DELETE("/:questionId", ctl.deleteQuestion)
}

// generateQuestions POST /api/knowledgebase/{id}/questions/generate。
func (ctl *QuestionController) generateQuestions(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var body struct {
		Difficulty    *string `json:"difficulty"`
		QuestionCount *int    `json:"questionCount"`
		FollowUpCount *int    `json:"followUpCount"`
		CategoryLimit *int    `json:"categoryLimit"`
		LLMProvider   *string `json:"llmProvider"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		result.Fail(c, 400, "请求体格式不正确")
		return
	}
	if violations := generateViolations(body.Difficulty, body.QuestionCount, body.FollowUpCount,
		body.CategoryLimit, body.LLMProvider); len(violations) > 0 {
		result.Fail(c, apperr.CodeBadRequest, strings.Join(violations, ", "))
		return
	}
	resp, err := ctl.svc.SubmitGenerationTask(c.Request.Context(), id, GenerateQuestionsRequest{
		Difficulty:    body.Difficulty,
		QuestionCount: body.QuestionCount,
		FollowUpCount: body.FollowUpCount,
		CategoryLimit: body.CategoryLimit,
		LLMProvider:   body.LLMProvider,
	})
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, resp)
}

// generateViolations 校验 generate 请求（Java @Valid 注解逐字：违规消息按字段
// 声明序排列，Java HandlerMethodValidationException 由全局处理器以 ", " 连接）。
func generateViolations(difficulty *string, questionCount, followUpCount, categoryLimit *int, llmProvider *string) []string {
	var violations []string
	if difficulty != nil && !generateDifficultyPattern(*difficulty) {
		violations = append(violations, "题目难度不合法")
	}
	if questionCount == nil || *questionCount < 1 {
		violations = append(violations, "题目数量最少1题")
	} else if *questionCount > 30 {
		violations = append(violations, "题目数量最多30题")
	}
	if followUpCount != nil {
		if *followUpCount < 0 {
			violations = append(violations, "追问数量不能小于0")
		} else if *followUpCount > 5 {
			violations = append(violations, "每题追问最多5个")
		}
	}
	if categoryLimit == nil {
		violations = append(violations, "方向数量不能为空")
	} else if *categoryLimit < 1 {
		violations = append(violations, "方向数最少1个")
	} else if *categoryLimit > 5 {
		violations = append(violations, "方向数最多5个")
	}
	if llmProvider != nil && len([]rune(*llmProvider)) > 64 {
		violations = append(violations, "模型提供商标识过长")
	}
	return violations
}

// generateDifficultyPattern Java @Pattern(regexp = "junior|mid|senior")。
func generateDifficultyPattern(v string) bool {
	switch v {
	case "junior", "mid", "senior":
		return true
	default:
		return false
	}
}

// listQuestions GET /api/knowledgebase/{id}/questions（status/category/difficulty/keyword）。
func (ctl *QuestionController) listQuestions(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var status *QuestionBankStatus
	if raw := c.Query("status"); raw != "" {
		st, err := parseQuestionStatus(raw)
		if err != nil {
			result.FailErr(c, err)
			return
		}
		status = &st
	}
	category := c.Query("category")
	difficulty := c.Query("difficulty")
	keyword := c.Query("keyword")
	list, err := ctl.svc.ListQuestions(c.Request.Context(), id, status, &category, &difficulty, &keyword)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, list)
}

// listCategories GET /api/knowledgebase/{id}/questions/categories。
func (ctl *QuestionController) listCategories(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	list, err := ctl.svc.ListCategories(c.Request.Context(), id)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, list)
}

// generationStatus GET /api/knowledgebase/{id}/questions/generation-status。
func (ctl *QuestionController) generationStatus(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	resp, err := ctl.svc.GetGenerationStatus(c.Request.Context(), id)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, resp)
}

// createQuestion POST /api/knowledgebase/{id}/questions（手动建题）。
func (ctl *QuestionController) createQuestion(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var req CreateQuestionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, 400, "请求体格式不正确")
		return
	}
	dto, err := ctl.svc.CreateQuestion(c.Request.Context(), id, req)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dto)
}

// updateQuestion PUT /api/knowledgebase/questions/{questionId}。
func (ctl *QuestionController) updateQuestion(c *gin.Context) {
	questionID, ok := parseQuestionID(c)
	if !ok {
		return
	}
	var req UpdateQuestionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, 400, "请求体格式不正确")
		return
	}
	dto, err := ctl.svc.UpdateQuestion(c.Request.Context(), questionID, req)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dto)
}

// updateQuestionStatus PUT /api/knowledgebase/questions/{questionId}/status。
func (ctl *QuestionController) updateQuestionStatus(c *gin.Context) {
	questionID, ok := parseQuestionID(c)
	if !ok {
		return
	}
	var body struct {
		Status *string `json:"status"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		result.Fail(c, 400, "请求体格式不正确")
		return
	}
	if body.Status == nil {
		result.Fail(c, 400, "题目状态不能为空")
		return
	}
	status, err := parseQuestionStatus(*body.Status)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	dto, err := ctl.svc.UpdateStatus(c.Request.Context(), questionID, status)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dto)
}

// deleteQuestion DELETE /api/knowledgebase/questions/{questionId}。
func (ctl *QuestionController) deleteQuestion(c *gin.Context) {
	questionID, ok := parseQuestionID(c)
	if !ok {
		return
	}
	if err := ctl.svc.DeleteQuestion(c.Request.Context(), questionID); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

// parseQuestionID 路径参数 questionId 解析为 int64；非法时返回 400。
func parseQuestionID(c *gin.Context) (int64, bool) {
	raw := c.Param("questionId")
	var id int64
	neg := false
	for i, ch := range raw {
		if i == 0 && ch == '-' {
			neg = true
			continue
		}
		if ch < '0' || ch > '9' {
			result.Fail(c, 400, "路径参数 questionId 非法")
			return 0, false
		}
		id = id*10 + int64(ch-'0')
	}
	if neg {
		id = -id
	}
	return id, true
}
