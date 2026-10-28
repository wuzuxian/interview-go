package redisx_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wuzuxian/interview/config"
	redisx "github.com/wuzuxian/interview/core/redis"
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
redis:
  addr: 10.0.0.9:6380
  password: secret
  db: 3
  pool_size: 50
  read_timeout: 2s
`)
	c, err := redisx.LoadConfig(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "10.0.0.9:6380" || c.Password != "secret" || c.DB != 3 ||
		c.PoolSize != 50 || c.ReadTimeout != 2*time.Second {
		t.Fatalf("配置绑定失败: %+v", c)
	}
	// 未配置字段保持默认值。
	if c.DialTimeout != 5*time.Second || c.MinIdleConns != 5 {
		t.Fatalf("默认值缺失: %+v", c)
	}
}

func TestConfigKeyAndMissing(t *testing.T) {
	cfg := newTestConfig(t, "cache:\n  addr: 1.2.3.4:6379\n")
	c, err := redisx.LoadConfig(cfg, "cache")
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "1.2.3.4:6379" || c.PoolSize != 20 {
		t.Fatalf("自定义键绑定失败: %+v", c)
	}

	c2, err := redisx.LoadConfig(cfg, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if c2.Addr != "127.0.0.1:6379" {
		t.Fatalf("缺失子树应返回默认值: %+v", c2)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("APP_REDIS_ADDR", "5.6.7.8:6399")
	t.Setenv("APP_REDIS_DB", "7")
	cfg := newTestConfig(t, "redis:\n  db: 0\n")
	c, err := redisx.LoadConfig(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "5.6.7.8:6399" || c.DB != 7 {
		t.Fatalf("环境变量覆盖失败: %+v", c)
	}
}

// TestNewRealConnection 使用本机默认 Redis（127.0.0.1:6379）；
// 也可通过 TEST_REDIS_ADDR 指定。环境不可用时跳过。
func TestNewRealConnection(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		c := redisx.Config{}
		addr = c.Addr
	}

	client, err := redisx.New(redisx.WithAddr(addr), redisx.WithDialTimeout(2*time.Second))
	if err != nil {
		t.Skipf("Redis %s 不可用，跳过真实连接测试: %v", addr, err)
	}
	defer client.Close()

	ctx := context.Background()
	if err := client.Set(ctx, "redisx:test:key", "hello", time.Minute).Err(); err != nil {
		t.Fatalf("Set 失败: %v", err)
	}
	got, err := client.Get(ctx, "redisx:test:key").Result()
	if err != nil || got != "hello" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if n, err := client.Del(ctx, "redisx:test:key").Result(); err != nil || n != 1 {
		t.Fatalf("Del = %d, %v", n, err)
	}
}

func TestNewWithConfigOptions(t *testing.T) {
	cfg := newTestConfig(t, "redis:\n  addr: 127.0.0.1:6379\n")
	client, err := redisx.NewWithConfig(cfg, "",
		redisx.WithDialTimeout(2*time.Second), redisx.WithPoolSize(3))
	if err != nil {
		t.Skipf("Redis 不可用，跳过: %v", err)
	}
	defer client.Close()

	if client.Config().PoolSize != 3 {
		t.Fatalf("显式选项应覆盖配置: %d", client.Config().PoolSize)
	}
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping 失败: %v", err)
	}
}
