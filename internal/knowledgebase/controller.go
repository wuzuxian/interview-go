// 知识库控制器：路由、限流与响应封装对齐 Java KnowledgeBaseController。
// 限流：upload 3/3、revectorize 2/2（@RateLimit 注解数值；窗口语义沿仓库
// ratelimit 固定窗口先例，与 Java 滑动窗口的差异见汇报）；其余管理接口
// （list/detail/delete/分类/搜索/统计/下载）在 Java 侧无限流注解，不加中间件。
// 除下载直出文件字节（ResponseEntity<byte[]>）外，全部走统一 Result 封装。
package knowledgebase

import (
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/ratelimit"
	"github.com/wuzuxian/interview/internal/result"
)

// Controller 知识库模块路由控制器。
type Controller struct {
	upload  *UploadService
	list    *ListService
	deleter *DeleteService
	query   *QueryService
	rl      *ratelimit.Limiter
}

// NewController 创建控制器。
func NewController(upload *UploadService, list *ListService, deleter *DeleteService, query *QueryService, rl *ratelimit.Limiter) *Controller {
	return &Controller{upload: upload, list: list, deleter: deleter, query: query, rl: rl}
}

// RegisterRoutes 注册 /api/knowledgebase 路由（方法与路径对齐 Java 注解）。
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

	h.GET("/list", ctl.listKnowledgeBases)
	h.GET("/search", ctl.search)
	h.GET("/stats", ctl.stats)
	h.POST("/query", ctl.rl.Middleware(
		ratelimit.Global(10, time.Second),
		ratelimit.PerIP(10, time.Second),
	), ctl.queryKnowledgeBase)
	h.POST("/query/stream", ctl.rl.Middleware(
		ratelimit.Global(5, time.Second),
		ratelimit.PerIP(5, time.Second),
	), ctl.queryKnowledgeBaseStream)
	h.GET("/categories", ctl.categories)
	h.GET("/category/:category", ctl.listByCategory)
	h.GET("/uncategorized", ctl.uncategorized)
	h.GET("/:id", ctl.detail)
	h.GET("/:id/download", ctl.download)
	h.DELETE("/:id", ctl.deleteKnowledgeBase)
	h.PUT("/:id/category", ctl.updateCategory)
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

// listKnowledgeBases GET /api/knowledgebase/list?sortBy=&vectorStatus=
// （Java：vectorStatus 非空白时按枚举大写解析，非法 → 500「无效的向量化状态: 原值」）。
func (ctl *Controller) listKnowledgeBases(c *gin.Context) {
	list, err := ctl.list.List(c.Request.Context(), c.Query("vectorStatus"), c.Query("sortBy"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, list)
}

// detail GET /api/knowledgebase/{id}（不存在 → 500「知识库不存在」，Java 契约照抄）。
func (ctl *Controller) detail(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	dto, err := ctl.list.Get(c.Request.Context(), id)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dto)
}

// deleteKnowledgeBase DELETE /api/knowledgebase/{id}。
func (ctl *Controller) deleteKnowledgeBase(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if err := ctl.deleter.Delete(c.Request.Context(), id); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

// categories GET /api/knowledgebase/categories。
func (ctl *Controller) categories(c *gin.Context) {
	list, err := ctl.list.Categories(c.Request.Context())
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, list)
}

// listByCategory GET /api/knowledgebase/category/{category}。
func (ctl *Controller) listByCategory(c *gin.Context) {
	list, err := ctl.list.ListByCategory(c.Request.Context(), c.Param("category"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, list)
}

// uncategorized GET /api/knowledgebase/uncategorized。
func (ctl *Controller) uncategorized(c *gin.Context) {
	list, err := ctl.list.Uncategorized(c.Request.Context())
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, list)
}

