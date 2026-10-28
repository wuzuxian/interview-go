package logx_test

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuzuxian/interview/config"
	logx "github.com/wuzuxian/interview/core/log"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入临时文件失败: %v", err)
	}
	return path
}

func newTestConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	c, err := config.Load(config.Options{Files: []string{writeTemp(t, "app.yaml", yaml)}})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	return c
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	return string(b)
}

func TestNewDefaults(t *testing.T) {
	l := logx.New()
	c := l.Config()
	if c.Level != "info" || c.Format != "json" || c.Output != "stdout" {
		t.Fatalf("默认配置错误: %+v", c)
	}
	if l.Level() != slog.LevelInfo {
		t.Fatalf("默认级别应为 info, got %v", l.Level())
	}
}

func TestLevelFilter(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "app.log")
	l := logx.New(logx.WithOutput(logFile), logx.WithFormat("json"))

	l.Debug("debug message", "k", "v") // info 级别下应被过滤
	l.Info("info message", "user", "alice")
	l.Warn("warn message")
	l.Error("error message")

	content := readLogFile(t, logFile)
	if strings.Contains(content, "debug message") {
		t.Fatal("debug 日志不应输出")
	}
	for _, want := range []string{"info message", "warn message", "error message"} {
		if !strings.Contains(content, want) {
			t.Fatalf("日志缺少 %q:\n%s", want, content)
		}
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.Split(content, "\n")[0]), &entry); err != nil {
		t.Fatalf("应为合法 JSON: %v\n%s", err, content)
	}
	if entry["user"] != "alice" {
		t.Fatalf("结构化字段丢失: %v", entry)
	}
}

func TestTextFormat(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "app.log")
	l := logx.New(logx.WithOutput(logFile), logx.WithFormat("text"), logx.WithLevel("debug"))
	l.Debug("hello", "n", 1)

	content := readLogFile(t, logFile)
	if !strings.Contains(content, "hello") || !strings.Contains(content, "n=1") {
		t.Fatalf("text 格式输出错误:\n%s", content)
	}
}

func TestConfigBindingAndOverride(t *testing.T) {
	cfg := newTestConfig(t, "log:\n  level: warn\n  format: text\n  output: stdout\n")

	l1 := logx.New(logx.WithConfig(cfg))
	if c := l1.Config(); c.Level != "warn" || c.Format != "text" {
		t.Fatalf("配置绑定失败: %+v", c)
	}

	// 显式选项优先于配置值，与顺序无关。
	l2 := logx.New(logx.WithConfig(cfg), logx.WithLevel("debug"), logx.WithFormat("json"))
	if c := l2.Config(); c.Level != "debug" || c.Format != "json" {
		t.Fatalf("显式选项应覆盖配置: %+v", c)
	}
	l3 := logx.New(logx.WithLevel("debug"), logx.WithConfig(cfg))
	if l3.Config().Level != "debug" {
		t.Fatalf("选项顺序不应影响结果: %+v", l3.Config())
	}
}

func TestMultiOutputAndClose(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.log")
	f2 := filepath.Join(dir, "sub", "b.log") // 目录不存在，应自动创建

	l := logx.New(logx.WithOutput(strings.Join([]string{f1, f2}, ",")), logx.WithFormat("json"))
	l.Info("multi output")
	if err := l.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	for _, f := range []string{f1, f2} {
		if !strings.Contains(readLogFile(t, f), "multi output") {
			t.Fatalf("输出 %s 缺少日志", f)
		}
	}
}

func TestConfigKey(t *testing.T) {
	cfg := newTestConfig(t, "logging:\n  level: error\n")
	l := logx.New(logx.WithConfig(cfg), logx.WithConfigKey("logging"))
	if l.Config().Level != "error" {
		t.Fatalf("自定义配置键失败: %+v", l.Config())
	}

	// 子树不存在时保持默认值。
	l2 := logx.New(logx.WithConfig(cfg), logx.WithConfigKey("nope"))
	if l2.Config().Level != "info" {
		t.Fatalf("缺失子树应保持默认: %+v", l2.Config())
	}
}

func TestSetAsDefault(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	logFile := filepath.Join(t.TempDir(), "app.log")
	l := logx.New(logx.WithOutput(logFile), logx.WithLevel("debug"))
	logx.SetAsDefault(l)

	slog.Warn("via slog default") // slog 包级函数应路由到封装的输出
	if !strings.Contains(readLogFile(t, logFile), "via slog default") {
		t.Fatal("slog 默认 logger 未生效")
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, "INFO": slog.LevelInfo,
		"warn": slog.LevelWarn, "warning": slog.LevelWarn,
		"error": slog.LevelError, "unknown": slog.LevelInfo,
	}
	for in, want := range cases {
		if got := logx.ParseLevel(in); got != want {
			t.Fatalf("ParseLevel(%q) = %v, 期望 %v", in, got, want)
		}
	}
}
