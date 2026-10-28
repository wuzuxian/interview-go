package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// newReplacer 构造键路径分隔符替换器，用于测试 SetEnvKeyReplacer。
func newReplacer(sep string) *strings.Replacer {
	return strings.NewReplacer(".", sep)
}

// stringReader 构造配置流 reader，用于测试 ReadConfig。
func stringReader(s string) io.Reader {
	return strings.NewReader(s)
}

func TestParseFormats(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content string
		check   func(t *testing.T, c *Config)
	}{
		{
			name: "json",
			file: "app.json",
			content: `{
				"server": {"host": "127.0.0.1", "port": 9000},
				"tags": ["a", "b"],
				"debug": true
			}`,
			check: func(t *testing.T, c *Config) {
				if got := c.GetString("server.host"); got != "127.0.0.1" {
					t.Fatalf("server.host = %q, 期望 127.0.0.1", got)
				}
				if got := c.GetInt("server.port"); got != 9000 {
					t.Fatalf("server.port = %d, 期望 9000", got)
				}
				if !c.GetBool("debug") {
					t.Fatal("debug 应为 true")
				}
				if got := c.GetStringSlice("tags"); len(got) != 2 || got[0] != "a" {
					t.Fatalf("tags = %v, 期望 [a b]", got)
				}
			},
		},
		{
			name: "yaml",
			file: "app.yaml",
			content: `
server:
  host: 0.0.0.0
  port: 8080
  timeout: 30s
db:
  hosts:
    - 10.0.0.1
    - 10.0.0.2
  replicas: 2
`,
			check: func(t *testing.T, c *Config) {
				if got := c.GetString("server.host"); got != "0.0.0.0" {
					t.Fatalf("server.host = %q, 期望 0.0.0.0", got)
				}
				if got := c.GetDuration("server.timeout"); got != 30*time.Second {
					t.Fatalf("server.timeout = %v, 期望 30s", got)
				}
				if got := c.GetStringSlice("db.hosts"); len(got) != 2 || got[1] != "10.0.0.2" {
					t.Fatalf("db.hosts = %v, 期望两个元素", got)
				}
				if got := c.GetInt("db.replicas"); got != 2 {
					t.Fatalf("db.replicas = %d, 期望 2", got)
				}
			},
		},
		{
			name: "toml",
			file: "app.toml",
			content: `
title = "demo"

[server]
host = "10.1.2.3"
port = 7070
ratio = 0.5

[db]
hosts = ["h1", "h2"]
`,
			check: func(t *testing.T, c *Config) {
				if got := c.GetString("title"); got != "demo" {
					t.Fatalf("title = %q, 期望 demo", got)
				}
				if got := c.GetInt("server.port"); got != 7070 {
					t.Fatalf("server.port = %d, 期望 7070", got)
				}
				if got := c.GetFloat64("server.ratio"); got != 0.5 {
					t.Fatalf("server.ratio = %v, 期望 0.5", got)
				}
				if got := c.GetStringSlice("db.hosts"); len(got) != 2 {
					t.Fatalf("db.hosts = %v, 期望两个元素", got)
				}
			},
		},
		{
			name: "ini",
			file: "app.ini",
			content: `
# 顶层注释
app_name = demo

[server]
host = 192.168.1.1
port = 6060
verbose = true   ; 行内注释
password = "p@ss 'quoted'"

[empty_section]
`,
			check: func(t *testing.T, c *Config) {
				if got := c.GetString("app_name"); got != "demo" {
					t.Fatalf("app_name = %q, 期望 demo", got)
				}
				if got := c.GetInt("server.port"); got != 6060 {
					t.Fatalf("server.port = %d, 期望 6060", got)
				}
				if got := c.GetString("server.host"); got != "192.168.1.1" {
					t.Fatalf("server.host = %q, 期望 192.168.1.1", got)
				}
				if !c.GetBool("server.verbose") {
					t.Fatal("server.verbose 应为 true")
				}
				if got := c.GetString("server.password"); got != `p@ss 'quoted'` {
					t.Fatalf("server.password = %q", got)
				}
			},
		},
		{
			name: "env",
			file: "app.env",
			content: `
# 注释
DB_HOST=127.0.0.1
export DB_PORT=3306
MSG="hello\nworld"
RAW='a #b'
FLAGS=a,b,c # 行内注释
EMPTY=
`,
			check: func(t *testing.T, c *Config) {
				if got := c.GetString("DB_HOST"); got != "127.0.0.1" {
					t.Fatalf("DB_HOST = %q", got)
				}
				if got := c.GetInt("DB_PORT"); got != 3306 {
					t.Fatalf("DB_PORT = %d, 期望 3306", got)
				}
				if got := c.GetString("MSG"); got != "hello\nworld" {
					t.Fatalf("MSG = %q, 期望带换行", got)
				}
				if got := c.GetString("RAW"); got != "a #b" {
					t.Fatalf("RAW = %q, 期望 'a #b'", got)
				}
				if got := c.GetStringSlice("FLAGS"); len(got) != 3 {
					t.Fatalf("FLAGS = %v, 期望三个元素", got)
				}
				if got := c.GetString("EMPTY"); got != "" {
					t.Fatalf("EMPTY = %q, 期望空串", got)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New()
			if err := c.LoadFile(writeTemp(t, tc.file, tc.content)); err != nil {
				t.Fatalf("LoadFile 失败: %v", err)
			}
			tc.check(t, c)
		})
	}
}

