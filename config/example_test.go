package config_test

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/wuzuxian/interview/config"
)

// Example_quickStart 演示典型用法：加载 YAML 配置 + 环境变量覆盖 + 结构体绑定。
func Example_quickStart() {
	dir, err := os.MkdirTemp("", "cfgdemo")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	yamlPath := filepath.Join(dir, "app.yaml")
	yamlContent := `
server:
  host: 0.0.0.0
  port: 8080
  timeout: 30s
  origins: [https://a.com, https://b.com]
log:
  level: info
`
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0o644); err != nil {
		log.Fatal(err)
	}
	os.Setenv("APP_SERVER_PORT", "9090") // 模拟部署时注入的环境变量
	defer os.Unsetenv("APP_SERVER_PORT")

	type LogConf struct {
		Level string `config:"level" default:"debug"`
	}
	type AppConf struct {
		Server struct {
			Host    string        `config:"host" default:"127.0.0.1"`
			Port    int           `config:"port" default:"8000"`
			Timeout time.Duration `config:"timeout" default:"5s"`
			Origins []string      `config:"origins"`
		} `config:"server"`
		Log LogConf `config:"log"`
	}

	cfg, err := config.Load(config.Options{
		Files:        []string{yamlPath},
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

	fmt.Println(conf.Server.Host, conf.Server.Port, conf.Server.Timeout, conf.Server.Origins)
	fmt.Println(conf.Log.Level)

	// 也可以按需点取，环境变量同样生效。
	fmt.Println(cfg.GetString("log.level"), cfg.IsSet("server.port"))

	// Output:
	// 0.0.0.0 9090 30s [https://a.com https://b.com]
	// info
	// info true
}
