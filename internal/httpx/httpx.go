// Package httpx 提供与 Java 版 GlobalExceptionHandler 对齐的 HTTP 中间件：
// panic 恢复统一转 Result、404/405 业务化、以及基于 Redis 的多维度限流。
package httpx

import (
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/result"
)

// Recovery 捕获 handler panic：首个 panic 转为 HTTP 200 + Result(500)，
// 与 Java 版 @RestControllerAdvice 兜底行为一致。
func Recovery(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("系统异常(panic)", slog.Any("panic", r), slog.String("path", c.Request.URL.Path))
				if !c.Writer.Written() {
					result.Fail(c, apperr.CodeInternalError, "系统繁忙，请稍后重试")
				}
				c.Abort()
			}
		}()
		c.Next()
		// handler 正常返回但未写响应的兜底。
		if !c.Writer.Written() {
			result.Fail(c, apperr.CodeInternalError, "系统繁忙，请稍后重试")
		}
	}
}

// NoRouteHandler 将未匹配路由统一转为 HTTP 200 + Result(404)。
func NoRouteHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		result.Fail(c, apperr.CodeNotFound, "API 接口不存在")
	}
}

// NoMethodHandler 将方法不匹配统一转为 HTTP 200 + Result(405)。
func NoMethodHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		result.Fail(c, apperr.CodeMethodNotAllowed, "请求方法不支持: "+c.Request.Method)
	}
}

// MaxMultipartMemory 控制 multipart 解析内存阈值，超出部分落盘。
const MaxMultipartMemory = 32 << 20 // 32MB，与 servlet 容器 50MB 上限配合

// ClientIP 提取客户端 IP：优先 X-Forwarded-For 首段，其次 X-Real-IP，最后 RemoteAddr。
func ClientIP(c *gin.Context) string {
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		if idx := strings.IndexByte(xff, ','); idx > 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	if xr := c.GetHeader("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host := c.Request.RemoteAddr
	if idx := strings.LastIndexByte(host, ':'); idx > 0 {
		host = host[:idx]
	}
	return host
}
