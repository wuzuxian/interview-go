# gormdb — PostgreSQL 数据库模块使用文档

`core/gorm` 基于 [gorm](https://gorm.io) + [postgres driver](https://github.com/go-gorm/postgres) + [pgvector-go](https://github.com/pgvector/pgvector-go) 封装的数据库模块。**包名为 `gormdb`**（目录名为 `core/gorm`）。

- 连接参数与连接池配置全部由 config 配置模块的 `postgres.*` 子树驱动
- 内置 SQL 日志到 slog 的适配（级别、慢查询阈值可配置），可与 logx 模块打通
- 创建时自动验证连通性（可用 `WithSkipPing` 跳过），提供 `Ping`/`Close` 生命周期方法
- 提供 pgvector 向量类型与扩展启用，支持向量存取与相似度检索

## 快速开始

`config.yaml`：

```yaml
postgres:
  host: 127.0.0.1
  port: 5432
  user: postgres
  password: postgres
  dbname: app
  sslmode: disable
  max_open_conns: 20
  slow_threshold: 200ms
```

`main.go`：

```go
db, err := gormdb.NewWithConfig(cfg, "") // cfg 为 *config.Config，读取 postgres.* 子树
if err != nil {
	log.Fatal(err) // 连接失败（含 Ping 验证）在此返回
}
defer db.Close()

// db 内嵌 *gorm.DB，gorm 全部能力直接可用
var count int64
db.Model(&User{}).Count(&count)
db.Where("name = ?", "alice").First(&user)
```

## 配置项

键名默认在 `postgres` 子树下（`WithConfigKey` 可更换），全部字段有默认值：

| 键 | 默认值 | 说明 |
| --- | --- | --- |
| `host` / `port` | `127.0.0.1` / `5432` | 连接地址 |
| `user` / `password` | `postgres` / `postgres` | 凭据 |
| `dbname` | `postgres` | 库名 |
| `sslmode` | `disable` | SSL 模式 |
| `time_zone` | `Asia/Shanghai` | 会话时区 |
| `max_open_conns` / `max_idle_conns` | `20` / `5` | 连接池 |
| `conn_max_lifetime` / `conn_max_idle_time` | `1h` / `30m` | 连接寿命 |
| `log_level` | `warn` | SQL 日志级别：silent / error / warn / info |
| `slow_threshold` | `200ms` | 慢查询阈值，超过以 warn 记录 |

环境变量同样生效：`APP_POSTGRES_HOST=db.internal`。

## 创建方式

```go
// 方式一：从配置管理器创建（推荐）
db, err := gormdb.NewWithConfig(cfg, "")

// 方式二：选项式
db, err := gormdb.New(
	gormdb.WithConfig(cfg),
	gormdb.WithConfigKey("postgres"),
	gormdb.WithHost("127.0.0.1"), gormdb.WithPort(5432), // 显式选项优先于配置值
	gormdb.WithLogger(logger.Logger),                    // SQL 日志路由到 slog
	gormdb.WithLogLevel("info"),
)

// 方式三：DSN 直连（连接池与日志配置仍然生效）
db, err := gormdb.New(gormdb.WithDSN("host=127.0.0.1 user=postgres ..."))
```

## 生命周期与健康检查

```go
db.Ping(ctx)  // 连通性验证（3 秒超时），可用于健康检查端点
db.Close()    // 关闭连接池
db.Config()   // 当前生效配置
```

## pgvector 向量检索

首次使用需启用扩展（需要数据库安装 pgvector 且账号有权限）：

```go
if err := db.EnableVectorExtension(ctx); err != nil {
	log.Fatal(err)
}
```

定义向量模型并迁移：

```go
type Doc struct {
	ID        int64         `gorm:"primaryKey"`
	Content   string        `gorm:"column:content"`
	Embedding gormdb.Vector `gorm:"type:vector(1536)"` // 维度与 embedding 模型一致
}

db.AutoMigrate(&Doc{})
```

插入与相似度检索（`<=>` 余弦距离、`<->` L2 距离）：

```go
db.Create(&Doc{Content: "go 教程", Embedding: gormdb.NewVector(embedding)})

q := gormdb.NewVector(queryEmbedding).String()
var docs []Doc
db.Order(fmt.Sprintf("embedding <=> '%s'::vector", q)). // 余弦相似度倒序
	Limit(5).
	Find(&docs)
```

（向量字面量由 float 切片生成，无注入面；生产中也可改用 `db.Raw` 参数化。）

## SQL 日志

未注入 logger 时使用 slog 默认 logger；与 logx 打通：

```go
db, err := gormdb.New(gormdb.WithConfig(cfg), gormdb.WithLogger(logger.Logger))
```

输出示例（JSON）：

```json
{"level":"WARN","msg":"gorm slow query","sql":"SELECT * FROM users","rows":1000,"elapsed":"1.2s","threshold":"200ms"}
```

- `log_level: silent` 全部关闭；`error` 仅错误；`warn` 错误 + 慢查询；`info` 全部 SQL
- `ErrRecordNotFound` 视为正常业务语义，任何级别都不输出

## 注意事项

- `New` 默认执行连通性验证，依赖不可达时直接返回错误；离线场景（如单测）可用 `gormdb.WithSkipPing()` 跳过这一步，但 `gorm.Open` 内部仍会探测连接，完全离线请改用 sqlite 等驱动配合 `WithEngine` 思路自行组装
- `DB` 内嵌 `*gorm.DB`，`db.Where(...)` 等调用返回的仍是 gorm 会话对象，用法与原生一致
