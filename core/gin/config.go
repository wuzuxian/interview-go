package ginserver

import (
	"time"

	"github.com/wuzuxian/interview/config"
)

// defaultConfigKey 是 ServerConfig 在配置文件中的默认键。
const defaultConfigKey = "server"

// ServerConfig 是 HTTP 服务器的运行配置。
// 可通过 config.Config 的对应子树绑定，所有字段都有缺省值，零配置即可启动。
//
// 对应的配置文件示例（YAML，键名可通过 WithConfigKey 调整）：
//
//	server:
//	  host: 0.0.0.0
//	  port: 8080
//	  mode: debug                # debug / release / test
//	  base_path: /api            # 全局路由前缀，可为空
//	  read_timeout: 30s
//	  write_timeout: 30s
//	  idle_timeout: 60s
//	  shutdown_timeout: 10s      # 优雅关闭时等待存量请求的最长时间
//	  trusted_proxies: [10.0.0.0/8]
type ServerConfig struct {
	Host            string        `config:"host" default:"0.0.0.0"`
	Port            int           `config:"port" default:"8080"`
	Mode            string        `config:"mode" default:"debug"`
	BasePath        string        `config:"base_path"`
	ReadTimeout     time.Duration `config:"read_timeout" default:"30s"`
	WriteTimeout    time.Duration `config:"write_timeout" default:"30s"`
	IdleTimeout     time.Duration `config:"idle_timeout" default:"60s"`
	ShutdownTimeout time.Duration `config:"shutdown_timeout" default:"10s"`
	// TrustedProxies 为信任的反向代理网段（CIDR）。
	// 未配置（nil）时保持 gin 默认行为（信任所有，启动时会有安全提示）；
	// 配置为空列表时表示不信任任何代理。
	TrustedProxies []string `config:"trusted_proxies"`
}

// LoadServerConfig 从配置管理器的 key 子树（如 "server"）绑定服务器配置。
// key 为空时使用默认键 "server"；子树不存在时返回全部默认值。
func LoadServerConfig(cfg *config.Config, key string) (ServerConfig, error) {
	if key == "" {
		key = defaultConfigKey
	}
	sc := defaultServerConfig()
	if err := cfg.UnmarshalKey(key, &sc); err != nil {
		return ServerConfig{}, err
	}
	return sc, nil
}

// defaultServerConfig 返回全默认值的服务器配置。
func defaultServerConfig() ServerConfig {
	sc := ServerConfig{}
	_ = config.New().Unmarshal(&sc) // 经由 default 标签填充默认值，不会失败
	return sc
}

// Addr 返回监听地址，如 "0.0.0.0:8080"。
func (c ServerConfig) Addr() string {
	return joinHostPort(c.Host, c.Port)
}
