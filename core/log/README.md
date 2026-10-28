# logx — 日志模块使用文档

`core/log` 基于 `log/slog` 封装的日志模块。**包名为 `logx`**（目录名为 `core/log`）。

- 级别（debug/info/warn/error）、格式（json/text）、输出（stdout/stderr/文件，可多输出）全部由 config 配置模块的 `log.*` 子树驱动
- 支持 `SetAsDefault` 一键接管 slog 包级函数与标准库 `log`
- 文件输出自动创建目录，进程退出前 `Close()`

## 快速开始

`config.yaml`：

```yaml
log:
  level: info        # debug / info / warn / error
  format: json       # json / text
  output: stdout     # stdout、stderr 或文件路径，逗号分隔多输出
```

`main.go`：

```go
logger := logx.NewFromConfig(cfg, "") // cfg 为 *config.Config，读取 log.* 子树
defer logger.Close()
logx.SetAsDefault(logger)             // 此后 slog.Info 等包级函数都走该配置

logger.Info("service started", slog.String("addr", ":8080"))
logger.Error("boom", slog.String("error", err.Error()))
```

## 输出效果

`format: json`（默认）：

```json
{"time":"2026-10-27T12:00:00+08:00","level":"INFO","msg":"service started","addr":":8080"}
```

`format: text`：

```
2026-10-27T12:00:00+08:00 INFO service started addr=:8080
```

## 创建方式

```go
// 方式一：从配置管理器创建（推荐），key 为空时读取 "log" 子树
logger := logx.NewFromConfig(cfg, "")

// 方式二：选项式，显式选项优先于配置值（与书写顺序无关）
logger := logx.New(
	logx.WithConfig(cfg),        // 绑定 log.* 配置
	logx.WithConfigKey("log"),   // 更换配置子树键
	logx.WithLevel("debug"),     // 覆盖级别
	logx.WithFormat("text"),     // 覆盖格式
	logx.WithOutput("stdout,/var/log/app.log"), // 覆盖输出（多输出）
)

// 方式三：纯代码，不经配置文件
logger := logx.New(logx.WithLevel("info"), logx.WithOutput("stdout"))
```

配置绑定失败（如子树缺失字段类型错误）不会 panic：保留默认值并在 stderr 打印一条引导提示。

## 输出目标说明

`output` 支持逗号分隔的多目标：

| 值 | 行为 |
| --- | --- |
| `stdout` / `stderr` | 标准输出 / 错误输出（默认 `stdout`） |
| `/var/log/app.log` | 追加写文件，目录不存在自动创建 |
| `stdout,/var/log/app.log` | 同时输出到多个目标 |

## 常用方法

```go
logger.Debug/Info/Warn/Error(msg string, args ...any) // 结构化键值对
logger.With("module", "api").Info("...")              // 派生带上下文的子 logger（内嵌 slog 能力）
logger.Config()                                       // 当前生效配置
logger.Level()                                        // 当前 slog.Level
logger.Close()                                        // 关闭文件输出（纯 stdout 无需关心）
logx.ParseLevel("warn")                               // 字符串 -> slog.Level
```

## 供其他模块使用

`*logx.Logger` 内嵌 `*slog.Logger`，可直接传给其他 core 模块：

```go
db, err := gormdb.New(gormdb.WithConfig(cfg), gormdb.WithLogger(logger.Logger))
rdb, err := redisx.NewWithConfig(cfg, "", redisx.WithLogger(logger.Logger))
```

## 注意事项

- 未被级别放行的日志不会序列化（如 info 级别下 `Debug` 零开销跳过）
- 输出到文件时进程退出前请 `Close()`；长期运行的服务建议配合 logrotate 或使用 `stdout` + 容器日志收集