func TestMergeAndPriority(t *testing.T) {
	base := writeTemp(t, "base.yaml", `
server:
  host: 0.0.0.0
  port: 8000
name: base
`)
	override := writeTemp(t, "override.json", `{"server": {"port": 9000, "tls": true}}`)

	c := New()
	c.SetDefault("server.host", "127.0.0.1")
	c.SetDefault("log.level", "info")
	if err := c.LoadFile(base); err != nil {
		t.Fatal(err)
	}
	if err := c.LoadFile(override); err != nil {
		t.Fatal(err)
	}

	if got := c.GetInt("server.port"); got != 9000 {
		t.Fatalf("后加载文件应覆盖先加载: server.port = %d", got)
	}
	if got := c.GetString("server.host"); got != "0.0.0.0" {
		t.Fatalf("文件应覆盖默认值: server.host = %q", got)
	}
	if got := c.GetString("log.level"); got != "info" {
		t.Fatalf("默认值应生效: log.level = %q", got)
	}
	if !c.GetBool("server.tls") {
		t.Fatal("深合并失败: server.tls 丢失")
	}
	if got := c.GetString("name"); got != "base" {
		t.Fatalf("同级其他键不应丢失: name = %q", got)
	}

	c.Set("server.port", 1)
	if got := c.GetInt("server.port"); got != 1 {
		t.Fatalf("Set 优先级应最高: server.port = %d", got)
	}
	if !c.IsSet("server.tls") || c.IsSet("not.exist") {
		t.Fatal("IsSet 结果不符合预期")
	}
}

func TestAutomaticEnv(t *testing.T) {
	c := New()
	c.SetEnvPrefix("APP")
	c.AutomaticEnv()
	c.SetDefault("server.port", 8000)
	c.SetDefault("server.host", "0.0.0.0")

	t.Setenv("APP_SERVER_PORT", "9999")
	t.Setenv("APP_DEBUG", "true")
	t.Setenv("APP_TIMEOUT", "2m")

	if got := c.GetInt("server.port"); got != 9999 {
		t.Fatalf("环境变量应覆盖默认值: server.port = %d", got)
	}
	if got := c.GetString("server.host"); got != "0.0.0.0" {
		t.Fatalf("未设置的环境变量不应影响取值: server.host = %q", got)
	}
	if got := c.GetBool("debug"); got != true {
		t.Fatalf("无前缀层级键 debug 应命中 APP_DEBUG, got %v", got)
	}
	if got := c.GetDuration("timeout"); got != 2*time.Minute {
		t.Fatalf("timeout = %v, 期望 2m", got)
	}

	// SetEnvKeyReplacer 自定义映射。
	c2 := New()
	c2.SetEnvPrefix("SVC")
	c2.SetEnvKeyReplacer(newReplacer("__"))
	c2.AutomaticEnv()
	t.Setenv("SVC_DB__MASTER__HOST", "db1")
	if got := c2.GetString("db.master.host"); got != "db1" {
		t.Fatalf("自定义 replacer 未生效: %q", got)
	}
}

