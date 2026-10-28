// Package ratelimit 实现 Java 版 @RateLimit 注解等价能力：
// 基于 Redis 的令牌桶（固定窗口计数），支持 GLOBAL / IP / USER 三个独立维度，
// 多条规则全部通过才放行；超限返回 Result(code=8001)。
package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/httpx"
	"github.com/wuzuxian/interview/internal/result"
)

// Dimension 限流维度。
type Dimension int

const (
	// DimensionGlobal 全局限流：所有请求共享一个计数器。
	DimensionGlobal Dimension = iota
	// DimensionIP 按客户端 IP 限流。
	DimensionIP
	// DimensionUser 按用户限流（当前平台无登录体系，退化为 IP）。
	DimensionUser
)

// Rule 单条限流规则：窗口 window 内最多 count 次。
type Rule struct {
	Dimension Dimension
	Count     int64
	Window    time.Duration
}

// Limiter Redis 限流器。
type Limiter struct {
	rdb    redis.Scripter
	logger *slog.Logger
	prefix string
}

// New 创建限流器，prefix 为 key 前缀（用于多实例隔离）。
func New(rdb redis.Scripter, logger *slog.Logger) *Limiter {
	return &Limiter{rdb: rdb, logger: logger, prefix: "ratelimit"}
}

// luaFixedWindow 固定窗口计数：超出返回 0，否则自增并设置过期。
const luaFixedWindow = `
local current = redis.call('INCR', KEYS[1])
if current == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return current
`

// Allow 检查单条规则；ok=false 时返回重试等待时间。
func (l *Limiter) Allow(ctx context.Context, rule Rule, key string) (bool, error) {
	now := time.Now()
	windowStart := now.Truncate(rule.Window)
	redisKey := fmt.Sprintf("%s:%s:%s:%d", l.prefix, rule.dimensionName(), key, windowStart.UnixMilli())
	ttl := rule.Window

	res, err := l.rdb.Eval(ctx, luaFixedWindow, []string{redisKey}, ttl.Milliseconds()).Int64()
	if err != nil {
		// Redis 异常时放行（fail-open），避免存储故障放大为全站不可用。
		if l.logger != nil {
			l.logger.Warn("限流器 Redis 访问失败，放行请求", slog.String("error", err.Error()))
		}
		return true, nil
	}
	return res <= rule.Count, nil
}

func (r Rule) dimensionName() string {
	switch r.Dimension {
	case DimensionGlobal:
		return "global"
	case DimensionUser:
		return "user"
	default:
		return "ip"
	}
}

// Middleware 返回 gin 中间件：所有规则全部通过才放行，任一超限立即拒绝。
func (l *Limiter) Middleware(rules ...Rule) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		ip := httpx.ClientIP(c)
		for _, rule := range rules {
			var key string
			switch rule.Dimension {
			case DimensionGlobal:
				key = "all"
			case DimensionIP, DimensionUser:
				key = ip
			}
			ok, err := l.Allow(ctx, rule, key)
			if err != nil {
				if l.logger != nil {
					l.logger.Warn("限流检查异常", slog.String("error", err.Error()))
				}
			}
			if !ok {
				result.Fail(c, apperr.CodeRateLimitExceeded, "请求过于频繁，请稍后再试")
				c.Abort()
				return
			}
		}
		c.Next()
	}
}

// Global 返回全局限流规则，每 window 窗口 count 次。
func Global(count int64, window time.Duration) Rule {
	return Rule{Dimension: DimensionGlobal, Count: count, Window: window}
}

// PerIP 返回 IP 维度限流规则。
func PerIP(count int64, window time.Duration) Rule {
	return Rule{Dimension: DimensionIP, Count: count, Window: window}
}
