package gormdb

import (
	"log/slog"
	"time"

	gormlogger "gorm.io/gorm/logger"

	"github.com/wuzuxian/interview/config"
)

// overrideField 标识显式设置过的配置字段，保证显式选项优先于 WithConfig 绑定值。
type overrideField int

const (
	ovHost overrideField = iota
	ovPort
	ovUser
	ovPassword
	ovDBName
	ovSSLMode
	ovMaxOpenConns
	ovMaxIdleConns
	ovLogLevel
	ovSlowThreshold
	ovCount
)

// options 收集 New 传入的选项。
type options struct {
	config     *config.Config
	key        string
	dsn        string
	override   Config
	set        [ovCount]bool
	logger     *slog.Logger
	gormLogger gormlogger.Interface
	skipPing   bool
}

// Option 是数据库连接构造选项。
type Option func(*options)

// WithConfig 从配置管理器绑定数据库配置，键由 WithConfigKey 指定（默认 "postgres"）。
func WithConfig(c *config.Config) Option {
	return func(o *options) {
		if c != nil {
			o.config = c
		}
	}
}

// WithConfigKey 指定配置子树键，默认 "postgres"。
func WithConfigKey(key string) Option {
	return func(o *options) { o.key = key }
}

// WithDSN 直接使用连接串，跳过配置绑定与凭据字段（连接池与日志配置仍然生效）。
func WithDSN(dsn string) Option {
	return func(o *options) { o.dsn = dsn }
}

// WithHost 覆盖主机。
func WithHost(host string) Option { return overrideString(ovHost, host) }

// WithPort 覆盖端口。
func WithPort(port int) Option {
	return func(o *options) {
		o.override.Port = port
		o.set[ovPort] = true
	}
}

// WithUser 覆盖用户名。
func WithUser(user string) Option { return overrideString(ovUser, user) }

// WithPassword 覆盖密码。
func WithPassword(password string) Option { return overrideString(ovPassword, password) }

// WithDBName 覆盖库名。
func WithDBName(name string) Option { return overrideString(ovDBName, name) }

// WithSSLMode 覆盖 sslmode（disable / require / verify-full 等）。
func WithSSLMode(mode string) Option { return overrideString(ovSSLMode, mode) }

// WithMaxOpenConns 覆盖最大打开连接数。
func WithMaxOpenConns(n int) Option {
	return func(o *options) {
		o.override.MaxOpenConns = n
		o.set[ovMaxOpenConns] = true
	}
}

// WithMaxIdleConns 覆盖最大空闲连接数。
func WithMaxIdleConns(n int) Option {
	return func(o *options) {
		o.override.MaxIdleConns = n
		o.set[ovMaxIdleConns] = true
	}
}

// WithLogLevel 覆盖 gorm SQL 日志级别：silent / error / warn / info。
func WithLogLevel(level string) Option { return overrideString(ovLogLevel, level) }

// WithSlowThreshold 覆盖慢查询阈值。
func WithSlowThreshold(d time.Duration) Option {
	return func(o *options) {
		o.override.SlowThreshold = d
		o.set[ovSlowThreshold] = true
	}
}

// WithLogger 注入 slog 日志器（如 *logx.Logger 的内嵌 *slog.Logger），
// gorm 的 SQL 日志将路由到它；不注入时使用 slog 默认 logger。
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

// WithGormLogger 直接注入 gorm 原生日志器（覆盖 slog 适配）。
func WithGormLogger(l gormlogger.Interface) Option {
	return func(o *options) { o.gormLogger = l }
}

// WithSkipPing 跳过创建时的连通性验证（离线构造场景，如单测）。
func WithSkipPing() Option {
	return func(o *options) { o.skipPing = true }
}

func overrideString(idx overrideField, v string) Option {
	return func(o *options) {
		switch idx {
		case ovHost:
			o.override.Host = v
		case ovUser:
			o.override.User = v
		case ovPassword:
			o.override.Password = v
		case ovDBName:
			o.override.DBName = v
		case ovSSLMode:
			o.override.SSLMode = v
		case ovLogLevel:
			o.override.LogLevel = v
		}
		o.set[idx] = true
	}
}

// applyOverrides 将显式选项覆盖值合并进配置（仅覆盖 set 标记过的字段）。
func applyOverrides(dst *Config, ov Config, set [ovCount]bool) {
	if set[ovHost] {
		dst.Host = ov.Host
	}
	if set[ovPort] {
		dst.Port = ov.Port
	}
	if set[ovUser] {
		dst.User = ov.User
	}
	if set[ovPassword] {
		dst.Password = ov.Password
	}
	if set[ovDBName] {
		dst.DBName = ov.DBName
	}
	if set[ovSSLMode] {
		dst.SSLMode = ov.SSLMode
	}
	if set[ovMaxOpenConns] {
		dst.MaxOpenConns = ov.MaxOpenConns
	}
	if set[ovMaxIdleConns] {
		dst.MaxIdleConns = ov.MaxIdleConns
	}
	if set[ovLogLevel] {
		dst.LogLevel = ov.LogLevel
	}
	if set[ovSlowThreshold] {
		dst.SlowThreshold = ov.SlowThreshold
	}
}
