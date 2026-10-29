// 路由注册守卫：REST 端点必须挂在与 Java/前端契约完全一致的路径上。
// DTO 契约 golden 只锁 JSON 字段名，锁不住「路由挂在什么路径」——gin 路由
// 前缀是编译期字面量，路径漂移只有路由表测试能抓住。背景：2026-10-29 排查
// 「语音面试 API 不存在」，根因是部署了不含本模块的旧二进制（main 分支构建），
// 顺带补上本守卫，防止路径设计偏离前端约定成为同类盲区。
package voiceinterview

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRegisterRoutesUnderFrontendContractPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// RegisterRoutes 不触碰 svc，零值控制器即可完成注册。
	NewController(nil).RegisterRoutes(r.Group("/api"))

	// 与 Java VoiceInterviewController 逐字对齐的十端点（前端按此路径调用）。
	want := []string{
		"POST /api/voice-interview/sessions",
		"GET /api/voice-interview/sessions",
		"GET /api/voice-interview/sessions/:sessionId",
		"POST /api/voice-interview/sessions/:sessionId/end",
		"PUT /api/voice-interview/sessions/:sessionId/pause",
		"PUT /api/voice-interview/sessions/:sessionId/resume",
		"DELETE /api/voice-interview/sessions/:sessionId",
		"GET /api/voice-interview/sessions/:sessionId/messages",
		"GET /api/voice-interview/sessions/:sessionId/evaluation",
		"POST /api/voice-interview/sessions/:sessionId/evaluation",
	}
	got := make(map[string]bool, len(r.Routes()))
	for _, rt := range r.Routes() {
		got[rt.Method+" "+rt.Path] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("契约路由缺失: %s", w)
		}
	}
}
