// HTTP 层：/api/interview-schedule 全部路由（POST ""、GET ""、GET/PUT/DELETE
// "/:id"、PATCH|PUT "/:id/status"）与入参校验。响应统一 result 封装（HTTP 200 +
// code 区分）；校验失败 400、消息为字段文案逗号拼接（对齐 Java @Valid +
// GlobalExceptionHandler 的 MethodArgumentNotValidException 处理），路径 ID 非法
// 400「无效的ID参数」（kb parsePathID 同口径）。
package interviewschedule

import (
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/result"
)

// Controller 日程控制器。
type Controller struct {
	svc      *Service
	parseSvc *ParseService
}

// NewController 创建控制器。
func NewController(svc *Service, parseSvc *ParseService) *Controller {
	return &Controller{svc: svc, parseSvc: parseSvc}
}

// RegisterRoutes 注册路由（基路径 /api/interview-schedule，与 Java
// @RequestMapping 对齐；状态接口 PATCH 与 PUT 双 method 同 handler，Java
// @RequestMapping(method={PATCH, PUT})）。
func (h *Controller) RegisterRoutes(g *gin.RouterGroup) {
	r := g.Group("/interview-schedule")
	r.POST("/parse", h.parse)
	r.POST("", h.create)
	r.GET("", h.list)
	r.GET("/:id", h.getByID)
	r.PUT("/:id", h.update)
	r.DELETE("/:id", h.remove)
	r.PATCH("/:id/status", h.updateStatus)
	r.PUT("/:id/status", h.updateStatus)
}

// parse 解析面试邀约文本（rawText 必填，@NotBlank「文本不能为空」）。
func (h *Controller) parse(c *gin.Context) {
	var req ParseRequest
	if !bindJSON(c, &req) {
		return
	}
	if strings.TrimSpace(req.RawText) == "" {
		result.Fail(c, apperr.CodeBadRequest, "文本不能为空")
		return
	}
	source := ""
	if req.Source != nil {
		source = *req.Source
	}
	result.OK(c, h.parseSvc.Parse(c.Request.Context(), req.RawText, source))
}

// create 创建日程。
func (h *Controller) create(c *gin.Context) {
	var req CreateInterviewRequest
	if !bindJSON(c, &req) {
		return
	}
	if msgs := validateCreateRequest(&req); len(msgs) > 0 {
		result.Fail(c, apperr.CodeBadRequest, strings.Join(msgs, ", "))
		return
	}
	dto, err := h.svc.Create(c.Request.Context(), &req)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dto)
}

// getByID 详情。
func (h *Controller) getByID(c *gin.Context) {
	id, ok := parsePathID(c)
	if !ok {
		return
	}
	dto, err := h.svc.GetByID(c.Request.Context(), id)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dto)
}

// list 列表（status/start/end 全部可选）。
func (h *Controller) list(c *gin.Context) {
	var status *string
	if v := c.Query("status"); v != "" {
		status = &v
	}
	start, ok := parseQueryTime(c, "start")
	if !ok {
		return
	}
	end, ok := parseQueryTime(c, "end")
	if !ok {
		return
	}
	dtos, err := h.svc.GetAll(c.Request.Context(), status, start, end)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dtos)
}

// update 整条更新。
func (h *Controller) update(c *gin.Context) {
	id, ok := parsePathID(c)
	if !ok {
		return
	}
	var req CreateInterviewRequest
	if !bindJSON(c, &req) {
		return
	}
	if msgs := validateCreateRequest(&req); len(msgs) > 0 {
		result.Fail(c, apperr.CodeBadRequest, strings.Join(msgs, ", "))
		return
	}
	dto, err := h.svc.Update(c.Request.Context(), id, &req)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dto)
}

// remove 删除（物理删，不存在恒成功）。
func (h *Controller) remove(c *gin.Context) {
	id, ok := parsePathID(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), id); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

// updateStatus 更新状态（query 参数 status 必填、限四枚举）。
func (h *Controller) updateStatus(c *gin.Context) {
	id, ok := parsePathID(c)
	if !ok {
		return
	}
	raw := c.Query("status")
	if raw == "" {
		result.Fail(c, apperr.CodeBadRequest, "缺少必填参数: status")
		return
	}
	st, ok := ParseInterviewStatus(raw)
	if !ok {
		result.Fail(c, apperr.CodeBadRequest, "无效的面试状态: "+raw)
		return
	}
	dto, err := h.svc.UpdateStatus(c.Request.Context(), id, st)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dto)
}

// bindJSON 请求体绑定（非法 JSON → 400「请求参数错误」，kb 同口径）。
func bindJSON(c *gin.Context, req any) bool {
	if err := c.ShouldBindJSON(req); err != nil {
		result.Fail(c, apperr.CodeBadRequest, "请求参数错误")
		return false
	}
	return true
}

// validateCreateRequest 字段校验（消息逐字对齐 Java @NotBlank/@NotNull 文案，
// 顺序按字段声明序，GlobalExceptionHandler 以 ", " 连接）。
func validateCreateRequest(req *CreateInterviewRequest) []string {
	var msgs []string
	if strings.TrimSpace(req.CompanyName) == "" {
		msgs = append(msgs, "公司名称不能为空")
	}
	if strings.TrimSpace(req.Position) == "" {
		msgs = append(msgs, "岗位不能为空")
	}
	if req.InterviewTime.IsZero() {
		msgs = append(msgs, "面试时间不能为空")
	}
	return msgs
}

// parsePathID 路径 ID 解析（Long.parseLong 语义，非数字/溢出/负数 → 400
// 「无效的ID参数」，kb 先例）。
func parsePathID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 0 {
		result.Fail(c, apperr.CodeBadRequest, "无效的ID参数")
		return 0, false
	}
	return id, true
}

// parseQueryTime 列表时间参数解析（Java @DateTimeFormat(ISO.DATE_TIME)；前端恒发
// 无时区 ISO 串，接受 16/19 位；非法 → 400，Java 侧为类型转换失败落兜底 500，
// 前端不可达路径，记偏离清单）。
func parseQueryTime(c *gin.Context, name string) (*time.Time, bool) {
	raw := c.Query(name)
	if raw == "" {
		return nil, true
	}
	for _, layout := range []string{naiveTimeFormatSec, naiveTimeFormatMin} {
		if v, err := time.Parse(layout, raw); err == nil {
			t := v
			return &t, true
		}
	}
	result.Fail(c, apperr.CodeBadRequest, "无效的时间参数: "+raw)
	return nil, false
}
