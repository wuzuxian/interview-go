package redisx

import (
	"log/slog"
	"time"

	"github.com/wuzuxian/interview/config"
)

// overrideField 标识显式设置过的配置字段，保证显式选项优先于 WithConfig 绑定值。
type overrideField int

const (
	ovAddr overrideField = iota
	ovPassword
	ovDB
	ovPoolSize
	ovMinIdleConns
	ovDialTimeout
	ovReadTimeout
	ovWriteTimeout
	ovCount
)

// options 收集 New 传入的选项。
type options struct {
	config      *config.Config
	key         string
	cfgExplicit bool
	override    Config
	set         [ovCount]bool
	logger      *slog.Logger
	skipPing    bool
}

// Option 是 Redis 客户端构造选项。
type Option func(*options)

// WithConfig 从配置管理器绑定 Redis 配置，键由 WithConfigKey 指定（默认 "redis"）。
func WithConfig(c *config.Config) Option {
	return func(o *options) {
		if c != nil {
			o.config = c
		}
	}
}

// WithConfigKey 指定配置子树键，默认 "redis"。
func WithConfigKey(key string) Option {
	return func(o *options) { o.key = key }
}

// WithServerConfig 直接给定完整 Redis 配置，跳过配置管理器绑定。
func WithServerConfig(conf Config) Option {
	return func(o *options) {
		o.override = conf
		for i := overrideField(0); i < ovCount; i++ {
			o.set[i] = true
		}
		o.cfgExplicit = true
	}
}

// WithAddr 覆盖连接地址，如 "127.0.0.1:6379"。
func WithAddr(addr string) Option { return overrideString(ovAddr, addr) }

// WithPassword 覆盖密码。
func WithPassword(password string) Option { return overrideString(ovPassword, password) }

// WithDB 覆盖逻辑库编号。
func WithDB(db int) Option {
	return func(o *options) {
		o.override.DB = db
		o.set[ovDB] = true
	}
}

// WithPoolSize 覆盖连接池大小。
func WithPoolSize(n int) Option {
	return func(o *options) {
		o.override.PoolSize = n
		o.set[ovPoolSize] = true
	}
}

// WithMinIdleConns 覆盖最小空闲连接数。
func WithMinIdleConns(n int) Option {
	return func(o *options) {
		o.override.MinIdleConns = n
		o.set[ovMinIdleConns] = true
	}
}

// WithDialTimeout 覆盖建连超时。
func WithDialTimeout(d time.Duration) Option { return overrideDuration(ovDialTimeout, d) }

// WithReadTimeout 覆盖读超时。
func WithReadTimeout(d time.Duration) Option { return overrideDuration(ovReadTimeout, d) }

// WithWriteTimeout 覆盖写超时。
func WithWriteTimeout(d time.Duration) Option { return overrideDuration(ovWriteTimeout, d) }

// WithLogger 注入 slog 日志器（如 *logx.Logger），用于内部调试输出。
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// WithSkipPing 跳过创建时的连通性验证（离线构造场景，如单测）。
func WithSkipPing() Option {
	return func(o *options) { o.skipPing = true }
}

func overrideString(idx overrideField, v string) Option {
	return func(o *options) {
		switch idx {
		case ovAddr:
			o.override.Addr = v
		case ovPassword:
			o.override.Password = v
		}
		o.set[idx] = true
	}
}

func overrideDuration(idx overrideField, d time.Duration) Option {
	return func(o *options) {
		switch idx {
		case ovDialTimeout:
			o.override.DialTimeout = d
		case ovReadTimeout:
			o.override.ReadTimeout = d
		case ovWriteTimeout:
			o.override.WriteTimeout = d
		}
		o.set[idx] = true
	}
}

// applyOverrides 将显式选项覆盖值合并进配置（仅覆盖 set 标记过的字段）。
func applyOverrides(dst *Config, ov Config, set [ovCount]bool) {
	if set[ovAddr] {
		dst.Addr = ov.Addr
	}
	if set[ovPassword] {
		dst.Password = ov.Password
	}
	if set[ovDB] {
		dst.DB = ov.DB
	}
	if set[ovPoolSize] {
		dst.PoolSize = ov.PoolSize
	}
	if set[ovMinIdleConns] {
		dst.MinIdleConns = ov.MinIdleConns
	}
	if set[ovDialTimeout] {
		dst.DialTimeout = ov.DialTimeout
	}
	if set[ovReadTimeout] {
		dst.ReadTimeout = ov.ReadTimeout
	}
	if set[ovWriteTimeout] {
		dst.WriteTimeout = ov.WriteTimeout
	}
}