// updateCategory PUT /api/knowledgebase/{id}/category（body {category}，空白置 NULL）。
func (ctl *Controller) updateCategory(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var body struct {
		Category *string `json:"category"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		result.Fail(c, 400, "请求参数错误")
		return
	}
	category := ""
	if body.Category != nil {
		category = *body.Category
	}
	if err := ctl.list.UpdateCategory(c.Request.Context(), id, category); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

// download GET /api/knowledgebase/{id}/download：直出文件字节（Java
// ResponseEntity<byte[]>，不走 Result 封装），Content-Disposition 带 UTF-8 编码文件名。
func (ctl *Controller) download(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	dl, err := ctl.list.Download(c.Request.Context(), id)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	encoded := javaURLEncode(dl.Filename)
	disposition := "attachment; filename=\"" + encoded + "\"; filename*=UTF-8''" + encoded
	contentType := dl.ContentType
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}
	c.Header("Content-Disposition", disposition)
	c.Data(200, contentType, dl.Data)
}

// search GET /api/knowledgebase/search?keyword=。
func (ctl *Controller) search(c *gin.Context) {
	list, err := ctl.list.Search(c.Request.Context(), c.Query("keyword"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, list)
}

// stats GET /api/knowledgebase/stats。
func (ctl *Controller) stats(c *gin.Context) {
	dto, err := ctl.list.Stats(c.Request.Context())
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dto)
}

// queryKnowledgeBase POST /api/knowledgebase/query（同步问答）。
// 参数校验对齐 Java @Valid：违规消息按字段声明顺序以 ", " 连接（400）。
func (ctl *Controller) queryKnowledgeBase(c *gin.Context) {
	req, ok := bindQueryRequest(c)
	if !ok {
		return
	}
	resp, err := ctl.query.QueryKnowledgeBase(c.Request.Context(), req)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, resp)
}

// queryKnowledgeBaseStream POST /api/knowledgebase/query/stream（流式 SSE，
// data-only 事件，多行内容按 SSE 标准拆分为多个 data: 行）。
func (ctl *Controller) queryKnowledgeBaseStream(c *gin.Context) {
	req, ok := bindQueryRequest(c)
	if !ok {
		return
	}
	startSSE(c)
	for chunk := range ctl.query.AnswerQuestionStream(c.Request.Context(), req.KnowledgeBaseIDs, req.Question, nil) {
		if c.Request.Context().Err() != nil {
			return
		}
		if !writeSSEData(c, chunk.Text, false) {
			return
		}
	}
}

// bindQueryRequest 解析并校验查询请求体（Java QueryRequest @NotEmpty/@NotBlank）。
func bindQueryRequest(c *gin.Context) (QueryRequest, bool) {
	var req QueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, 400, "请求体格式不正确")
		return req, false
	}
	var violations []string
	if len(req.KnowledgeBaseIDs) == 0 {
		violations = append(violations, "至少选择一个知识库")
	}
	if strings.TrimSpace(req.Question) == "" {
		violations = append(violations, "问题不能为空")
	}
	if len(violations) > 0 {
		result.Fail(c, 400, strings.Join(violations, ", "))
		return req, false
	}
	return req, true
}

// startSSE 设置流式响应头（Java produces = text/event-stream）。
func startSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
}

// writeSSEData 写入一个 data-only 事件并刷新。
// escapeNewlines=true（RAG 会话流）：换行转义为字面 \n、\r 转义为字面 \r，
// 事件保持单 data 行（Java ServerSentEvent 包装语义）；
// false（query 流）：按 SSE 标准把多行内容拆为多个 data: 行（WebFlux 对
// Flux<String> 的编码语义），前端逐行解析。
func writeSSEData(c *gin.Context, data string, escapeNewlines bool) bool {
	payload := data
	if escapeNewlines {
		payload = strings.ReplaceAll(data, "\n", "\\n")
		payload = strings.ReplaceAll(payload, "\r", "\\r")
		if _, err := c.Writer.WriteString("data:" + payload + "\n\n"); err != nil {
			return false
		}
	} else {
		for _, line := range strings.Split(payload, "\n") {
			if _, err := c.Writer.WriteString("data:" + line + "\n"); err != nil {
				return false
			}
		}
		if _, err := c.Writer.WriteString("\n"); err != nil {
			return false
		}
	}
	c.Writer.Flush()
	return true
}

// javaURLEncode 复刻 java.net.URLEncoder.encode(s, UTF_8) + replace("+", "%20")：
// 保留字母数字与 .-*_，空格编码为 %20（URLEncoder 的 + 再替换），其余按 UTF-8
// 字节输出大写百分号编码——下载文件名 Content-Disposition 与 Java 逐字节一致。
func javaURLEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '.', ch == '-', ch == '*', ch == '_':
			b.WriteByte(ch)
		case ch == ' ':
			b.WriteString("%20")
		default:
			b.WriteByte('%')
			b.WriteByte(hex[ch>>4])
			b.WriteByte(hex[ch&0x0F])
		}
	}
	return b.String()
}

// parseID 路径参数 id 解析为 int64（Java @PathVariable Long 的 Long.parseLong
// 语义）：非数字、超出 int64 范围与负数均返回 400「无效的ID参数」。
func parseID(c *gin.Context) (int64, bool) {
	return parsePathID(c, "id")
}

// parsePathID 路径 ID 解析公共实现（Long.parseLong 语义，另按评审要求拒绝负数）；
// id 与 questionId 两路径参数共用。
func parsePathID(c *gin.Context, param string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(param), 10, 64)
	if err != nil || id < 0 {
		result.Fail(c, apperr.CodeBadRequest, "无效的ID参数")
		return 0, false
	}
	return id, true
}