func TestUnmarshalStruct(t *testing.T) {
	yaml := writeTemp(t, "app.yaml", `
server:
  host: 10.0.0.9
  port: 8081
  timeout: 45s
  features: [a, b]
db:
  primary:
    dsn: root@tcp(127.0.0.1:3306)/demo
  max_open: 100
limits:
  rate: 3.5
  started_at: 2026-01-02T15:04:05Z
tags: [x, y]
extra:
  k1: v1
  k2: 2
`)
	c, err := Load(Options{Files: []string{yaml}, EnvPrefix: "APP", AutomaticEnv: true})
	if err != nil {
		t.Fatal(err)
	}

	type DBConf struct {
		Primary struct {
			DSN string `config:"dsn"`
		} `config:"primary"`
		MaxOpen int `config:"max_open" default:"10"`
		Backup  *struct {
			DSN string `config:"dsn"`
		} `config:"backup"`
	}
	type AppConf struct {
		Server struct {
			Host     string        `config:"host" default:"0.0.0.0"`
			Port     int           `config:"port" default:"8000"`
			Timeout  time.Duration `config:"timeout" default:"30s"`
			Features []string      `config:"features"`
			Enabled  bool          `config:"enabled" default:"true"`
		} `config:"server"`
		DB     DBConf `config:"db"`
		Limits struct {
			Rate      float64   `config:"rate"`
			StartedAt time.Time `config:"started_at"`
		} `config:"limits"`
		Tags  []string          `config:"tags"`
		Extra map[string]string `config:"extra"`
	}

	var conf AppConf
	if err := c.Unmarshal(&conf); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if conf.Server.Host != "10.0.0.9" {
		t.Fatalf("Host = %q", conf.Server.Host)
	}
	if conf.Server.Port != 8081 {
		t.Fatalf("Port = %d", conf.Server.Port)
	}
	if conf.Server.Timeout != 45*time.Second {
		t.Fatalf("Timeout = %v", conf.Server.Timeout)
	}
	if len(conf.Server.Features) != 2 || conf.Server.Features[0] != "a" {
		t.Fatalf("Features = %v", conf.Server.Features)
	}
	if !conf.Server.Enabled {
		t.Fatal("default tag 应生效: Enabled")
	}
	if conf.DB.Primary.DSN != "root@tcp(127.0.0.1:3306)/demo" {
		t.Fatalf("DSN = %q", conf.DB.Primary.DSN)
	}
	if conf.DB.MaxOpen != 100 {
		t.Fatalf("MaxOpen = %d", conf.DB.MaxOpen)
	}
	if conf.DB.Backup != nil {
		t.Fatal("配置中不存在的指针字段应保持 nil")
	}
	if conf.Limits.Rate != 3.5 {
		t.Fatalf("Rate = %v", conf.Limits.Rate)
	}
	want, _ := time.Parse(time.RFC3339, "2026-01-02T15:04:05Z")
	if !conf.Limits.StartedAt.Equal(want) {
		t.Fatalf("StartedAt = %v, 期望 %v", conf.Limits.StartedAt, want)
	}
	if len(conf.Tags) != 2 {
		t.Fatalf("Tags = %v", conf.Tags)
	}
	if conf.Extra["k1"] != "v1" || conf.Extra["k2"] != "2" {
		t.Fatalf("Extra = %v", conf.Extra)
	}
}

func TestUnmarshalEnvAndDefaults(t *testing.T) {
	t.Setenv("SRV_PORT", "7000")
	t.Setenv("APP_SERVER_PORT", "7000") // 自动推导: server.port -> APP_SERVER_PORT
	t.Setenv("APP_DB_HOST", "env-db-host")

	c := New()
	c.SetEnvPrefix("APP")
	c.AutomaticEnv()

	type Conf struct {
		Server struct {
			Port int    `config:"port" default:"1"`
			Name string `env:"SRV_PORT"` // env 标签始终按完整变量名查找
		} `config:"server"`
		DB struct {
			Host string `config:"host"`
		} `config:"db"`
		Missing string `config:"missing"` // 无任何来源，保持零值
	}
	var conf Conf
	if err := c.Unmarshal(&conf); err != nil {
		t.Fatal(err)
	}
	// 自动推导: server.port -> APP_SERVER_PORT
	if conf.Server.Port != 7000 {
		t.Fatalf("自动推导环境变量未生效: Port = %d", conf.Server.Port)
	}
	if conf.Server.Name != "7000" {
		t.Fatalf("env tag 未生效: Name = %q", conf.Server.Name)
	}
	if conf.DB.Host != "env-db-host" {
		t.Fatalf("嵌套路径环境变量推导失败: Host = %q", conf.DB.Host)
	}
	if conf.Missing != "" {
		t.Fatalf("无来源字段应保持零值: %q", conf.Missing)
	}

	// 无环境变量时 default tag 兜底。
	t.Setenv("SRV_PORT", "")
	os.Unsetenv("SRV_PORT")
	t.Setenv("APP_SERVER_PORT", "")
	os.Unsetenv("APP_SERVER_PORT")
	t.Setenv("APP_DB_HOST", "")
	os.Unsetenv("APP_DB_HOST")

	var conf2 Conf
	if err := c.Unmarshal(&conf2); err != nil {
		t.Fatal(err)
	}
	if conf2.Server.Port != 1 {
		t.Fatalf("default tag 兜底失败: Port = %d", conf2.Server.Port)
	}
}

