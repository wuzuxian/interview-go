// Controller 语音面试模块控制器，路径与 Java 版 VoiceInterviewController 一致
// （基路径 /api/voice-interview；evaluation 两接口属票 08，本票不注册）。
// 与 Java 一致无 @RateLimit（语音限流配置为 Java 死配置，spec 偏离 #3 不移植）。
package voiceinterview

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/result"
)

// Controller 语音面试控制器。
type Controller struct {
	svc *Service
}

// NewController 创建控制器。
func NewController(svc *Service) *Controller { return &Controller{svc: svc} }

// RegisterRoutes 注册 /api/voice-interview 下全部路由。
func (ctl *Controller) RegisterRoutes(g *gin.RouterGroup) {
	h := g.Group("/voice-interview")
	h.POST("/sessions", ctl.createSession)
	h.GET("/sessions", ctl.listSessions)
	h.GET("/sessions/:sessionId", ctl.getSession)
	h.POST("/sessions/:sessionId/end", ctl.endSession)
	h.PUT("/sessions/:sessionId/pause", ctl.pauseSession)
	h.PUT("/sessions/:sessionId/resume", ctl.resumeSession)
	h.DELETE("/sessions/:sessionId", ctl.deleteSession)
	h.GET("/sessions/:sessionId/messages", ctl.getMessages)
	// GET/POST /sessions/{id}/evaluation 属票 08。
}

// parseSessionID 路径参数解析（Java @PathVariable Long：非数字在 Spring 侧 400）。
func parseSessionID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("sessionId"), 10, 64)
	if err != nil {
		result.Fail(c, apperr.CodeBadRequest, "会话ID格式不正确")
		return 0, false
	}
	return id, true
}

func (ctl *Controller) createSession(c *gin.Context) {
	var req CreateSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, apperr.CodeBadRequest, "请求体格式不正确")
		return
	}
	data, err := ctl.svc.CreateSession(c.Request.Context(), req)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) listSessions(c *gin.Context) {
	data, err := ctl.svc.GetAllSessions(c.Request.Context(), c.Query("userId"), c.Query("status"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

// getSession GET /sessions/{id}：服务层返回 nil 时对齐 Java Controller L69-75 的
// Result.error("Session not found: " + id)（code=SERVER_ERROR=500，data=null）。
func (ctl *Controller) getSession(c *gin.Context) {
	id, ok := parseSessionID(c)
	if !ok {
		return
	}
	data, err := ctl.svc.GetSessionDTO(c.Request.Context(), id)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	if data == nil {
		result.Fail(c, apperr.CodeInternalError, "Session not found: "+c.Param("sessionId"))
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) endSession(c *gin.Context) {
	id, ok := parseSessionID(c)
	if !ok {
		return
	}
	if err := ctl.svc.EndSession(c.Request.Context(), id); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) pauseSession(c *gin.Context) {
	id, ok := parseSessionID(c)
	if !ok {
		return
	}
	// 对齐 Java @RequestBody Map 取 reason（缺省 "user_initiated"）。
	var req struct {
		Reason *string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, apperr.CodeBadRequest, "请求体格式不正确")
		return
	}
	reason := DefaultPauseReason
	if req.Reason != nil {
		reason = *req.Reason
	}
	if err := ctl.svc.PauseSession(c.Request.Context(), id, reason); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) resumeSession(c *gin.Context) {
	id, ok := parseSessionID(c)
	if !ok {
		return
	}
	data, err := ctl.svc.ResumeSession(c.Request.Context(), id)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) deleteSession(c *gin.Context) {
	id, ok := parseSessionID(c)
	if !ok {
		return
	}
	if err := ctl.svc.DeleteSession(c.Request.Context(), id); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) getMessages(c *gin.Context) {
	id, ok := parseSessionID(c)
	if !ok {
		return
	}
	data, err := ctl.svc.GetConversationHistoryDTO(c.Request.Context(), id)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}
