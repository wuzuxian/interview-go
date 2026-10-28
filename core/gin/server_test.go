package ginserver_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/config"
	ginserver "github.com/wuzuxian/interview/core/gin"
)

// writeTemp 将内容写入临时文件并返回路径。
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入临时文件失败: %v", err)
	}
	return path
}

// newTestConfig 构造一个带 server 子树的配置管理器。
func newTestConfig(t *testing.T, yamlContent string) *config.Config {
	t.Helper()
	c, err := config.Load(config.Options{
		Files:        []string{writeTemp(t, "app.yaml", yamlContent)},
		EnvPrefix:    "APP",
		AutomaticEnv: true,
	})
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	return c
}

func pingHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"message": "pong"})
}

// startTestServer 在随机端口启动服务器并等待就绪，返回停止函数与实际地址。
func startTestServer(t *testing.T, s *ginserver.Server) (stop func(), addr string) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if addr := s.Addr(); strings.Contains(addr, ":") && !strings.HasSuffix(addr, ":0") {
			stop = func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := s.Shutdown(ctx); err != nil {
					t.Errorf("关闭失败: %v", err)
				}
				if err := <-errCh; err != nil && err != http.ErrServerClosed {
					t.Errorf("Start 返回异常: %v", err)
				}
			}
			return stop, addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("服务器 3 秒内未就绪")
	return nil, ""
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestNewDefaults(t *testing.T) {
	s := ginserver.New()
	sc := s.Config()
	if sc.Host != "0.0.0.0" || sc.Port != 8080 {
		t.Fatalf("默认 host/port = %s:%d", sc.Host, sc.Port)
	}
	if sc.ReadTimeout != 30*time.Second || sc.ShutdownTimeout != 10*time.Second {
		t.Fatalf("默认超时不正确: %+v", sc)
	}
	// 默认配置 mode=debug，New 会同步 gin 全局模式。
	if sc.Mode != gin.DebugMode || gin.Mode() != gin.DebugMode {
		t.Fatalf("默认 mode 应为 debug, got config=%s gin=%s", sc.Mode, gin.Mode())
	}
}

func TestConfigBindingAndOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := newTestConfig(t, `
server:
  host: 127.0.0.1
  port: 9001
  mode: release
  base_path: /api
  read_timeout: 5s
  shutdown_timeout: 3s
  trusted_proxies: [10.0.0.0/8]
`)

	s := ginserver.New(ginserver.WithConfig(cfg))
	sc := s.Config()
	if sc.Host != "127.0.0.1" || sc.Port != 9001 || sc.Mode != "release" {
		t.Fatalf("配置绑定失败: %+v", sc)
	}
	if sc.BasePath != "/api" || sc.ReadTimeout != 5*time.Second || sc.ShutdownTimeout != 3*time.Second {
		t.Fatalf("配置绑定失败: %+v", sc)
	}
	if len(sc.TrustedProxies) != 1 || sc.TrustedProxies[0] != "10.0.0.0/8" {
		t.Fatalf("trusted_proxies 绑定失败: %v", sc.TrustedProxies)
	}

	// 显式选项优先于配置值，且与书写顺序无关。
	s2 := ginserver.New(ginserver.WithConfig(cfg), ginserver.WithPort(7000), ginserver.WithMode(gin.TestMode))
	if s2.Config().Port != 7000 || s2.Config().Mode != gin.TestMode {
		t.Fatalf("显式选项应覆盖配置值: %+v", s2.Config())
	}
	s3 := ginserver.New(ginserver.WithPort(7000), ginserver.WithConfig(cfg))
	if s3.Config().Port != 7000 {
		t.Fatalf("选项顺序不应影响结果: %d", s3.Config().Port)
	}

	// 环境变量覆盖配置文件。
	t.Setenv("APP_SERVER_PORT", "9100")
	s4 := ginserver.New(ginserver.WithConfig(cfg))
	if s4.Config().Port != 9100 {
		t.Fatalf("环境变量应覆盖配置: %d", s4.Config().Port)
	}
}

