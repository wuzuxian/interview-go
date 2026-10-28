package interview

import (
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/pdfexport"
	"github.com/wuzuxian/interview/internal/ratelimit"
	"github.com/wuzuxian/interview/internal/result"
)

// Controller 面试模块控制器，路径与 Java 版 InterviewController /
// InterviewSkillController 一致。
type Controller struct {
	svc      *Service
	sessions *SessionService
	report   *ReportService
	rl       *ratelimit.Limiter
}

// NewController 创建控制器。sessions/rl 为票 03 场次子系统，report 为票 06
// 报告/明细节拍；skills 路由（票 01）与场次路由共存于同一控制器。
func NewController(svc *Service, sessions *SessionService, report *ReportService, rl *ratelimit.Limiter) *Controller {
	return &Controller{svc: svc, sessions: sessions, report: report, rl: rl}
}

// RegisterRoutes 注册 /api/interview 下全部路由。
// 列表/详情/unfinished/DELETE 与 Java 一致无 @RateLimit；
// parse-jd 对齐 Java @RateLimit(dimension = IP, count = 5)（默认 interval=1s → 每秒 5 次/IP），
// 按主控裁决 #6 用仓库现有 Redis 固定窗口中间件近似滑动窗口。
func (ctl *Controller) RegisterRoutes(g *gin.RouterGroup) {
	h := g.Group("/interview")

	skills := h.Group("/skills")
	skills.GET("", ctl.listSkills)
	skills.GET("/:id", ctl.getSkill)
	skills.POST("/parse-jd", ctl.rl.Middleware(ratelimit.PerIP(5, time.Second)), ctl.parseJd)

	// 先注册静态段 unfinished，避免被 :sessionId 通配吞掉（gin 静态优先，仍按序靠前声明）。
	h.GET("/sessions/unfinished/:resumeId", ctl.findUnfinishedSession)

	// 创建限流：全局 5/s + IP 5/s，对齐 Java 双 @RateLimit 注解（可重复注解全部生效）。
	h.POST("/sessions", ctl.rl.Middleware(
		ratelimit.Global(5, time.Second),
		ratelimit.PerIP(5, time.Second),
	), ctl.createSession)
	h.GET("/sessions", ctl.listSessions)
	h.GET("/sessions/:sessionId", ctl.getSession)
	// 问答闭环（票 04）：提交答案全局限流 10/s（对齐 Java @RateLimit(GLOBAL, 10)）；
	// 取题 / 暂存（PUT）/ 提前交卷与 Java 一致无 @RateLimit（笔记 03 §5）。
	h.GET("/sessions/:sessionId/question", ctl.getCurrentQuestion)
	h.POST("/sessions/:sessionId/answers", ctl.rl.Middleware(ratelimit.Global(10, time.Second)), ctl.submitAnswer)
	h.PUT("/sessions/:sessionId/answers", ctl.saveAnswer)
	h.POST("/sessions/:sessionId/complete", ctl.completeSession)
	// 报告/明细（票 06）：与 Java 一致无 @RateLimit（笔记 03 §5）。
	h.GET("/sessions/:sessionId/report", ctl.getReport)
	h.GET("/sessions/:sessionId/details", ctl.getDetails)
	// 报告 PDF 导出（票 08）：与 Java 一致无 @RateLimit。
	h.GET("/sessions/:sessionId/export", ctl.exportPDF)
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

// ===== 问答闭环（票 04）=====

// bindAnswerRequest PUT/POST /sessions/{id}/answers 共用请求体绑定：
// JSON 非法 → 400；questionIndex 缺失由 service 显式校验（answerIndexOf）。
func bindAnswerRequest(c *gin.Context) (AnswerRequest, bool) {
	var req AnswerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, 400, "请求体格式不正确")
		return req, false
	}
	return req, true
}

func (ctl *Controller) getCurrentQuestion(c *gin.Context) {
	data, err := ctl.sessions.GetCurrentQuestion(c.Request.Context(), c.Param("sessionId"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) submitAnswer(c *gin.Context) {
	req, ok := bindAnswerRequest(c)
	if !ok {
		return
	}
	data, err := ctl.sessions.SubmitAnswer(c.Request.Context(), c.Param("sessionId"), req)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) saveAnswer(c *gin.Context) {
	req, ok := bindAnswerRequest(c)
	if !ok {
		return
	}
	if err := ctl.sessions.SaveAnswer(c.Request.Context(), c.Param("sessionId"), req); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) completeSession(c *gin.Context) {
	if err := ctl.sessions.CompleteSession(c.Request.Context(), c.Param("sessionId")); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

// ===== 报告/明细（票 06）=====

func (ctl *Controller) getReport(c *gin.Context) {
	data, err := ctl.report.GetReport(c.Request.Context(), c.Param("sessionId"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) getDetails(c *gin.Context) {
	data, err := ctl.report.GetInterviewDetail(c.Request.Context(), c.Param("sessionId"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

// exportPDF GET /sessions/{id}/report 导出 PDF（票 08）：成功返回二进制流
// （Content-Type application/pdf + RFC 5987 文件名）；失败按裁决 #10 用 Result 包装
// （HTTP 200 + 业务码），不复制 Java 的空 body 500。
func (ctl *Controller) exportPDF(c *gin.Context) {
	sessionID := c.Param("sessionId")
	in, err := ctl.report.GetInterviewExportInput(c.Request.Context(), sessionID)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	pdfBytes, err := pdfexport.ExportInterviewReport(*in)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	// 中文文件名：RFC 5987 filename*（percent-encoded UTF-8）+ ASCII 回退 filename（兼容旧客户端）。
	encoded := url.PathEscape("模拟面试报告_" + sessionID + ".pdf")
	c.Header("Content-Disposition", `attachment; filename="interview-report_`+sessionID+`.pdf"; filename*=UTF-8''`+encoded)
	c.Data(200, "application/pdf", pdfBytes)
}
