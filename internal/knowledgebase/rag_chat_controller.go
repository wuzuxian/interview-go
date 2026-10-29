// RAG 会话控制器（Java RagChatController 逐字对齐）：8 个接口。
// 发消息流式：先同步落库用户消息 + AI 占位，SSE data-only 事件换行转义为字面
// \n；完成回写全文，取消保存已生成部分（空则固定中断文案）；上游错误已由查询
// 管线 onErrorResume 转为流内固定错误文案，无独立错误分支。
package knowledgebase

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/result"
)

// 客户端中断时的固定落库文案（Java doOnCancel 逐字）。上游错误已由查询管线
// onErrorResume 转为流内固定错误文案（query_service.go streamErrorText），
// Go 侧无独立 doOnError 分支。
const streamCancelledText = "【已中断】回答生成已取消"

// RagChatController RAG 会话路由控制器。
type RagChatController struct {
	svc    *RagChatService
	logger *slog.Logger
}

// NewRagChatController 创建控制器。
func NewRagChatController(svc *RagChatService, logger *slog.Logger) *RagChatController {
	return &RagChatController{svc: svc, logger: logger}
}

// RegisterRoutes 注册 /api/rag-chat 路由（方法与路径对齐 Java 注解）。
func (ctl *RagChatController) RegisterRoutes(g *gin.RouterGroup) {
	h := g.Group("/rag-chat")
	h.POST("/sessions", ctl.createSession)
	h.GET("/sessions", ctl.listSessions)
	h.GET("/sessions/:id", ctl.sessionDetail)
	h.PUT("/sessions/:id/title", ctl.updateTitle)
	h.PUT("/sessions/:id/pin", ctl.togglePin)
	h.PUT("/sessions/:id/knowledge-bases", ctl.updateKnowledgeBases)
	h.DELETE("/sessions/:id", ctl.deleteSession)
	h.POST("/sessions/:id/messages/stream", ctl.sendMessageStream)
}

// createSession POST /api/rag-chat/sessions（kbIds 必填、标题可选）。
func (ctl *RagChatController) createSession(c *gin.Context) {
	var body struct {
		KnowledgeBaseIDs []int64 `json:"knowledgeBaseIds"`
		Title            *string `json:"title"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		result.Fail(c, 400, "请求体格式不正确")
		return
	}
	if len(body.KnowledgeBaseIDs) == 0 {
		result.Fail(c, 400, "至少选择一个知识库")
		return
	}
	title := ""
	if body.Title != nil {
		title = *body.Title
	}
	dto, err := ctl.svc.CreateSession(c.Request.Context(), body.KnowledgeBaseIDs, title)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dto)
}

// listSessions GET /api/rag-chat/sessions（置顶优先、含消息数与知识库名）。
func (ctl *RagChatController) listSessions(c *gin.Context) {
	list, err := ctl.svc.ListSessions(c.Request.Context())
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, list)
}

// sessionDetail GET /api/rag-chat/sessions/{id}（含消息）。
func (ctl *RagChatController) sessionDetail(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	dto, err := ctl.svc.GetSessionDetail(c.Request.Context(), id)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, dto)
}

// updateTitle PUT /api/rag-chat/sessions/{id}/title（标题非空校验对齐 @NotBlank）。
func (ctl *RagChatController) updateTitle(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var body struct {
		Title *string `json:"title"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		result.Fail(c, 400, "请求体格式不正确")
		return
	}
	if body.Title == nil || strings.TrimSpace(*body.Title) == "" {
		result.Fail(c, 400, "标题不能为空")
		return
	}
	if err := ctl.svc.UpdateSessionTitle(c.Request.Context(), id, *body.Title); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

// togglePin PUT /api/rag-chat/sessions/{id}/pin。
func (ctl *RagChatController) togglePin(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if _, err := ctl.svc.ToggleSessionPin(c.Request.Context(), id); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

// updateKnowledgeBases PUT /api/rag-chat/sessions/{id}/knowledge-bases。
func (ctl *RagChatController) updateKnowledgeBases(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var body struct {
		KnowledgeBaseIDs []int64 `json:"knowledgeBaseIds"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		result.Fail(c, 400, "请求体格式不正确")
		return
	}
	if len(body.KnowledgeBaseIDs) == 0 {
		result.Fail(c, 400, "至少选择一个知识库")
		return
	}
	if err := ctl.svc.UpdateSessionKnowledgeBases(c.Request.Context(), id, body.KnowledgeBaseIDs); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

// deleteSession DELETE /api/rag-chat/sessions/{id}。
func (ctl *RagChatController) deleteSession(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	if err := ctl.svc.DeleteSession(c.Request.Context(), id); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

// sendMessageStream POST /api/rag-chat/sessions/{id}/messages/stream（SSE）。
// 行为对齐 Java：先同步 prepareStreamMessage（用户消息 + AI 占位，404 直接以
// Result JSON 返回），随后流式输出；换行转义字面 \n；完成回写全文，取消/错误
// 保存已生成部分（空则固定文案）。
func (ctl *RagChatController) sendMessageStream(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	var body struct {
		Question *string `json:"question"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		result.Fail(c, 400, "请求体格式不正确")
		return
	}
	if body.Question == nil || strings.TrimSpace(*body.Question) == "" {
		result.Fail(c, 400, "问题不能为空")
		return
	}
	question := *body.Question

	messageID, err := ctl.svc.PrepareStreamMessage(c.Request.Context(), id, question)
	if err != nil {
		result.FailErr(c, err)
		return
	}
	ch, err := ctl.svc.StreamAnswer(c.Request.Context(), id, question)
	if err != nil {
		result.FailErr(c, err)
		return
	}

	startSSE(c)
	var full strings.Builder
	for {
		select {
		case <-c.Request.Context().Done():
			// 客户端中断：保存已生成部分，空则固定中断文案（Java doOnCancel）。
			ctl.completeDetached(messageID, full.String(), streamCancelledText)
			return
		case chunk, ok := <-ch:
			if !ok {
				// 流式完成：回写全文（Java doOnComplete）。
				ctl.completeDetached(messageID, full.String(), "")
				ctl.logger.Info("RAG 聊天流式完成",
					slog.Int64("sessionId", id), slog.Int64("messageId", messageID))
				return
			}
			full.WriteString(chunk.Text)
			if !writeSSEData(c, chunk.Text, true) {
				// 连接已断：与取消同语义，保存已生成部分（Java doOnCancel）。
				ctl.completeDetached(messageID, full.String(), streamCancelledText)
				return
			}
		}
	}
}

// completeDetached 落库回写使用独立短超时上下文：客户端断开后请求 ctx 已取消，
// 回写仍须落库（Java doOnCancel/doOnError 在连接关闭后照常执行）。
func (ctl *RagChatController) completeDetached(messageID int64, content, fallback string) {
	if content == "" {
		content = fallback
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ctl.svc.CompleteStreamMessage(ctx, messageID, content); err != nil {
		ctl.logger.Error("回写流式消息失败",
			slog.Int64("messageId", messageID), slog.String("error", err.Error()))
	}
}
