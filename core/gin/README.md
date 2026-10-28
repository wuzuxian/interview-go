# ginserver — 基于 gin 的 HTTP 服务器模块使用文档

`core/gin` 基于 [gin-gonic/gin](https://github.com/gin-gonic/gin) 封装的 HTTP 服务器模块，与本仓库的 [config 配置管理模块](../config/README.md)联动。**包名为 `ginserver`**（目录名为 `core/gin`，与官方 gin 包名区分开）。

- 一行选项实例化服务器，监听地址、端口、超时、运行模式等全部可由配置文件/环境变量驱动
- 提供路由、路由组、全局/组级中间件、404 兜底、静态文件等注册方法
- 内置 CORS 中间件；`Run()` 一键启动并监听 SIGINT/SIGTERM 优雅退出

## 安装与导入

```go
import (
	"github.com/gin-gonic/gin"
	ginserver "github.com/wuzuxian/interview/core/gin"
)
```

## 快速开始

`config.yaml`：

```yaml
server:
  host: 0.0.0.0
  port: 8080
  mode: debug          # debug / release / test
  read_timeout: 30s
  write_timeout: 30s
  shutdown_timeout: 10s
```

`main.go`：

```go
package main

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/config"
	ginserver "github.com/wuzuxian/interview/core/gin"
)

func main() {
	cfg := config.MustLoad(config.Options{
		Files:        []string{"config.yaml"},
		EnvPrefix:    "APP",
		AutomaticEnv: true,
	})

	srv := ginserver.New(
		ginserver.WithConfig(cfg), // 读取配置的 server.* 子树
		ginserver.WithMiddleware(gin.Logger(), gin.Recovery()),
	)

	srv.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong"})
	})

	if err := srv.Run(); err != nil { // 阻塞启动，SIGINT/SIGTERM 优雅退出
		panic(err)
	}
}
```

环境变量可直接覆盖配置：`APP_SERVER_PORT=9090 ./app`。

## 实例化

```go
srv := ginserver.New(opts ...ginserver.Option)
```

常用选项：

| 选项 | 说明 |
| --- | --- |
| `WithConfig(cfg)` | 从配置管理器的 `server` 子树绑定 `ServerConfig` |
| `WithConfigKey("http")` | 更换配置子树键（默认 `server`） |
| `WithServerConfig(sc)` | 直接给定 `ServerConfig` 结构，跳过配置绑定 |
| `WithHost("127.0.0.1")` / `WithPort(9000)` | 覆盖监听地址 / 端口（优先级高于配置值，与选项顺序无关） |
| `WithMode(gin.ReleaseMode)` | 覆盖运行模式 `debug` / `release` / `test` |
| `WithBasePath("/api")` | 全局路由前缀，之后注册的路由自动挂到前缀下 |
| `WithReadTimeout(d)` / `WithWriteTimeout(d)` / `WithIdleTimeout(d)` | 覆盖各超时 |
| `WithShutdownTimeout(d)` | 覆盖优雅关闭等待时长（默认 10s） |
| `WithTrustedProxies("10.0.0.0/8")` | 信任代理网段；不传参数 = 不信任任何代理 |
| `WithMiddleware(mw...)` | 构造时注册全局中间件 |
| `WithEngine(e)` | 使用调用方自建的 `gin.Engine` |
| `WithOnShutdown(fn)` | 注册优雅关闭后的回调（如关闭数据库），可多次调用 |

未配置的字段使用默认值：`host=0.0.0.0`、`port=8080`、`mode=debug`、`read/write_timeout=30s`、`idle_timeout=60s`、`shutdown_timeout=10s`。即零配置也能直接 `ginserver.New().Run()`。

`ServerConfig` 与配置文件的键对应关系（也可用 `ginserver.LoadServerConfig(cfg, key)` 手动加载）：

```yaml
server:
  host: 0.0.0.0
  port: 8080
  mode: debug
  base_path: ""            # 全局路由前缀
  read_timeout: 30s
  write_timeout: 30s
  idle_timeout: 60s
  shutdown_timeout: 10s
  trusted_proxies: []      # CIDR 列表；空列表 = 不信任任何代理；缺省 = gin 默认
```

## 注册路由

```go
srv.GET("/ping", pingHandler)                       // 常用方法均有对应快捷方法
srv.POST("/users", createUser)
srv.PUT / PATCH / DELETE / HEAD / OPTIONS / Any(...) // 同理

srv.Handle("GET", "/legacy", handler)               // 任意方法，method 大小写不敏感
srv.NoRoute(func(c *gin.Context) {                  // 404 兜底
	c.JSON(http.StatusNotFound, gin.H{"message": "not found"})
})
srv.NoMethod(handler)                               // 405 兜底
srv.Health("/healthz")                              // 快捷健康检查端点 {"status":"ok"}
```

参数风格与原生 gin 一致：`handlers ...gin.HandlerFunc` 按序执行，最后一个通常是业务处理器。

## 注册路由组

```go
api := srv.Group("/api/v1")                 // 返回原生 *gin.RouterGroup
{
	api.Use(requireAuth())                  // 组级中间件
	api.GET("/users", listUsers)            // 完整路径 /api/v1/users
	api.GET("/users/:id", getUser)
	api.POST("/users", createUser)
}

// 组级中间件也可以直接写在 Group 参数里
pub := srv.Group("/public", rateLimiter())
```

设置了 `base_path` 时，路由组路径自动追加前缀：`base_path=/api` + `Group("/v1")` + `GET("/x")` → `/api/v1/x`。

## 注册中间件

```go
// 全局中间件：作用于所有路由（含 NoRoute/NoMethod），在路由注册前调用
srv.Use(gin.Logger(), gin.Recovery())
srv.Use(ginserver.AllowCORS("https://a.com", "https://b.com"))

// 构造时注册，效果等价
srv := ginserver.New(ginserver.WithMiddleware(gin.Recovery()))
```

内置中间件：

```go
ginserver.AllowCORS(allowOrigins ...string)
```

- 不传参数 = 允许所有来源（`Access-Control-Allow-Origin: *`）
- 传入完整 origin 列表 = 白名单模式，命中才回写 `Allow-Origin`；OPTIONS 预检请求直接返回 204

也可以直接使用 gin 官方中间件（`gin.Logger`、`gin.Recovery` 等）或任意 `gin.HandlerFunc`。

## 启动与优雅退出

```go
// 方式一（推荐）：阻塞启动 + 监听 SIGINT/SIGTERM 优雅退出
err := srv.Run()

// 方式二：自己管理生命周期
go srv.Start()                                  // 纯阻塞启动
ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
defer cancel()
err := srv.Shutdown(ctx)                        // 手动优雅关闭（完成后执行 OnShutdown 回调）
err = srv.GracefulStop()                        // 等价：按 shutdown_timeout 优雅关闭
```

`Run()` 的退出流程：停止接收新连接 → 等待存量请求完成（最长 `shutdown_timeout`）→ 依次执行 `WithOnShutdown` 注册的回调 → 返回 `nil`。

## 辅助方法

```go
srv.Engine()   // 返回底层 *gin.Engine，用于封装未覆盖的场景（如 pprof）
srv.Addr()     // 监听地址；启动后返回实际地址（port 配置为 0 时为内核分配的随机端口）
srv.Config()   // 当前生效的 ServerConfig
srv.Group()    // 返回 *gin.RouterGroup
srv.Static("/assets", "./public")   // 静态文件（StaticFile / StaticFS 同理）
```

## 完整示例

见项目根目录 `main.go` 与 `config.yaml`：加载配置 → 实例化服务器 → 注册路由与路由组 → `Run()` 优雅运行。启动后可验证：

```bash
curl http://127.0.0.1:8080/ping
curl http://127.0.0.1:8080/api/v1/users
curl -X POST -H 'Content-Type: application/json' -d '{"name":"carol"}' http://127.0.0.1:8080/api/v1/users
APP_SERVER_PORT=9090 go run .   # 环境变量覆盖端口
```

## API 速查

| 类别 | 方法 | 说明 |
| --- | --- | --- |
| 实例化 | `New(opts ...Option)` | 创建服务器，选项见上表 |
| 配置 | `LoadServerConfig(cfg, key)` | 手动从配置管理器加载 `ServerConfig` |
| 路由 | `GET` `POST` `PUT` `PATCH` `DELETE` `HEAD` `OPTIONS` `Any` | 常用方法注册 |
| | `Handle(method, path, handlers...)` | 任意 HTTP 方法 |
| | `NoRoute(handlers...)` / `NoMethod(handlers...)` | 404 / 405 兜底 |
| | `Health(path)` | 健康检查端点 |
| 路由组 | `Group(path, handlers...)` | 返回原生 `*gin.RouterGroup` |
| 中间件 | `Use(mw...)` | 全局中间件；内置 `AllowCORS(origins...)` |
| 静态 | `Static` / `StaticFile` / `StaticFS` | 静态资源 |
| 生命周期 | `Run()` | 阻塞启动 + 信号优雅退出 |
| | `Start()` | 纯阻塞启动 |
| | `GracefulStop()` / `Shutdown(ctx)` | 优雅关闭 |
| 辅助 | `Engine()` `Addr()` `Config()` | 底层引擎、监听地址、生效配置 |
