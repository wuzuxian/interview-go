// 知识库面试控制器（票 05，Java KnowledgeBaseInterviewController 面试 2 接口逐字对齐）：
// POST /api/knowledgebase-interviews/sessions 与 GET /api/knowledgebase/{id}/interview-capacity。
// 两接口在 Java 侧均无 @RateLimit 注解，不加限流中间件；创建请求体校验消息与声明序
// 对齐 @Valid（MethodArgumentNotValidException 以 ", " 连接，先例题库 generate）；
// 容量 query 的 mainQuestionCount 越界在 Java 为 HandlerMethodValidationException
// （无专用处理器）落入全局兜底 → 500「系统繁忙，请稍后重试」，类型转换失败同。
package knowledgebase

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/result"
)

// InterviewController 知识库面试路由控制器。
type InterviewController struct {
	svc *InterviewService
}

// NewInterviewController 创建控制器。
func NewInterviewController(svc *InterviewService) *InterviewController {
	return &InterviewController{svc: svc}
}

// RegisterRoutes 注册知识库面试路由（路径对齐 Java 注解；/knowledgebase-interviews
// 为与 /knowledgebase 平级的独立静态前缀，:id/interview-capacity 与题库路由同组）。
func (ctl *InterviewController) RegisterRoutes(g *gin.RouterGroup) {
	g.POST("/knowledgebase-interviews/sessions", ctl.createSession)
	h := g.Group("/knowledgebase")
	h.GET("/:id/interview-capacity", ctl.capacity)
}

// createSession POST /api/knowledgebase-interviews/sessions。
func (ctl *InterviewController) createSession(c *gin.Context) {
	var req CreateInterviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, 400, "请求体格式不正确")
		return
	}
	if violations := createInterviewViolations(&req); len(violations) > 0 {
		result.Fail(c, apperr.CodeBadRequest, strings.Join(violations, ", "))
		return
	}
	dto, err := ctl.svc.CreateSession(c.Request.Context(), req)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dto)
}

// createInterviewViolations 校验创建请求（Java @Valid 注解逐字：违规消息按字段声明序
// 排列，全局处理器以 ", " 连接；mainQuestionCount/followUpCount 为原生 int，缺失按
// 缺省 0 参与校验）。
func createInterviewViolations(req *CreateInterviewRequest) []string {
	var violations []string
	if req.KnowledgeBaseID == nil {
		violations = append(violations, "知识库不能为空")
	}
	mainQuestionCount := intValueOf(req.MainQuestionCount)
	if mainQuestionCount < 1 {
		violations = append(violations, "主问题数量最少1题")
	} else if mainQuestionCount > 20 {
		violations = append(violations, "主问题数量最多20题")
	}
	followUpCount := intValueOf(req.FollowUpCount)
	if followUpCount < 0 {
		violations = append(violations, "追问数量不能小于0")
	} else if followUpCount > 5 {
		violations = append(violations, "每题追问最多5个")
	}
	return violations
}

// capacity GET /api/knowledgebase/{id}/interview-capacity：
// category 可空；difficulty 默认 mid；mainQuestionCount 默认 5（Java @RequestParam
// defaultValue），越界/非法在 Java 走全局兜底 → 500「系统繁忙，请稍后重试」。
func (ctl *InterviewController) capacity(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	mainQuestionCount, err := strconv.Atoi(c.DefaultQuery("mainQuestionCount", "5"))
	if err != nil || mainQuestionCount < 1 || mainQuestionCount > 20 {
		result.Fail(c, apperr.CodeInternalError, "系统繁忙，请稍后重试")
		return
	}
	category := c.Query("category")
	difficulty := c.DefaultQuery("difficulty", "mid")
	resp, err := ctl.svc.GetCapacity(c.Request.Context(), id, &category, difficulty, mainQuestionCount)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, resp)
}
