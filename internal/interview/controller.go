package interview

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/ratelimit"
	"github.com/wuzuxian/interview/internal/result"
)

// Controller 面试模块控制器，路径与 Java 版 InterviewController /
// InterviewSkillController 一致。
type Controller struct {
	svc      *Service
	sessions *SessionService
	rl       *ratelimit.Limiter
}

// NewController 创建控制器。sessions/rl 为票 03 场次子系统；
// skills 路由（票 01）与场次路由共存于同一控制器。
func NewController(svc *Service, sessions *SessionService, rl *ratelimit.Limiter) *Controller {
	return &Controller{svc: svc, sessions: sessions, rl: rl}
}

// RegisterRoutes 注册 /api/interview 下全部路由。
// 列表/详情/unfinished/DELETE 与 Java 一致无 @RateLimit；
// parse-jd 随票 02 上线时再加限流。
func (ctl *Controller) RegisterRoutes(g *gin.RouterGroup) {
	h := g.Group("/interview")

	skills := h.Group("/skills")
	skills.GET("", ctl.listSkills)
	skills.GET("/:id", ctl.getSkill)

	// 先注册静态段 unfinished，避免被 :sessionId 通配吞掉（gin 静态优先，仍按序靠前声明）。
	h.GET("/sessions/unfinished/:resumeId", ctl.findUnfinishedSession)

	// 创建限流：全局 5/s + IP 5/s，对齐 Java 双 @RateLimit 注解（可重复注解全部生效）。
	h.POST("/sessions", ctl.rl.Middleware(
		ratelimit.Global(5, time.Second),
		ratelimit.PerIP(5, time.Second),
	), ctl.createSession)
	h.GET("/sessions", ctl.listSessions)
	h.GET("/sessions/:sessionId", ctl.getSession)
	h.DELETE("/sessions/:sessionId", ctl.deleteSession)
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

func (ctl *Controller) createSession(c *gin.Context) {
	var req CreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, 400, "请求体格式不正确")
		return
	}
	data, err := ctl.sessions.CreateSession(c.Request.Context(), req)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) listSessions(c *gin.Context) {
	data, err := ctl.sessions.ListSessions(c.Request.Context())
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) getSession(c *gin.Context) {
	data, err := ctl.sessions.GetSession(c.Request.Context(), c.Param("sessionId"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) findUnfinishedSession(c *gin.Context) {
	resumeID, err := strconv.ParseInt(c.Param("resumeId"), 10, 64)
	if err != nil {
		result.Fail(c, 400, "简历ID格式不正确")
		return
	}
	data, err := ctl.sessions.FindUnfinishedOrThrow(c.Request.Context(), resumeID)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) deleteSession(c *gin.Context) {
	if err := ctl.sessions.DeleteSession(c.Request.Context(), c.Param("sessionId")); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}
