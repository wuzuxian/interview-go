package gormdb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	gormlogger "gorm.io/gorm/logger"
)

// slogGormLogger 将 gorm SQL 日志路由到 slog。
// 级别映射：silent=不输出，error=仅错误，warn=错误+慢查询，info=全部 SQL。
type slogGormLogger struct {
	l             *slog.Logger
	level         gormlogger.LogLevel
	slowThreshold time.Duration
}

// NewSlogLogger 构造 gorm 的 slog 适配日志器，也可独立使用。
// l 为 nil 时使用 slog 默认 logger；level 为 silent/error/warn/info。
func NewSlogLogger(l *slog.Logger, level string, slowThreshold time.Duration) gormlogger.Interface {
	if l == nil {
		l = slog.Default()
	}
	if slowThreshold <= 0 {
		slowThreshold = 200 * time.Millisecond
	}
	return &slogGormLogger{
		l:             l,
		level:         parseGormLogLevel(level),
		slowThreshold: slowThreshold,
	}
}

// newSlogLogger 供包内使用。
func newSlogLogger(l *slog.Logger, level string, slowThreshold time.Duration) gormlogger.Interface {
	return NewSlogLogger(l, level, slowThreshold)
}

// parseGormLogLevel 解析 gorm 日志级别字符串，无法识别时为 warn。
func parseGormLogLevel(s string) gormlogger.LogLevel {
	switch s {
	case "silent":
		return gormlogger.Silent
	case "error":
		return gormlogger.Error
	case "info":
		return gormlogger.Info
	default:
		return gormlogger.Warn
	}
}

func (g *slogGormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	n := *g
	n.level = level
	return &n
}

func (g *slogGormLogger) Info(ctx context.Context, format string, args ...any) {
	if g.level < gormlogger.Info {
		return
	}
	g.l.InfoContext(ctx, fmt.Sprintf(format, args...))
}

func (g *slogGormLogger) Warn(ctx context.Context, format string, args ...any) {
	if g.level < gormlogger.Warn {
		return
	}
	g.l.WarnContext(ctx, fmt.Sprintf(format, args...))
}

func (g *slogGormLogger) Error(ctx context.Context, format string, args ...any) {
	if g.level < gormlogger.Error {
		return
	}
	g.l.ErrorContext(ctx, fmt.Sprintf(format, args...))
}

// Trace 记录 SQL 执行：错误以 error 级别、慢查询以 warn 级别、其余按 info 级别输出。
// ErrRecordNotFound 属于正常业务语义，任何级别都不输出。
func (g *slogGormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if g.level <= gormlogger.Silent {
		return
	}
	if err != nil && errors.Is(err, gormlogger.ErrRecordNotFound) {
		return
	}
	elapsed := time.Since(begin)
	sql, rows := fc()
	switch {
	case err != nil && g.level >= gormlogger.Error:
		g.l.ErrorContext(ctx, "gorm trace",
			slog.String("sql", sql),
			slog.Int64("rows", rows),
			slog.Duration("elapsed", elapsed),
			slog.String("error", err.Error()))
	case g.slowThreshold > 0 && elapsed > g.slowThreshold && g.level >= gormlogger.Warn:
		g.l.WarnContext(ctx, "gorm slow query",
			slog.String("sql", sql),
			slog.Int64("rows", rows),
			slog.Duration("elapsed", elapsed),
			slog.Duration("threshold", g.slowThreshold))
	case g.level >= gormlogger.Info:
		g.l.InfoContext(ctx, "gorm query",
			slog.String("sql", sql),
			slog.Int64("rows", rows),
			slog.Duration("elapsed", elapsed))
	}
}
