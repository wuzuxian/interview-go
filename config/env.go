package config

import (
	"os"
	"strings"
)

// defaultEnvReplacer 默认将键中的 "." 替换为 "_"。
var defaultEnvReplacer = strings.NewReplacer(".", "_")

// SetEnvPrefix 设置环境变量名前缀，如设为 "APP" 后，
// 键 "server.port" 对应的环境变量为 APP_SERVER_PORT。传空串表示不使用前缀。
func (c *Config) SetEnvPrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.envPrefix = strings.ToUpper(strings.TrimSpace(prefix))
	if c.envPrefix != "" && !strings.HasSuffix(c.envPrefix, "_") {
		c.envPrefix += "_"
	}
}

// AutomaticEnv 启用环境变量覆盖：此后每次 Get/Unmarshal 都会按
// 前缀 + 大写化后的键（"." 替换为 "_"）查找环境变量，命中则优先采用。
func (c *Config) AutomaticEnv() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.envOn = true
}

// SetEnvKeyReplacer 自定义键到环境变量名的替换规则，设置后不再使用默认的 "." -> "_"。
func (c *Config) SetEnvKeyReplacer(r *strings.Replacer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r == nil {
		r = defaultEnvReplacer
	}
	c.envRepl = r
}

// lookupEnvValue 按配置键查找对应环境变量，返回其字符串值。
func (c *Config) lookupEnvValue(key string) (string, bool) {
	if !c.getEnvOn() {
		return "", false
	}
	name := strings.ToUpper(c.envPrefix + c.getEnvRepl().Replace(key))
	return os.LookupEnv(name)
}

func (c *Config) getEnvPrefix() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.envPrefix
}

func (c *Config) getEnvOn() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.envOn
}

func (c *Config) getEnvRepl() *strings.Replacer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.envRepl
}