func TestConfigBindingError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// port 写成非法字符串，绑定错误应在 Start 时返回而非 New 时 panic。
	cfg := newTestConfig(t, "server:\n  port: abc\n")
	s := ginserver.New(ginserver.WithConfig(cfg))
	if err := s.Start(); err == nil {
		t.Fatal("配置绑定错误应在 Start 时返回")
	}
}

func TestRoutesGroupsAndMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var order []string
	mk := func(name string) gin.HandlerFunc {
		return func(c *gin.Context) {
			order = append(order, name)
			c.Next()
		}
	}

	s := ginserver.New(
		ginserver.WithConfig(newTestConfig(t, "server:\n  port: 0\n")),
		ginserver.WithMiddleware(mk("global")),
	)
	s.GET("/ping", pingHandler)
	s.Handle("post", "/echo", func(c *gin.Context) {
		order = append(order, "echo")
		c.String(http.StatusOK, c.Request.URL.Path)
	})
	s.Health("/healthz")
	s.NoRoute(func(c *gin.Context) {
		c.String(http.StatusNotFound, "custom-404")
	})

	api := s.Group("/v1", mk("group"))
	{
		api.GET("/users", mk("users-mw"), func(c *gin.Context) {
			order = append(order, "users")
			c.String(http.StatusOK, "users")
		})
	}

	stop, addr := startTestServer(t, s)
	defer stop()

	code, body := httpGet(t, "http://"+addr+"/ping")
	if code != 200 || !strings.Contains(body, "pong") {
		t.Fatalf("GET /ping => %d %q", code, body)
	}

	resp, err := http.Post("http://"+addr+"/echo", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST /echo => %d", resp.StatusCode)
	}

	code, _ = httpGet(t, "http://"+addr+"/healthz")
	if code != 200 {
		t.Fatalf("health => %d", code)
	}

	code, body = httpGet(t, "http://"+addr+"/not-exist")
	if code != 404 || body != "custom-404" {
		t.Fatalf("NoRoute => %d %q", code, body)
	}

	order = nil
	code, body = httpGet(t, "http://"+addr+"/v1/users")
	if code != 200 || body != "users" {
		t.Fatalf("GET /v1/users => %d %q", code, body)
	}
	if strings.Join(order, ",") != "global,group,users-mw,users" {
		t.Fatalf("中间件执行顺序错误: %v", order)
	}
}

func TestBasePath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := ginserver.New(ginserver.WithConfig(
		newTestConfig(t, "server:\n  port: 0\n  base_path: /api\n")))
	s.GET("/ping", pingHandler)

	stop, addr := startTestServer(t, s)
	defer stop()

	// BasePath 前缀下可访问。
	if code, _ := httpGet(t, "http://"+addr+"/api/ping"); code != 200 {
		t.Fatalf("/api/ping 应可访问, got %d", code)
	}
	// 不带前缀不可访问。
	if code, _ := httpGet(t, "http://"+addr+"/ping"); code != 404 {
		t.Fatalf("不带前缀应 404, got %d", code)
	}

	// 路由组路径在 BasePath 上追加。
	sub := s.Group("/v1")
	sub.GET("/x", pingHandler)
	if code, _ := httpGet(t, "http://"+addr+"/api/v1/x"); code != 200 {
		t.Fatalf("/api/v1/x 应可访问, got %d", code)
	}
}

func TestAllowCORS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := ginserver.New(ginserver.WithConfig(newTestConfig(t, "server:\n  port: 0\n")))
	s.Use(ginserver.AllowCORS("https://a.com"))
	s.OPTIONS("/x", func(c *gin.Context) { c.String(200, "x") })

	stop, addr := startTestServer(t, s)
	defer stop()

	// 白名单命中的预检请求。
	req, _ := http.NewRequest(http.MethodOptions, "http://"+addr+"/x", nil)
	req.Header.Set("Origin", "https://a.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("预检请求应返回 204, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://a.com" {
		t.Fatalf("CORS 头错误: %q", got)
	}

	// 白名单外的预检请求应被拦截。
	req2, _ := http.NewRequest(http.MethodOptions, "http://"+addr+"/x", nil)
	req2.Header.Set("Origin", "https://evil.com")
	req2.Header.Set("Access-Control-Request-Method", "POST")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("拦截的预检也应返回 204, got %d", resp2.StatusCode)
	}
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("白名单外不应有 Allow-Origin 头, got %q", got)
	}
}

func TestStartShutdownLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var shutdownCalled bool
	s := ginserver.New(
		ginserver.WithServerConfig(ginserver.ServerConfig{
			Host:            "127.0.0.1",
			Port:            0,
			Mode:            gin.TestMode,
			ShutdownTimeout: 2 * time.Second,
		}),
		ginserver.WithOnShutdown(func(ctx context.Context) { shutdownCalled = true }),
	)
	s.GET("/ping", pingHandler)

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()

	deadline := time.Now().Add(3 * time.Second)
	var addr string
	for time.Now().Before(deadline) {
		addr = s.Addr()
		if !strings.HasSuffix(addr, ":0") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.HasSuffix(s.Addr(), ":0") {
		t.Fatal("服务器未就绪")
	}

	if code, body := httpGet(t, "http://"+addr+"/ping"); code != 200 || !strings.Contains(body, "pong") {
		t.Fatalf("请求失败: %d %q", code, body)
	}

	// 二次 Start 应报错。
	if err := s.Start(); err == nil {
		t.Fatal("重复 Start 应返回错误")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}
	if !shutdownCalled {
		t.Fatal("OnShutdown 回调未执行")
	}
	if err := <-errCh; err != http.ErrServerClosed {
		t.Fatalf("Start 应返回 ErrServerClosed, got %v", err)
	}
}

func TestRunGracefulStopOnSignal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := ginserver.New(ginserver.WithConfig(newTestConfig(t, "server:\n  port: 0\n")))
	s.GET("/ping", pingHandler)

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run() }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && strings.HasSuffix(s.Addr(), ":0") {
		time.Sleep(10 * time.Millisecond)
	}
	if strings.HasSuffix(s.Addr(), ":0") {
		t.Fatal("服务器未就绪")
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("发送信号失败: %v", err)
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run 应正常返回 nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run 未在 5 秒内优雅退出")
	}
}

func TestWithEngine(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.GET("/native", func(c *gin.Context) { c.String(200, "native") })

	s := ginserver.New(ginserver.WithEngine(e), ginserver.WithConfig(newTestConfig(t, "server:\n  port: 0\n")))
	if s.Engine() != e {
		t.Fatal("WithEngine 应使用调用方引擎")
	}

	stop, addr := startTestServer(t, s)
	defer stop()
	if code, body := httpGet(t, "http://"+addr+"/native"); code != 200 || body != "native" {
		t.Fatalf("自带引擎路由失效: %d %q", code, body)
	}
}

func TestLoadServerConfigKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := newTestConfig(t, `
http:
  port: 7001
  shutdown_timeout: 4s
`)
	sc, err := ginserver.LoadServerConfig(cfg, "http")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Port != 7001 || sc.ShutdownTimeout != 4*time.Second {
		t.Fatalf("自定义键绑定失败: %+v", sc)
	}
	// 其余字段保持默认值。
	if sc.Host != "0.0.0.0" || sc.ReadTimeout != 30*time.Second {
		t.Fatalf("未配置字段应保持默认: %+v", sc)
	}

	// 子树不存在时返回默认值。
	sc2, err := ginserver.LoadServerConfig(cfg, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if sc2.Port != 8080 {
		t.Fatalf("缺失子树应返回默认值: %d", sc2.Port)
	}
}

func ExampleServer() {
	gin.SetMode(gin.ReleaseMode)

	cfg := config.MustLoad(config.Options{
		Defaults: map[string]any{"server.port": 18080},
	})
	srv := ginserver.New(
		ginserver.WithConfig(cfg),
		ginserver.WithMiddleware(ginserver.AllowCORS()),
	)
	srv.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})
	api := srv.Group("/api/v1")
	api.GET("/hello", func(c *gin.Context) {
		c.String(http.StatusOK, fmt.Sprintf("hello %s", c.Query("name")))
	})
	fmt.Println("routes ready, addr =", srv.Addr())
	// Output:
	// routes ready, addr = 0.0.0.0:18080
}
