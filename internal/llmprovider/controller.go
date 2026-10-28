package llmprovider

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/ratelimit"
	"github.com/wuzuxian/interview/internal/result"
)

// Controller 设置模块路由控制器，路径与 Java 版 LlmProviderController 一致。
type Controller struct {
	svc *Service
	rl  *ratelimit.Limiter
}

// NewController 创建控制器。
func NewController(svc *Service, rl *ratelimit.Limiter) *Controller {
	return &Controller{svc: svc, rl: rl}
}

// RegisterRoutes 注册 /api/llm-provider 全部路由。
// 限流参数与 Java 版 @RateLimit 注解一致（次数/秒）。
func (ctl *Controller) RegisterRoutes(g *gin.RouterGroup) {
	h := g.Group("/llm-provider")

	h.GET("/list", ctl.rl.Middleware(ratelimit.Global(30, time.Second)), ctl.list)
	h.GET("/:id", ctl.rl.Middleware(ratelimit.Global(30, time.Second)), ctl.get)
	h.POST("", ctl.rl.Middleware(ratelimit.Global(5, time.Second)), ctl.create)
	h.PUT("/:id", ctl.rl.Middleware(ratelimit.Global(5, time.Second)), ctl.update)
	h.DELETE("/:id", ctl.rl.Middleware(ratelimit.Global(5, time.Second)), ctl.remove)
	h.POST("/:id/test", ctl.rl.Middleware(ratelimit.Global(10, time.Second)), ctl.test)
	h.POST("/reload", ctl.rl.Middleware(ratelimit.Global(5, time.Second)), ctl.reload)
	h.GET("/default-provider", ctl.rl.Middleware(ratelimit.Global(30, time.Second)), ctl.getDefault)
	h.PUT("/default-provider", ctl.rl.Middleware(ratelimit.Global(5, time.Second)), ctl.updateDefault)
	h.PUT("/default-embedding-provider", ctl.rl.Middleware(ratelimit.Global(5, time.Second)), ctl.updateDefaultEmbedding)

	// Voice ASR/TTS
	h.GET("/voice/asr", ctl.rl.Middleware(ratelimit.Global(30, time.Second)), ctl.getAsr)
	h.PUT("/voice/asr", ctl.rl.Middleware(ratelimit.Global(5, time.Second)), ctl.updateAsr)
	h.GET("/voice/tts", ctl.rl.Middleware(ratelimit.Global(30, time.Second)), ctl.getTts)
	h.PUT("/voice/tts", ctl.rl.Middleware(ratelimit.Global(5, time.Second)), ctl.updateTts)
	h.POST("/voice/asr/test", ctl.rl.Middleware(ratelimit.Global(10, time.Second)), ctl.testAsr)
}

func (ctl *Controller) list(c *gin.Context) {
	data, err := ctl.svc.ListProviders()
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) get(c *gin.Context) {
	data, err := ctl.svc.GetProvider(c.Param("id"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) create(c *gin.Context) {
	var req CreateProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, 400, "请求参数错误: "+err.Error())
		return
	}
	if err := ctl.svc.CreateProvider(c.Request.Context(), &req); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) update(c *gin.Context) {
	var req UpdateProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, 400, "请求参数错误: "+err.Error())
		return
	}
	if err := ctl.svc.UpdateProvider(c.Request.Context(), c.Param("id"), &req); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) remove(c *gin.Context) {
	if err := ctl.svc.DeleteProvider(c.Request.Context(), c.Param("id")); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) test(c *gin.Context) {
	data, err := ctl.svc.TestProvider(c.Request.Context(), c.Param("id"))
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) reload(c *gin.Context) {
	if err := ctl.svc.ReloadProviders(); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) getDefault(c *gin.Context) {
	data, err := ctl.svc.GetDefaultProvider()
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) updateDefault(c *gin.Context) {
	var req DefaultProviderDTO
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, 400, "请求参数错误: "+err.Error())
		return
	}
	if err := ctl.svc.UpdateDefaultProvider(c.Request.Context(), &req); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) updateDefaultEmbedding(c *gin.Context) {
	var req DefaultProviderDTO
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, 400, "请求参数错误: "+err.Error())
		return
	}
	if err := ctl.svc.UpdateDefaultEmbeddingProvider(c.Request.Context(), &req); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) getAsr(c *gin.Context) {
	data, err := ctl.svc.GetAsrConfig()
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) updateAsr(c *gin.Context) {
	var req AsrConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, 400, "请求参数错误: "+err.Error())
		return
	}
	if err := ctl.svc.UpdateAsrConfig(c.Request.Context(), &req); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) getTts(c *gin.Context) {
	data, err := ctl.svc.GetTtsConfig()
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}

func (ctl *Controller) updateTts(c *gin.Context) {
	var req TtsConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		result.Fail(c, 400, "请求参数错误: "+err.Error())
		return
	}
	if err := ctl.svc.UpdateTtsConfig(c.Request.Context(), &req); err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, nil)
}

func (ctl *Controller) testAsr(c *gin.Context) {
	data, err := ctl.svc.TestAsrConfig(c.Request.Context())
	if err != nil {
		result.FailErr(c, err)
		return
	}
	result.OK(c, data)
}