func TestUnmarshalKeyAndSub(t *testing.T) {
	c := New()
	c.SetDefault("db.primary.dsn", "root@localhost/dev")
	c.Set("db.replica.enabled", true)

	type DSN struct {
		DSN string `config:"dsn"`
	}
	var p DSN
	if err := c.UnmarshalKey("db.primary", &p); err != nil {
		t.Fatal(err)
	}
	if p.DSN != "root@localhost/dev" {
		t.Fatalf("UnmarshalKey 失败: %q", p.DSN)
	}

	sub, ok := c.Sub("db")
	if !ok {
		t.Fatal("Sub(db) 应存在")
	}
	if !sub.GetBool("replica.enabled") {
		t.Fatal("子配置应包含完整子树")
	}
	if _, ok := c.Sub("nope"); ok {
		t.Fatal("不存在的键 Sub 应返回 false")
	}
	if _, ok := c.Sub("db.primary.dsn"); ok {
		t.Fatal("叶子键 Sub 应返回 false")
	}
}

func TestErrorsAndEdgeCases(t *testing.T) {
	c := New()
	if err := c.LoadFile(writeTemp(t, "bad.txt", "x=1")); err == nil {
		t.Fatal("不支持的扩展名应报错")
	}
	if err := c.LoadFile(writeTemp(t, "bad.json", "{oops")); err == nil {
		t.Fatal("解析失败应报错")
	}
	if err := c.LoadFile(writeTemp(t, "bad.ini", "no_eq_line")); err == nil {
		t.Fatal("INI 缺少 = 应报错")
	}
	if err := c.LoadFile(writeTemp(t, "bad.env", "NO_EQUAL")); err == nil {
		t.Fatal(".env 缺少 = 应报错")
	}
	if err := c.LoadFile(writeTemp(t, "bad.yaml", "[1, 2, 3]")); err == nil {
		t.Fatal("YAML 顶层非映射应报错")
	}

	if _, ok := c.Get(""); ok {
		t.Fatal("空键不应命中")
	}
	if c.GetString("none") != "" || c.GetInt("none") != 0 {
		t.Fatal("缺失键应返回零值")
	}

	// ReadConfig 指定格式。
	c2 := New()
	if err := c2.ReadConfig(stringReader(`{"a": 1}`), FormatJSON); err != nil {
		t.Fatal(err)
	}
	if c2.GetInt("a") != 1 {
		t.Fatal("ReadConfig 未生效")
	}
}

func TestLoadOptionsAndPanics(t *testing.T) {
	f := writeTemp(t, "app.toml", `[server]
port = 1234
`)
	c := MustLoad(Options{
		Files:    []string{f},
		Defaults: map[string]any{"server.host": "1.2.3.4"},
	})
	if c.GetInt("server.port") != 1234 || c.GetString("server.host") != "1.2.3.4" {
		t.Fatal("MustLoad 选项未生效")
	}

	defer func() {
		if recover() == nil {
			t.Fatal("加载失败应 panic")
		}
	}()
	MustLoad(Options{Files: []string{"not-exist.yaml"}})
}

func TestKeysAndAllAndJSON(t *testing.T) {
	c := New()
	c.SetDefault("a.b", 1)
	c.Set("c", "x")
	if err := c.ReadConfig(stringReader(`{"d": {"e": true}}`), FormatJSON); err != nil {
		t.Fatal(err)
	}
	all := c.All()
	if len(all) != 3 {
		t.Fatalf("All() 应有 3 个顶层键, got %d", len(all))
	}
	keys := c.Keys()
	if len(keys) != 3 {
		t.Fatalf("Keys() 应有 3 个顶层键, got %d", len(keys))
	}
	if c.JSON("a") == "" || c.JSON("missing") != "" {
		t.Fatal("JSON() 结果不符合预期")
	}
}
