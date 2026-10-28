# config — Go 配置管理模块使用文档

轻量级配置管理模块：加载本地配置文件（JSON / YAML / TOML / INI / .env），结合环境变量覆盖与默认值，将配置读取为强类型值或绑定到结构体。加载完成后配置即稳定只读，不涉及远程配置与动态刷新。

## 特性一览

- 支持五种文件格式：`.json` / `.yaml` `.yml` / `.toml` / `.ini` / `.env`，按扩展名自动识别
- 支持多文件按顺序加载，后加载的文件覆盖先加载的同名键（适合 `base.yaml` + `override.yaml` 场景）
- 支持环境变量覆盖：可配置前缀与键名替换规则，也能在结构体字段上用 `env` 标签精确指定
- 支持默认值：`SetDefault` 编程式设置，或结构体字段 `default` 标签声明
- 类型化读取：`GetInt` / `GetDuration` / `GetStringSlice` 等，值类型不符时自动转换（如字符串 `"8080"` 自动转整数）
- 结构体绑定：一次性将配置映射到强类型结构体，支持嵌套、切片、map、指针、`time.Duration`、`time.Time`

## 安装

模块位于 `github.com/wuzuxian/interview/config`：

```go
import "github.com/wuzuxian/interview/config"
```

第三方依赖仅 YAML 与 TOML 解析器（`gopkg.in/yaml.v3`、`github.com/BurntSushi/toml`），INI 与 .env 为内置实现。

## 快速开始

`config.yaml`：

```yaml
server:
  host: 0.0.0.0
  port: 8080
  timeout: 30s
log:
  level: info
```

`main.go`：

```go
package main

import (
	"fmt"
	"log"

	"github.com/wuzuxian/interview/config"
)

type AppConf struct {
	Server struct {
		Host    string        `config:"host" default:"127.0.0.1"`
		Port    int           `config:"port" default:"8000"`
		Timeout time.Duration `config:"timeout" default:"5s"`
	} `config:"server"`
	Log struct {
		Level string `config:"level" default:"debug"`
	} `config:"log"`
}

func main() {
	cfg, err := config.Load(config.Options{
		Files:        []string{"config.yaml"},
		EnvPrefix:    "APP",
		AutomaticEnv: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	var conf AppConf
	if err := cfg.Unmarshal(&conf); err != nil {
		log.Fatal(err)
	}
	fmt.Println(conf.Server.Host, conf.Server.Port, conf.Server.Timeout)

	// 也可以按需点取单个值
	fmt.Println(cfg.GetString("log.level"))
}
```

启动前执行 `export APP_SERVER_PORT=9090`，则 `conf.Server.Port` 将读到 `9090`（环境变量覆盖文件值）。

## 加载配置文件

### 一步式加载（推荐）

```go
cfg, err := config.Load(config.Options{
	Files:        []string{"config.yaml", "config.override.yaml"}, // 按顺序合并，后者覆盖前者
	Defaults:     map[string]any{"log.level": "info"},             // 初始默认值
	EnvPrefix:    "APP",
	AutomaticEnv: true,
})
```

出错即 panic 的版本（适合 `main` 启动阶段）：

```go
cfg := config.MustLoad(config.Options{Files: []string{"config.yaml"}})
```

### 逐步构建

```go
cfg := config.New()
cfg.SetDefault("server.port", 8000)

if err := cfg.LoadFile("config.yaml"); err != nil { // 可连续加载多个文件
	log.Fatal(err)
}
if err := cfg.LoadFile("config.override.yaml"); err != nil {
	log.Fatal(err)
}
cfg.SetEnvPrefix("APP")
cfg.AutomaticEnv()
```

### 从内存 / 网络 IO 读取

已有字节流时可直接按指定格式读取，不落盘：

```go
err := cfg.ReadConfig(r, config.FormatJSON) // r 为 io.Reader
```

## 支持的文件格式

| 扩展名 | 格式 | 说明 |
| --- | --- | --- |
| `.json` | JSON | 标准库解析 |
| `.yaml` `.yml` | YAML | 嵌套映射、列表 |
| `.toml` | TOML | 嵌套表、内联数组 |
| `.ini` | INI | `[section]` 分节，`section.key` 访问；值保留为字符串，读取时自动转型 |
| `.env` | dotenv | `KEY=VALUE`，键直接作为顶层键使用 |

各格式示例（等价内容）：

```yaml
# config.yaml
server:
  host: 0.0.0.0
  port: 8080
tags: [web, api]
```

```json
{ "server": { "host": "0.0.0.0", "port": 8080 }, "tags": ["web", "api"] }
```

