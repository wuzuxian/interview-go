package resume

import (
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/pdfexport"
	"github.com/wuzuxian/interview/internal/ratelimit"
	"github.com/wuzuxian/interview/internal/result"
	"github.com/wuzuxian/interview/internal/storage"
)

// Controller 简历模块路由控制器，路径与响应结构对齐 Java 版 ResumeController。
type Controller struct {
	upload  *Uploader
	delete  *DeleteService
	history *HistoryService
	storage *storage.Service
	rl      *ratelimit.Limiter
}

// NewController 创建控制器。
func NewController(upload *Uploader, del *DeleteService, history *HistoryService, st *storage.Service, rl *ratelimit.Limiter) *Controller {
	return &Controller{upload: upload, delete: del, history: history, storage: st, rl: rl}
}

// RegisterRoutes 注册 /api/resumes 全部路由。
func (ctl *Controller) RegisterRoutes(g *gin.RouterGroup) {
	h := g.Group("/resumes")

	// 上传限流：全局 5/s + IP 5/s，与 Java @RateLimit 一致
	h.POST("/upload", ctl.rl.Middleware(
		ratelimit.Global(5, time.Second),
		ratelimit.PerIP(5, time.Second),
	), ctl.uploadAndAnalyze)

	h.GET("", ctl.list)
	h.GET("/statistics", ctl.statistics)
	h.GET("/health", ctl.health)
	h.GET("/:id/detail", ctl.detail)
	h.GET("/:id/export", ctl.exportPDF)
	h.DELETE("/:id", ctl.remove)
	h.POST("/:id/reanalyze", ctl.rl.Middleware(
		ratelimit.Global(2, time.Second),
		ratelimit.PerIP(2, time.Second),
	), ctl.reanalyze)
}

func (ctl *Controller) uploadAndAnalyze(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		result.Fail(c, 400, "请选择要上传的简历文件")
		return
	}
	f, err := fileHeader.Open()
	if err != nil {
		result.Fail(c, 400, "读取上传文件失败")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		result.Fail(c, 400, "读取上传文件失败")
		return
	}

	resp, err := ctl.upload.UploadAndAnalyze(c.Request.Context(), fileHeader.Filename, fileHeader.Header.Get("Content-Type"), data)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	if resp.Duplicate {
		result.OKMessage(c, "检测到相同简历，已返回历史分析结果", resp)
		return
	}
	result.OK(c, resp)
}

func (ctl *Controller) list(c *gin.Context) {
	data, err := ctl.history.GetAllResumes(c.Request.Context())
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) statistics(c *gin.Context) {
	data, err := ctl.history.GetStatistics(c.Request.Context())
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) detail(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	data, err := ctl.history.GetResumeDetail(c.Request.Context(), id)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) exportPDF(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	resume, analysis, err := ctl.history.ExportAnalysis(c.Request.Context(), id)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	report := &pdfexport.AnalysisReport{
		OverallScore: analysis.OverallScore,
		ScoreDetail: &pdfexport.ScoreDetail{
			ContentScore:    analysis.ScoreDetail.ContentScore,
			StructureScore:  analysis.ScoreDetail.StructureScore,
			SkillMatchScore: analysis.ScoreDetail.SkillMatchScore,
			ExpressionScore: analysis.ScoreDetail.ExpressionScore,
			ProjectScore:    analysis.ScoreDetail.ProjectScore,
		},
		Summary:     analysis.Summary,
		Strengths:   analysis.Strengths,
		Suggestions: toPDFSuggestions(analysis.Suggestions),
	}
	uploadedAt := resume.UploadedAt
	pdfBytes, err := pdfexport.ExportResumeAnalysis(pdfexport.ReportInput{
		Filename:   resume.OriginalFilename,
		UploadedAt: &uploadedAt,
		Analysis:   report,
	})
	if err != nil {
		result.FailErr(c, err)
		return
	}
	filename := url.PathEscape(fmt.Sprintf("简历分析报告_%s.pdf", resume.OriginalFilename))
	c.Header("Content-Disposition", "attachment; filename*=UTF-8''"+filename)
	c.Data(200, "application/pdf", pdfBytes)
}

func toPDFSuggestions(in []Suggestion) []pdfexport.Suggestion {
	out := make([]pdfexport.Suggestion, 0, len(in))
	for _, s := range in {
		out = append(out, pdfexport.Suggestion{
			Category:       s.Category,
			Priority:       s.Priority,
			Issue:          s.Issue,
			Recommendation: s.Recommendation,
		})
	}
	return out
}

func (ctl *Controller) remove(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if err := ctl.delete.DeleteResume(c.Request.Context(), id); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) reanalyze(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if err := ctl.upload.Reanalyze(c.Request.Context(), id); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) health(c *gin.Context) {
	result.OK(c, gin.H{
		"status":  "UP",
		"service": "AI Interview Platform - Resume Service",
	})
}

// parseID 解析路径参数 id 并在非法时写出错误响应。
func parseID(c *gin.Context) (int64, bool) {
	var id int64
	if _, err := fmt.Sscanf(c.Param("id"), "%d", &id); err != nil || id <= 0 {
		result.Fail(c, 400, "无效的简历ID")
		return 0, false
	}
	return id, true
}
