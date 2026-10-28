package logx

import (
	"github.com/wuzuxian/interview/config"
)

// overrideField 标识显式设置过的配置字段，保证显式选项优先于 WithConfig 绑定值。
type overrideField int

const (
	ovLevel overrideField = iota
	ovFormat
	ovOutput
	ovCount
)

// options 收集 New 传入的选项。
type options struct {
	cfg         *config.Config
	key         string
	cfgOverride Config
	set         [ovCount]bool
}

// Option 是日志器构造选项。
type Option func(*options)

// WithConfig 从配置管理器绑定日志配置，键由 WithConfigKey 指定（默认 "log"）。
// 绑定失败时保留默认值并在 stderr 打印一条引导提示，不会 panic。
func WithConfig(c *config.Config) Option {
	return func(o *options) {
		if c != nil {
			o.cfg = c
		}
	}
}

// WithConfigKey 指定配置子树键，默认 "log"。
func WithConfigKey(key string) Option {
	return func(o *options) { o.key = key }
}

// WithLevel 覆盖日志级别：debug / info / warn / error。
func WithLevel(level string) Option {
	return func(o *options) {
		o.cfgOverride.Level = level
		o.set[ovLevel] = true
	}
}

// WithFormat 覆盖日志格式：json / text。
func WithFormat(format string) Option {
	return func(o *options) {
		o.cfgOverride.Format = format
		o.set[ovFormat] = true
	}
}

// WithOutput 覆盖输出目标：stdout / stderr / 文件路径，逗号分隔可多输出。
func WithOutput(output string) Option {
	return func(o *options) {
		o.cfgOverride.Output = output
		o.set[ovOutput] = true
	}
}

// applyOverrides 将显式选项覆盖值合并进配置（仅覆盖 set 标记过的字段）。
func applyOverrides(dst *Config, ov Config, set [ovCount]bool) {
	if set[ovLevel] {
		dst.Level = ov.Level
	}
	if set[ovFormat] {
		dst.Format = ov.Format
	}
	if set[ovOutput] {
		dst.Output = ov.Output
	}
}