```toml
# config.toml
tags = ["web", "api"]

[server]
host = "0.0.0.0"
port = 8080
```

```ini
# config.ini
tags = web,api

[server]
host = 0.0.0.0
port = 8080
```

```bash
# config.env —— 键以原样顶层键存在
SERVER_HOST=0.0.0.0
SERVER_PORT=8080
TAGS=web,api
```

.ini 与 .env 细节：

- 两者均支持 `#`、`;` 行注释与行内注释；值两侧成对引号会被剥掉
- .env 支持 `export KEY=VALUE` 前缀；双引号内支持 `\n` `\t` `\"` `\\` 转义，单引号内原样保留

## 读取优先级

从高到低：

1. `Set(key, value)` — 程序内显式覆盖
2. 环境变量 — `AutomaticEnv` 开启后生效（含结构体字段 `env` 标签）
3. `LoadFile` / `ReadConfig` 加载的文件内容 — 后加载覆盖先加载
4. `SetDefault` 设置的默认值
5. 结构体字段 `default` 标签（仅在 `Unmarshal` 绑定时生效）

## 按键读取（点分路径）

嵌套键用 `.` 分隔访问：

```go
cfg.GetString("server.host")     // "0.0.0.0"
cfg.GetInt("server.port")        // 8080
cfg.GetDuration("server.timeout") // 30s，字符串 "30s" 自动解析
cfg.GetBool("server.debug")
cfg.GetFloat64("server.ratio")
cfg.GetTime("deploy.at")         // 支持 RFC3339、"2006-01-02 15:04:05"、Unix 时间戳
cfg.GetStringSlice("tags")       // ["web", "api"]；单个字符串按英文逗号拆分
cfg.GetIntSlice("ports")
cfg.GetBoolSlice("flags")
cfg.GetFloat64Slice("rates")
cfg.GetDurationSlice("timeouts")
cfg.GetStringMap("server")       // map[string]any
cfg.GetStringMapString("labels") // map[string]string
cfg.GetTime("server.started")
```

所有 `GetXXX` 系列方法在键不存在或类型转换失败时返回目标类型的零值，不会 panic；需要区分"缺失"与"零值"时用：

```go
v, ok := cfg.Get("server.host")
if cfg.IsSet("server.host") { ... }
```

其他辅助方法：

```go
cfg.Set("server.port", 9090) // 程序内覆盖，优先级最高
cfg.All()                    // map[string]any，合并所有来源后的完整配置树
cfg.Keys()                   // 顶层键名列表
cfg.JSON("server")           // 该键对应值的 JSON 字符串，调试用
```

## 环境变量

开启后每次读取都会尝试环境变量，命中即覆盖文件值。键映射规则：`前缀 + 配置键大写化（"." 替换为 "_"）`。

```go
cfg.SetEnvPrefix("APP") // server.port -> APP_SERVER_PORT
cfg.AutomaticEnv()
cfg.SetEnvKeyReplacer(strings.NewReplacer(".", "__")) // 可选：server.port -> APP_SERVER__PORT
```

```bash
export APP_SERVER_PORT=9090
export APP_TAGS=a,b,c        # GetStringSlice("tags") -> ["a", "b", "c"]
export APP_SERVER_TIMEOUT=2m # GetDuration("server.timeout") -> 2m0s
```

- `SetEnvPrefix` 传空串表示不使用前缀（`server.port` -> `SERVER_PORT`）
- 环境变量值均为字符串，`GetXXX` 读取时会自动转换为目标类型
- 按键读取（`Get`/`GetXXX`）与结构体绑定（`Unmarshal`）两条路径都支持环境变量覆盖

## 结构体绑定

```go
type DBConf struct {
	DSN     string        `config:"dsn" default:"root@localhost/dev"`
	MaxOpen int           `config:"max_open" default:"10"`
	Timeout time.Duration `config:"timeout" default:"3s"`
}

type AppConf struct {
	Server struct {
		Host    string   `config:"host"`
		Port    int      `config:"port"`
		Tags    []string `config:"tags"`
	} `config:"server"`
	DB     DBConf            `config:"db"`
	Labels map[string]string `config:"labels"`
	Token  string            `env:"APP_AUTH_TOKEN"` // env 标签：精确指定完整环境变量名，始终生效
}

var conf AppConf
err := cfg.Unmarshal(&conf)

// 只绑定某个子树
var db DBConf
err = cfg.UnmarshalKey("db", &db)
```

字段标签说明：

