package ginserver

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wuzuxian/interview/config"
)

// overrideField 标识显式设置过的配置字段，保证显式选项优先于 WithConfig 绑定值，
// 且与选项的书写顺序无关。
type overrideField int

const (
	ovHost overrideField = iota
	ovPort
	ovMode
	ovBasePath
	ovReadTimeout
	ovWriteTimeout
	ovIdleTimeout
	ovShutdownTimeout
	ovTrustedProxies
	ovCount
)

// options 收集 New 传入的选项，最终统一构建 Server。
type options struct {
	cfg        *config.Config
	configKey  string
	override   ServerConfig // 显式选项写入的覆盖值
	set        [ovCount]bool
	engine     *gin.Engine
	middleware []gin.HandlerFunc
	onShutdown []func(context.Context)
	initErr    error
}

// Option 是 Server 的构造选项。
type Option func(*options)

// WithConfig 从配置管理器绑定服务器配置，键由 WithConfigKey 指定（默认 "server"）。
// 绑定失败不会在 New 时报错，而是保留到 Start/Run 时返回。
func WithConfig(c *config.Config) Option {
	return func(o *options) {
		if c != nil {
			o.cfg = c
		}
	}
}

// WithConfigKey 指定配置子树键，默认 "server"。
func WithConfigKey(key string) Option {
	return func(o *options) { o.configKey = key }
}

// WithServerConfig 直接给定完整服务器配置，跳过配置管理器绑定。
func WithServerConfig(sc ServerConfig) Option {
	return func(o *options) {
		o.override = sc
		for i := overrideField(0); i < ovCount; i++ {
			o.set[i] = true
		}
	}
}

// WithEngine 使用调用方自建的 gin.Engine，此后所有路由注册到该引擎上。
func WithEngine(e *gin.Engine) Option {
	return func(o *options) { o.engine = e }
}

// WithHost 覆盖监听地址，如 "127.0.0.1"。
func WithHost(host string) Option {
	return overrideString(ovHost, host)
}

// WithPort 覆盖监听端口。
func WithPort(port int) Option {
	return func(o *options) {
		o.override.Port = port
		o.set[ovPort] = true
	}
}

// WithMode 覆盖 gin 运行模式：gin.DebugMode / gin.ReleaseMode / gin.TestMode。
func WithMode(mode string) Option {
	return overrideString(ovMode, mode)
}

// WithBasePath 覆盖全局路由前缀，如 "/api"，注册的路由都会挂在该前缀之下。
func WithBasePath(path string) Option {
	return overrideString(ovBasePath, path)
}

// WithReadTimeout 覆盖读超时。
func WithReadTimeout(d time.Duration) Option {
	return overrideDuration(ovReadTimeout, d)
}

// WithWriteTimeout 覆盖写超时。
func WithWriteTimeout(d time.Duration) Option {
	return overrideDuration(ovWriteTimeout, d)
}

// WithIdleTimeout 覆盖空闲连接超时。
func WithIdleTimeout(d time.Duration) Option {
	return overrideDuration(ovIdleTimeout, d)
}

// WithShutdownTimeout 覆盖优雅关闭时等待存量请求的最长时长。
func WithShutdownTimeout(d time.Duration) Option {
	return overrideDuration(ovShutdownTimeout, d)
}

// WithTrustedProxies 覆盖信任代理网段；不传参数表示不信任任何代理。
func WithTrustedProxies(cidrs ...string) Option {
	return func(o *options) {
		o.override.TrustedProxies = append([]string(nil), cidrs...)
		o.set[ovTrustedProxies] = true
	}
}

// WithMiddleware 注册全局中间件，等价于构造后调用 Server.Use。
func WithMiddleware(mw ...gin.HandlerFunc) Option {
	return func(o *options) { o.middleware = append(o.middleware, mw...) }
}

// WithOnShutdown 注册服务器优雅关闭后执行的回调（如关闭数据库连接池），可多次调用。
func WithOnShutdown(fn func(ctx context.Context)) Option {
	return func(o *options) { o.onShutdown = append(o.onShutdown, fn) }
}

// overrideString 返回覆盖字符串字段的选项。
func overrideString(idx overrideField, v string) Option {
	return func(o *options) {
		switch idx {
		case ovHost:
			o.override.Host = v
		case ovMode:
			o.override.Mode = v
		case ovBasePath:
			o.override.BasePath = v
		}
		o.set[idx] = true
	}
}

// overrideDuration 返回覆盖时长字段的选项。
func overrideDuration(idx overrideField, d time.Duration) Option {
	return func(o *options) {
		switch idx {
		case ovReadTimeout:
			o.override.ReadTimeout = d
		case ovWriteTimeout:
			o.override.WriteTimeout = d
		case ovIdleTimeout:
			o.override.IdleTimeout = d
		case ovShutdownTimeout:
			o.override.ShutdownTimeout = d
		}
		o.set[idx] = true
	}
}

// joinHostPort 拼接监听地址，IPv6 场景自动加方括号。
func joinHostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}
