// Package ginserver 基于 gin 封装的 HTTP 服务器模块：
// 与本仓库的 config 配置管理模块联动，提供实例化（函数式选项）、
// 路由与路由组注册、中间件注册、优雅启停等常见能力。
package ginserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

// Server 是封装后的 HTTP 服务器。
// 通过 New 创建，注册路由与中间件后调用 Run（自带信号优雅退出）或 Start（纯阻塞）启动。
type Server struct {
	conf   ServerConfig
	engine *gin.Engine
	root   *gin.RouterGroup // 挂载全局 BasePath 后的路由挂载点；BasePath 为空时为 nil

	onShutdown []func(ctx context.Context)
	initErr    error

	mu      sync.Mutex
	httpSrv *http.Server
	started bool
	// actualAddr 为监听就绪后的实际地址（Port 配置为 0 时由内核分配）。
	actualAddr string
}

// New 创建服务器实例。选项按需传入；WithConfig 的绑定值会先应用，
// WithHost/WithPort 等显式选项始终覆盖配置值（与书写顺序无关）。
func New(opts ...Option) *Server {
	o := &options{configKey: defaultConfigKey}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}

	s := &Server{
		conf:       defaultServerConfig(),
		onShutdown: o.onShutdown,
	}

	// 1) 配置管理器绑定。
	if o.cfg != nil {
		sc, err := LoadServerConfig(o.cfg, o.configKey)
		if err != nil {
			s.initErr = fmt.Errorf("ginserver: 绑定服务器配置 %q 失败: %w", o.configKey, err)
		}
		s.conf = sc
	}

	// 2) 显式选项覆盖。
	applyOverrides(&s.conf, o.override, o.set)

	// 3) 运行模式需在创建引擎前设置。
	gin.SetMode(s.conf.Mode)

	// 4) 引擎与信任代理。
	if o.engine != nil {
		s.engine = o.engine
	} else {
		s.engine = gin.New()
	}
	if s.conf.TrustedProxies != nil {
		if err := s.engine.SetTrustedProxies(s.conf.TrustedProxies); err != nil {
			s.initErr = errors.Join(s.initErr, fmt.Errorf("ginserver: 设置可信代理失败: %w", err))
		}
	}

	// 5) 全局路由前缀挂载点。
	if s.conf.BasePath != "" {
		s.root = s.engine.Group(s.conf.BasePath)
	}

	// 6) 选项携带的中间件。
	if len(o.middleware) > 0 {
		s.engine.Use(o.middleware...)
	}
	return s
}

// applyOverrides 将显式选项覆盖值合并进配置（仅覆盖 set 标记过的字段）。
func applyOverrides(dst *ServerConfig, ov ServerConfig, set [ovCount]bool) {
	if set[ovHost] {
		dst.Host = ov.Host
	}
	if set[ovPort] {
		dst.Port = ov.Port
	}
	if set[ovMode] {
		dst.Mode = ov.Mode
	}
	if set[ovBasePath] {
		dst.BasePath = ov.BasePath
	}
	if set[ovReadTimeout] {
		dst.ReadTimeout = ov.ReadTimeout
	}
	if set[ovWriteTimeout] {
		dst.WriteTimeout = ov.WriteTimeout
	}
	if set[ovIdleTimeout] {
		dst.IdleTimeout = ov.IdleTimeout
	}
	if set[ovShutdownTimeout] {
		dst.ShutdownTimeout = ov.ShutdownTimeout
	}
	if set[ovTrustedProxies] {
		dst.TrustedProxies = ov.TrustedProxies
	}
}

// Start 阻塞启动 HTTP 服务。进程退出需调用方自行管理；
// 生产环境通常直接使用 Run。
func (s *Server) Start() error {
	if s.initErr != nil {
		return s.initErr
	}

	srv := &http.Server{
		Handler:      s.engine,
		ReadTimeout:  s.conf.ReadTimeout,
		WriteTimeout: s.conf.WriteTimeout,
		IdleTimeout:  s.conf.IdleTimeout,
	}

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("ginserver: 服务器已启动")
	}
	s.started = true
	s.mu.Unlock()

	ln, err := net.Listen("tcp", s.conf.Addr())
	if err != nil {
		return fmt.Errorf("ginserver: 监听 %s 失败: %w", s.conf.Addr(), err)
	}

	s.mu.Lock()
	s.httpSrv = srv
	s.actualAddr = ln.Addr().String()
	s.mu.Unlock()

	log.Printf("ginserver: HTTP 服务已启动，监听 %s（mode=%s）", s.actualAddr, s.conf.Mode)
	err = srv.Serve(ln)
	// Shutdown 引发的 ErrServerClosed 属于正常退出路径，原样返回交由调用方判断。
	if errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return fmt.Errorf("ginserver: 服务异常退出: %w", err)
}

// Run 阻塞启动 HTTP 服务，并自动监听 SIGINT / SIGTERM 信号执行优雅关闭：
// 停止接收新请求 -> 等待存量请求完成（不超过 ShutdownTimeout）-> 执行 OnShutdown 回调。
// 主动 Shutdown 导致的 http.ErrServerClosed 会被吞掉，Run 返回 nil。
func (s *Server) Run() error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case sig := <-quit:
		log.Printf("ginserver: 收到信号 %v，开始优雅关闭...", sig)
		if err := s.GracefulStop(); err != nil {
			return err
		}
		log.Println("ginserver: 服务已退出")
		return nil
	}
}

// GracefulStop 按配置的 ShutdownTimeout 优雅关闭服务器，随后执行 OnShutdown 回调。
// 未启动时为空操作。
func (s *Server) GracefulStop() error {
	timeout := s.conf.ShutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.Shutdown(ctx)
}

// Shutdown 按传入的 ctx 优雅关闭服务器，随后执行 OnShutdown 回调。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.httpSrv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("ginserver: 优雅关闭失败: %w", err)
	}
	for _, fn := range s.onShutdown {
		fn(ctx)
	}
	return nil
}

// Addr 返回监听地址：服务已启动时返回实际监听地址（含内核分配的随机端口），否则返回配置地址。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.actualAddr != "" {
		return s.actualAddr
	}
	return s.conf.Addr()
}

// Config 返回当前生效的服务器配置。
func (s *Server) Config() ServerConfig {
	return s.conf
}

// Engine 返回底层 gin.Engine，用于封装未覆盖的场景（如 pprof 注册）。
func (s *Server) Engine() *gin.Engine {
	return s.engine
}
