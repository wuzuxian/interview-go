# redisx — Redis 模块使用文档

`core/redis` 基于 [go-redis v9](https://github.com/redis/go-redis) 封装的 Redis 模块。**包名为 `redisx`**（目录名为 `core/redis`）。

- 连接参数与连接池配置全部由 config 配置模块的 `redis.*` 子树驱动
- 创建时自动验证连通性（可用 `WithSkipPing` 跳过），提供 `Ping`/`Close` 生命周期方法
- 内嵌 `*redis.Client`，原生 API 全部直接可用

## 快速开始

`config.yaml`：

```yaml
redis:
  addr: 127.0.0.1:6379
  password: ""       # 无密码留空
  db: 0
  pool_size: 20
```

`main.go`：

```go
rdb, err := redisx.NewWithConfig(cfg, "") // cfg 为 *config.Config，读取 redis.* 子树
if err != nil {
	log.Fatal(err) // 连接失败（含 Ping 验证）在此返回
}
defer rdb.Close()

// rdb 内嵌 *redis.Client，原生方法直接可用（v9 全部方法需要 context）
ctx := context.Background()
rdb.Set(ctx, "key", "value", 10*time.Minute)
val, err := rdb.Get(ctx, "key").Result()
```

## 配置项

键名默认在 `redis` 子树下（`WithConfigKey` 可更换），全部字段有默认值：

| 键 | 默认值 | 说明 |
| --- | --- | --- |
| `addr` | `127.0.0.1:6379` | 连接地址 |
| `password` | 空 | 密码（无密码留空） |
| `db` | `0` | 逻辑库编号 |
| `pool_size` / `min_idle_conns` | `20` / `5` | 连接池 |
| `dial_timeout` | `5s` | 建连超时 |
| `read_timeout` / `write_timeout` | `3s` / `3s` | 读写超时 |
| `conn_max_idle_time` / `conn_max_lifetime` | `5m` / `1h` | 连接寿命 |

环境变量同样生效：`APP_REDIS_ADDR=redis.internal:6379`。

## 创建方式

```go
// 方式一：从配置管理器创建（推荐）
rdb, err := redisx.NewWithConfig(cfg, "")

// 方式二：选项式
rdb, err := redisx.New(
	redisx.WithConfig(cfg),
	redisx.WithConfigKey("redis"),
	redisx.WithAddr("127.0.0.1:6379"), // 显式选项优先于配置值
	redisx.WithDB(3),
	redisx.WithPoolSize(50),
	redisx.WithLogger(logger.Logger),
)

// 方式三：纯代码
rdb, err := redisx.New(redisx.WithServerConfig(redisx.Config{Addr: "127.0.0.1:6379"}))
```

配置绑定失败（子树字段类型错误等）在 `New` 返回值中体现，不会 panic。

## 生命周期与健康检查

```go
rdb.Ping(ctx)  // 连通性验证（3 秒超时），可用于健康检查端点
rdb.Close()    // 关闭连接池
rdb.Config()   // 当前生效配置
```

## 常用操作示例

```go
ctx := context.Background()

// 字符串
rdb.Set(ctx, "greeting", "hello", time.Minute)
v, _ := rdb.Get(ctx, "greeting").Result()

// Hash
rdb.HSet(ctx, "user:1", "name", "alice", "age", 20)
m, _ := rdb.HGetAll(ctx, "user:1").Result()

// 列表 / 集合
rdb.RPush(ctx, "queue", "task1", "task2")
rdb.SAdd(ctx, "tags", "go", "redis")

// 发布订阅
rdb.Publish(ctx, "events", "payload")

// 分布式锁的简化用法：SET NX
ok, _ := rdb.SetNX(ctx, "lock:job", "holder", 10*time.Second).Result()
```

所有命令返回 `*Cmd` 系列，`.Err()` / `.Result()` 取结果——与 go-redis v9 完全一致，无需学习新 API。

## 注意事项

- go-redis v9 的所有命令都需要传入 `context`，超时与取消由 ctx 控制（客户端还内置了 `read_timeout`/`write_timeout` 兜底）
- `New` 默认执行一次 `Ping` 验证；Redis 未启动时会返回错误，离线场景（如单测）用 `redisx.WithSkipPing()`
