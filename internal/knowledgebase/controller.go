// 知识库控制器（票 01 范围：上传 + 重新向量化），路由与限流对齐 Java
// KnowledgeBaseController（upload 3/3、revectorize 2/2，窗口语义沿仓库
// ratelimit 固定窗口先例）；其余管理接口随票 02 接入。
package knowledgebase

import (
	"io"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/ratelimit"
	"github.com/wuzuxian/interview/internal/result"
	"time"
)

// Controller 知识库模块路由控制器。
type Controller struct {
	upload *UploadService
	rl     *ratelimit.Limiter
}

// NewController 创建控制器。
func NewController(upload *UploadService, rl *ratelimit.Limiter) *Controller {
	return &Controller{upload: upload, rl: rl}
}

// RegisterRoutes 注册 /api/knowledgebase 路由。
func (ctl *Controller) RegisterRoutes(g *gin.RouterGroup) {
	h := g.Group("/knowledgebase")
	h.POST("/upload", ctl.rl.Middleware(
		ratelimit.Global(3, time.Second),
		ratelimit.PerIP(3, time.Second),
	), ctl.uploadKnowledgeBase)
	h.POST("/:id/revectorize", ctl.rl.Middleware(
		ratelimit.Global(2, time.Second),
		ratelimit.PerIP(2, time.Second),
	), ctl.revectorize)
}

// uploadKnowledgeBase POST /api/knowledgebase/upload（multipart file + 可选 name/category）。
func (ctl *Controller) uploadKnowledgeBase(c *gin.Context) {
	fileHeader, err := c.FormFile("file")
	if err != nil {
		result.Fail(c, 400, "请选择要上传的知识库文件")
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
	resp, err := ctl.upload.Upload(c.Request.Context(),
		fileHeader.Filename, fileHeader.Header.Get("Content-Type"), data,
		c.PostForm("name"), c.PostForm("category"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, resp)
}

// revectorize POST /api/knowledgebase/{id}/revectorize（手动重试）。
func (ctl *Controller) revectorize(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if err := ctl.upload.Revectorize(c.Request.Context(), id); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

// parseID 路径参数解析为 int64；非法时返回 400。
func parseID(c *gin.Context) (int64, bool) {
	raw := c.Param("id")
	var id int64
	neg := false
	for i, ch := range raw {
		if i == 0 && ch == '-' {
			neg = true
			continue
		}
		if ch < '0' || ch > '9' {
			result.Fail(c, 400, "路径参数 id 非法")
			return 0, false
		}
		id = id*10 + int64(ch-'0')
	}
	if neg {
		id = -id
	}
	return id, true
}
