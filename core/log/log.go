// Package logx 基于 log/slog 封装的日志模块：
// 支持从 config 配置管理模块读取级别、格式与输出目标，支持多输出，
// 并可一键设置为 slog 全局默认 logger。
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wuzuxian/interview/config"
)

// defaultConfigKey 是 Config 在配置文件中的默认键。
const defaultConfigKey = "log"

// Config 是日志模块配置。
//
// 对应的配置文件示例（YAML）：
//
//	log:
//	  level: info          # debug / info / warn / error
//	  format: json         # json / text
//	  output: stdout       # stdout、stderr 或文件路径，逗号分隔可多输出
type Config struct {
	Level  string `config:"level" default:"info"`
	Format string `config:"format" default:"json"`
	Output string `config:"output" default:"stdout"`
}

// LoadConfig 从配置管理器的 key 子树（默认 "log"）绑定日志配置。
func LoadConfig(cfg *config.Config, key string) (Config, error) {
	if key == "" {
		key = defaultConfigKey
	}
	c := Config{}
	if err := cfg.UnmarshalKey(key, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// ParseLevel 将字符串解析为 slog 级别，不区分大小写；无法识别时返回 info。
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Logger 是封装后的日志器，内嵌 *slog.Logger，可直接调用 Debug/Info/Warn/Error/With/WithGroup。
type Logger struct {
	*slog.Logger
	cfg     Config
	mu      sync.Mutex
	closers []io.Closer
}

// New 按选项创建日志器。级别、格式、输出依次为 debug/info/warn/error、
// json/text、stdout/stderr/文件路径（逗号分隔多输出）。
func New(opts ...Option) *Logger {
	o := &options{key: defaultConfigKey}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}

	l := &Logger{cfg: defaultConfig()}

	// 依次应用：默认值 -> 配置管理器绑定 -> 显式选项覆盖（与书写顺序无关）。
	if o.cfg != nil {
		if c, err := LoadConfig(o.cfg, o.key); err != nil {
			l.printBootstrapError("加载日志配置 %q 失败，使用默认值: %v", o.key, err)
		} else {
			l.cfg = c
		}
	}
	applyOverrides(&l.cfg, o.cfgOverride, o.set)

	w, closers, err := buildWriters(l.cfg.Output)
	if err != nil {
		l.printBootstrapError("%v", err)
		w = os.Stdout
	}
	l.closers = closers
	l.Logger = slog.New(newHandler(w, l.cfg))
	return l
}

// NewFromConfig 从配置管理器创建日志器，key 为空时使用 "log"。
func NewFromConfig(cfg *config.Config, key string) *Logger {
	return New(WithConfig(cfg), WithConfigKey(key))
}

// SetAsDefault 将 l 设置为 slog 包级默认 logger，
// 此后 slog.Info / slog.Error 等包级函数与 *log.Logger（标准库 log）都路由到它。
func SetAsDefault(l *Logger) {
	slog.SetDefault(l.Logger)
	slog.SetLogLoggerLevel(ParseLevel(l.cfg.Level))
}

// Config 返回当前生效的日志配置。
func (l *Logger) Config() Config {
	return l.cfg
}

// Level 返回当前日志级别。
func (l *Logger) Level() slog.Level {
	return ParseLevel(l.cfg.Level)
}

// Close 关闭日志器打开的文件输出，进程退出前调用。
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var err error
	for _, c := range l.closers {
		if e := c.Close(); e != nil {
			err = e
		}
	}
	l.closers = nil
	return err
}

// printBootstrapError 在 logger 尚未就绪时兜底输出引导错误。
func (l *Logger) printBootstrapError(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "logx: "+format+"\n", args...)
}

// defaultConfig 返回全默认值的日志配置。
func defaultConfig() Config {
	c := Config{}
	_ = config.New().Unmarshal(&c) // 经由 default 标签填充默认值，不会失败
	return c
}

// newHandler 按格式创建 slog handler。
func newHandler(w io.Writer, cfg Config) slog.Handler {
	opts := &slog.HandlerOptions{Level: ParseLevel(cfg.Level)}
	if strings.EqualFold(strings.TrimSpace(cfg.Format), "text") {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

// buildWriters 解析 output 配置为 writer 列表，返回需要关闭的文件。
func buildWriters(output string) (io.Writer, []io.Closer, error) {
	parts := strings.Split(output, ",")
	writers := make([]io.Writer, 0, len(parts))
	var closers []io.Closer

	for _, p := range parts {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "", "stdout":
			writers = append(writers, os.Stdout)
		case "stderr":
			writers = append(writers, os.Stderr)
		default:
			path := strings.TrimSpace(p)
			if dir := filepath.Dir(path); dir != "." && dir != "" {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return nil, nil, fmt.Errorf("创建日志目录 %s 失败: %w", dir, err)
				}
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return nil, nil, fmt.Errorf("打开日志文件 %s 失败: %w", path, err)
			}
			writers = append(writers, f)
			closers = append(closers, f)
		}
	}

	if len(writers) == 1 {
		return writers[0], closers, nil
	}
	return io.MultiWriter(writers...), closers, nil
}
