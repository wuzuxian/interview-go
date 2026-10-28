package ginserver

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// mount 返回路由挂载点：配置了全局 BasePath 时挂在其子组上，否则直接挂引擎。
func (s *Server) mount() *gin.RouterGroup {
	if s.root != nil {
		return s.root
	}
	return &s.engine.RouterGroup
}

// Use 注册全局中间件（作用于所有路由，含 NoRoute/NoMethod）。
// 可一次注册多个，按注册顺序执行；须在路由注册之前调用才能影响请求处理链前置部分。
func (s *Server) Use(mw ...gin.HandlerFunc) {
	s.engine.Use(mw...)
}

// Handle 注册任意方法路由，method 为 "GET"/"POST" 等（大小写不敏感）。
func (s *Server) Handle(method, relativePath string, handlers ...gin.HandlerFunc) {
	s.mount().Handle(strings.ToUpper(method), relativePath, handlers...)
}

// GET 注册 GET 路由，handlers 依序执行，最后一个通常是业务处理器。
func (s *Server) GET(relativePath string, handlers ...gin.HandlerFunc) {
	s.mount().GET(relativePath, handlers...)
}

// POST 注册 POST 路由。
func (s *Server) POST(relativePath string, handlers ...gin.HandlerFunc) {
	s.mount().POST(relativePath, handlers...)
}

// PUT 注册 PUT 路由。
func (s *Server) PUT(relativePath string, handlers ...gin.HandlerFunc) {
	s.mount().PUT(relativePath, handlers...)
}

// PATCH 注册 PATCH 路由。
func (s *Server) PATCH(relativePath string, handlers ...gin.HandlerFunc) {
	s.mount().PATCH(relativePath, handlers...)
}

// DELETE 注册 DELETE 路由。
func (s *Server) DELETE(relativePath string, handlers ...gin.HandlerFunc) {
	s.mount().DELETE(relativePath, handlers...)
}

// HEAD 注册 HEAD 路由。
func (s *Server) HEAD(relativePath string, handlers ...gin.HandlerFunc) {
	s.mount().HEAD(relativePath, handlers...)
}

// OPTIONS 注册 OPTIONS 路由。
func (s *Server) OPTIONS(relativePath string, handlers ...gin.HandlerFunc) {
	s.mount().OPTIONS(relativePath, handlers...)
}

// Any 注册支持全部 HTTP 方法的路由。
func (s *Server) Any(relativePath string, handlers ...gin.HandlerFunc) {
	s.mount().Any(relativePath, handlers...)
}

// Group 创建路由组，返回 gin.RouterGroup，可继续链式注册子路由与组级中间件。
// 路径会在全局 BasePath 基础上追加。
func (s *Server) Group(relativePath string, handlers ...gin.HandlerFunc) *gin.RouterGroup {
	return s.mount().Group(relativePath, handlers...)
}

// NoRoute 注册未匹配任何路由时的处理器（404 兜底）。
func (s *Server) NoRoute(handlers ...gin.HandlerFunc) {
	s.engine.NoRoute(handlers...)
}

// NoMethod 注册命中路由但 HTTP 方法不允许时的处理器（405 兜底）。
// 注意：需 engine.HandleMethodNotAllowed = true 时生效，可通过 WithEngine 自建引擎开启。
func (s *Server) NoMethod(handlers ...gin.HandlerFunc) {
	s.engine.NoMethod(handlers...)
}

// Static 注册静态文件目录。
func (s *Server) Static(relativePath, root string) {
	s.mount().Static(relativePath, root)
}

// StaticFile 注册单个静态文件。
func (s *Server) StaticFile(relativePath, filepath string) {
	s.mount().StaticFile(relativePath, filepath)
}

// StaticFS 注册自定义文件系统。
func (s *Server) StaticFS(relativePath string, fs http.FileSystem) {
	s.mount().StaticFS(relativePath, fs)
}

// Health 注册健康检查端点，返回 200 与 JSON 状态体。
func (s *Server) Health(relativePath string) {
	s.mount().GET(relativePath, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
}

// AllowCORS 是内置的跨域中间件：允许指定来源（完整 origin，如 "https://a.com"），
// 传 "*" 表示允许所有来源。预检请求直接返回 204。
func AllowCORS(allowOrigins ...string) gin.HandlerFunc {
	allowed := "*"
	if len(allowOrigins) > 0 {
		allowed = strings.Join(allowOrigins, ", ")
	}
	allAllowed := allowed == "*"

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			if allAllowed {
				c.Header("Access-Control-Allow-Origin", "*")
			} else if containsOrigin(allowed, origin) {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Vary", "Origin")
			}
			c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization, X-Requested-With")
		}
		if c.Request.Method == http.MethodOptions && c.GetHeader("Access-Control-Request-Method") != "" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// containsOrigin 判断逗号分隔的白名单中是否包含指定 origin。
func containsOrigin(whitelist, origin string) bool {
	for _, o := range strings.Split(whitelist, ",") {
		if strings.TrimSpace(o) == origin {
			return true
		}
	}
	return false
}