| 标签 | 作用 | 缺省行为 |
| --- | --- | --- |
| `config:"key"` | 配置键名；也兼容 `mapstructure` / `json` / `yaml` / `toml` 标签 | 使用字段名（大小写不敏感） |
| `config:"-"` | 忽略该字段 | — |
| `default:"..."` | 所有来源都缺失时的默认值（字符串，自动转型为目标类型） | 保持零值 |
| `env:"NAME"` | 环境变量完整名称，优先级高于配置文件 | 按前缀 + 路径自动推导 |

绑定行为要点：

- 支持字段类型：基本类型、`time.Duration`（`"30s"` 或数字纳秒）、`time.Time`、切片/数组、`map[string]T`、嵌套结构体、结构体指针
- 值类型不符时自动转换（如 YAML 里写 `port: "8080"` 字符串也能绑到 `int` 字段）；越界（如 `int8` 放 99999）会返回错误
- 配置树与环境变量中都没有、也没有 `default` 标签的字段保持零值；非指针嵌套结构体仍会继续递归处理其内部字段的环境变量与默认值
- 结构体指针字段在配置缺失时保持 `nil`，存在时自动分配

## 子配置

取出某个子树作为独立的配置对象，读取 API 完全一致：

```go
sub, ok := cfg.Sub("server")
if ok {
	fmt.Println(sub.GetInt("port")) // 等价于 cfg.GetInt("server.port")
}
```

## 完整示例程序

```go
package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/wuzuxian/interview/config"
)

type ServerConf struct {
	Host    string        `config:"host" default:"0.0.0.0"`
	Port    int           `config:"port" default:"8000"`
	Timeout time.Duration `config:"timeout" default:"30s"`
	Origins []string      `config:"origins"`
}

type DBConf struct {
	DSN     string `config:"dsn"`
	MaxOpen int    `config:"max_open" default:"10"`
}

type AppConf struct {
	Server ServerConf `config:"server"`
	DB     DBConf     `config:"db"`
	Token  string     `env:"APP_AUTH_TOKEN"`
}

func main() {
	files := []string{"config.yaml"}
	if _, err := os.Stat("config.override.yaml"); err == nil {
		files = append(files, "config.override.yaml") // 本地覆盖文件，存在才加载
	}

	cfg, err := config.Load(config.Options{
		Files:        files,
		Defaults:     map[string]any{"log.level": "info"},
		EnvPrefix:    "APP",
		AutomaticEnv: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	var conf AppConf
	if err := cfg.Unmarshal(&conf); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("server: %s:%d (timeout %s)\n", conf.Server.Host, conf.Server.Port, conf.Server.Timeout)
	fmt.Printf("origins: %s\n", strings.Join(conf.Server.Origins, ", "))
	fmt.Printf("db: %s (max open %d)\n", conf.DB.DSN, conf.DB.MaxOpen)

	// 运行前 export APP_AUTH_TOKEN=secret，这里就能读到
	if conf.Token != "" {
		fmt.Println("auth token 已通过环境变量注入")
	}
}
```

## API 速查

| 类别 | 方法 | 说明 |
| --- | --- | --- |
| 构造 | `config.New()` | 空配置管理器 |
| | `config.Load(Options)` / `config.MustLoad(Options)` | 一步式加载文件、默认值与环境变量 |
| 加载 | `LoadFile(path)` | 按扩展名识别格式并合并 |
| | `ReadConfig(io.Reader, Format)` | 从流按指定格式合并 |
| 默认/覆盖 | `SetDefault(key, v)` / `Set(key, v)` | 键均支持点分路径 |
| 读取 | `Get(key)` | 返回 `any` 与是否存在 |
| | `GetString` `GetBool` `GetInt` `GetInt32` `GetInt64` `GetUint` `GetUint64` `GetFloat32` `GetFloat64` | 基本类型 |
| | `GetDuration` `GetTime` | 时间类型 |
| | `GetStringSlice` `GetIntSlice` `GetBoolSlice` `GetFloat64Slice` `GetDurationSlice` | 切片，字符串自动按逗号拆分 |
| | `GetStringMap` `GetStringMapString` | map 读取 |
| | `IsSet(key)` `All()` `Keys()` `JSON(key)` | 存在性、全树、顶层键、JSON 调试输出 |
| 环境变量 | `SetEnvPrefix(prefix)` `AutomaticEnv()` `SetEnvKeyReplacer(r)` | 开启环境变量覆盖 |
| 绑定 | `Unmarshal(&v)` / `UnmarshalKey("db", &v)` | 绑定整树 / 子树到结构体 |
| 子树 | `Sub(key)` | 取子配置对象 |
