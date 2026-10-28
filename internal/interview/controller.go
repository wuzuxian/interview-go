package interview

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/ratelimit"
	"github.com/wuzuxian/interview/internal/result"
)

// Controller 面试模块控制器，路径与 Java 版 InterviewSkillController 一致。
type Controller struct {
	svc *Service
	rl  *ratelimit.Limiter
}

// NewController 创建控制器。
func NewController(svc *Service, rl *ratelimit.Limiter) *Controller {
	return &Controller{svc: svc, rl: rl}
}

// RegisterRoutes 注册 /api/interview 下本票范围内的全部路由。
// 列表与详情为公开读接口，Java 版无 @RateLimit，故不挂限流；
// parse-jd 对齐 Java @RateLimit(dimension = IP, count = 5)（默认 interval=1s → 每秒 5 次/IP），
// 按主控裁决 #6 用仓库现有 Redis 固定窗口中间件近似滑动窗口。
func (ctl *Controller) RegisterRoutes(g *gin.RouterGroup) {
	h := g.Group("/interview")

	skills := h.Group("/skills")
	skills.GET("", ctl.listSkills)
	skills.GET("/:id", ctl.getSkill)
	skills.POST("/parse-jd", ctl.rl.Middleware(ratelimit.PerIP(5, time.Second)), ctl.parseJd)
}

func (ctl *Controller) listSkills(c *gin.Context) {
	result.OK(c, ctl.svc.ListSkills())
}

func (ctl *Controller) getSkill(c *gin.Context) {
	data, err := ctl.svc.GetSkill(c.Param("id"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

// parseJdRequest 对齐 Java ParseJdRequest(@NotBlank String jdText)；
// 空白/过短文本由 service 的长度校验拒绝，消息与 Java parseJd 一致。
type parseJdRequest struct {
	JdText string `json:"jdText"`
}

func (ctl *Controller) parseJd(c *gin.Context) {
	var req parseJdRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		result.FailErr(c, apperr.BadRequest("请求体非法: "+err.Error()))
		return
	}
	data, err := ctl.svc.ParseJd(c.Request.Context(), req.JdText)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}
