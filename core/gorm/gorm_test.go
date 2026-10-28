package gormdb_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gormlogger "gorm.io/gorm/logger"

	"github.com/wuzuxian/interview/config"
	gormdb "github.com/wuzuxian/interview/core/gorm"
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
	c, err := config.Load(config.Options{
		Files:        []string{writeTemp(t, "app.yaml", yaml)},
		EnvPrefix:    "APP",
		AutomaticEnv: true,
	})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	return c
}

func TestLoadConfigAndDefaults(t *testing.T) {
	cfg := newTestConfig(t, `
postgres:
  host: 10.0.0.5
  port: 6543
  user: app
  password: secret
  dbname: mydb
  sslmode: require
  max_open_conns: 50
  slow_threshold: 500ms
`)
	sc, err := gormdb.LoadConfig(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Host != "10.0.0.5" || sc.Port != 6543 || sc.User != "app" ||
		sc.DBName != "mydb" || sc.SSLMode != "require" || sc.MaxOpenConns != 50 ||
		sc.SlowThreshold != 500*time.Millisecond {
		t.Fatalf("配置绑定失败: %+v", sc)
	}
	// 未配置字段保持默认值。
	if sc.ConnMaxLifetime != time.Hour || sc.LogLevel != "warn" {
		t.Fatalf("默认值缺失: %+v", sc)
	}

	want := "host=10.0.0.5 port=6543 user=app password=secret dbname=mydb sslmode=require TimeZone=Asia/Shanghai"
	if got := sc.DSN(); got != want {
		t.Fatalf("DSN = %q, 期望 %q", got, want)
	}
	if sc.Addr() != "10.0.0.5:6543" {
		t.Fatalf("Addr = %q", sc.Addr())
	}
}

func TestConfigKeyAndMissing(t *testing.T) {
	cfg := newTestConfig(t, "db:\n  port: 7000\n")
	sc, err := gormdb.LoadConfig(cfg, "db")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Port != 7000 || sc.Host != "127.0.0.1" {
		t.Fatalf("自定义键绑定失败: %+v", sc)
	}

	sc2, err := gormdb.LoadConfig(cfg, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if sc2.Port != 5432 {
		t.Fatalf("缺失子树应返回默认值: %+v", sc2)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("APP_POSTGRES_HOST", "db.internal")
	t.Setenv("APP_POSTGRES_PORT", "6000")
	cfg := newTestConfig(t, "postgres:\n  port: 5432\n")
	sc, err := gormdb.LoadConfig(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Host != "db.internal" || sc.Port != 6000 {
		t.Fatalf("环境变量覆盖失败: %+v", sc)
	}
}

func TestSlogGormLogger(t *testing.T) {
	var buf bytes.Buffer
	sl := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	gl := gormdb.NewSlogLogger(sl, "info", 100*time.Millisecond) // info：全部 SQL 输出
	ctx := context.Background()

	// 正常查询 -> info。
	gl.Trace(ctx, time.Now(), func() (string, int64) { return "SELECT 1", 1 }, nil)
	// 慢查询 -> warn。
	gl.Trace(ctx, time.Now().Add(-time.Second), func() (string, int64) { return "SELECT 2", 2 }, nil)
	// 执行错误 -> error（ErrRecordNotFound 除外）。
	gl.Trace(ctx, time.Now(), func() (string, int64) { return "SELECT 3", 0 }, context.DeadlineExceeded)
	// 记录不存在 -> 不输出。
	gl.Trace(ctx, time.Now(), func() (string, int64) { return "SELECT 4", 0 }, gormlogger.ErrRecordNotFound)

	out := buf.String()
	t.Log(out)
	if !strings.Contains(out, `"level":"INFO"`) || !strings.Contains(out, "SELECT 1") {
		t.Fatalf("普通查询未以 info 输出:\n%s", out)
	}
	if !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, "SELECT 2") {
		t.Fatalf("慢查询未以 warn 输出:\n%s", out)
	}
	if !strings.Contains(out, `"level":"ERROR"`) || !strings.Contains(out, "SELECT 3") {
		t.Fatalf("错误查询未以 error 输出:\n%s", out)
	}
	if strings.Contains(out, "SELECT 4") {
		t.Fatalf("ErrRecordNotFound 不应输出:\n%s", out)
	}

	// 输出应为合法 JSON 行。
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("日志行非 JSON: %v\n%s", err, line)
		}
	}
}

func TestSlogGormLoggerLevel(t *testing.T) {
	var buf bytes.Buffer
	sl := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// warn 级别：普通 SQL 不输出。
	gl := gormdb.NewSlogLogger(sl, "warn", time.Hour)
	gl.Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT x", 1 }, nil)
	if strings.Contains(buf.String(), "SELECT x") {
		t.Fatalf("warn 级别下普通 SQL 不应输出:\n%s", buf.String())
	}

	// silent 级别：错误也不输出。
	gl = gormdb.NewSlogLogger(sl, "silent", time.Hour)
	gl.Error(context.Background(), "boom")
	gl.Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT y", 1 }, context.DeadlineExceeded)
	if strings.Contains(buf.String(), "SELECT y") || strings.Contains(buf.String(), "boom") {
		t.Fatalf("silent 级别下不应有任何输出:\n%s", buf.String())
	}
}

func TestNewRealConnection(t *testing.T) {
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_PG_DSN，跳过真实连接测试")
	}
	db, err := gormdb.New(gormdb.WithDSN(dsn))
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer db.Close()

	if err := db.Ping(context.Background()); err != nil {
		t.Fatalf("Ping 失败: %v", err)
	}

	var one int
	if err := db.WithContext(context.Background()).Raw("SELECT 1").Scan(&one).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 = %d", one)
	}
}
