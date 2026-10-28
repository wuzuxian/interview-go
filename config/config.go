// Package config 提供一个轻量的配置管理模块：
// 支持加载 JSON / YAML / TOML / INI / .env 配置文件，支持环境变量覆盖与默认值，
// 并可将配置绑定到结构体。不支持远程配置与动态刷新，配置在加载完成后即只读稳定。
package config

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// Format 标识配置文件格式。
type Format string

// 支持的配置文件格式。
const (
	FormatJSON Format = "json"
	FormatYAML Format = "yaml"
	FormatTOML Format = "toml"
	FormatINI  Format = "ini"
	FormatENV  Format = "env"
)

// Config 是配置管理器，聚合多个来源的配置并按优先级读取。
//
// 读取优先级（从高到低）：
//  1. Set        程序内显式覆盖
//  2. 环境变量    AutomaticEnv 开启时生效
//  3. LoadFile   按加载顺序，后加载的文件覆盖先加载的
//  4. SetDefault 显式默认值
//  5. 结构体 default tag（仅在 Unmarshal 绑定时生效）
type Config struct {
	mu        sync.RWMutex
	values    map[string]any    // 配置文件合并后的配置树
	overrides map[string]any    // Set 写入的覆盖值
	defaults  map[string]any    // SetDefault 写入的默认值
	envPrefix string            // 环境变量名前缀，如 "APP"
	envOn     bool              // 是否启用环境变量覆盖
	envRepl   *strings.Replacer // 环境变量名替换规则，默认将 "." 替换为 "_"
}

// New 创建一个空的配置管理器。
func New() *Config {
	return &Config{
		values:    map[string]any{},
		overrides: map[string]any{},
		defaults:  map[string]any{},
		envRepl:   defaultEnvReplacer,
	}
}

// Options 是 Load 的一步式构造参数。
type Options struct {
	// Files 为按顺序加载的配置文件路径，后加载的文件覆盖先加载的同名键。
	Files []string
	// Defaults 为初始默认值，键支持 "server.port" 点分路径。
	Defaults map[string]any
	// EnvPrefix 为环境变量名前缀，如 "APP"。
	EnvPrefix string
	// AutomaticEnv 为 true 时启用环境变量覆盖。
	AutomaticEnv bool
}

// Load 按选项构建并返回一个配置管理器。
func Load(opts Options) (*Config, error) {
	c := New()
	for k, v := range opts.Defaults {
		c.SetDefault(k, v)
	}
	for _, f := range opts.Files {
		if err := c.LoadFile(f); err != nil {
			return nil, err
		}
	}
	if opts.EnvPrefix != "" {
		c.SetEnvPrefix(opts.EnvPrefix)
	}
	if opts.AutomaticEnv {
		c.AutomaticEnv()
	}
	return c, nil
}

// MustLoad 同 Load，出错时 panic，适合在程序启动阶段使用。
func MustLoad(opts Options) *Config {
	c, err := Load(opts)
	if err != nil {
		panic(fmt.Sprintf("config: 加载配置失败: %v", err))
	}
	return c
}

// SetDefault 设置默认值，键支持点分路径（如 "server.port"）。
func (c *Config) SetDefault(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	setPath(c.defaults, splitKey(key), value)
}

// Set 显式设置配置值，优先级最高，键支持点分路径。
func (c *Config) Set(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	setPath(c.overrides, splitKey(key), value)
}

// IsSet 报告指定键是否有可用来源（覆盖值、环境变量、配置文件或默认值）。
func (c *Config) IsSet(key string) bool {
	_, ok := c.Get(key)
	return ok
}

// Sub 返回某个子树对应的配置管理器，子配置与其共享覆盖值与环境变量行为；
// 键不存在时返回 nil, false。
func (c *Config) Sub(key string) (*Config, bool) {
	v, ok := c.Get(key)
	if !ok {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	sub := New()
	sub.mu.Lock()
	sub.values = mergeTree(map[string]any{}, m)
	sub.mu.Unlock()
	sub.envPrefix = c.getEnvPrefix()
	sub.envOn = c.getEnvOn()
	sub.envRepl = c.getEnvRepl()
	return sub, true
}

// All 返回合并所有来源后的完整配置树（不含动态环境变量）。
func (c *Config) All() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := mergeTree(map[string]any{}, c.values)
	if len(c.defaults) > 0 {
		out = mergeTree(out, mergeTree(map[string]any{}, c.defaults))
	}
	if len(c.overrides) > 0 {
		out = mergeTree(out, mergeTree(map[string]any{}, c.overrides))
	}
	return out
}

// Keys 返回顶层键名列表。
func (c *Config) Keys() []string {
	all := c.All()
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	return keys
}

// splitKey 将 "server.port" 拆为 ["server", "port"]。
func splitKey(key string) []string {
	parts := strings.Split(key, ".")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// setPath 按 key 路径在树中写入值，沿途补齐缺失的层级。
func setPath(tree map[string]any, path []string, value any) {
	node := tree
	for i, p := range path {
		if i == len(path)-1 {
			node[p] = value
			return
		}
		next, ok := node[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			node[p] = next
		}
		node = next
	}
}

// lookupPath 按 key 路径在树中查找值。
func lookupPath(tree map[string]any, path []string) (any, bool) {
	node := any(tree)
	for _, p := range path {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		node, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return node, true
}

// mergeTree 将 src 深合并进 dst：map 递归合并，其他类型直接覆盖。
func mergeTree(dst, src map[string]any) map[string]any {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				dst[k] = mergeTree(dm, sm)
				continue
			}
			dst[k] = mergeTree(map[string]any{}, sm)
			continue
		}
		if s, ok := v.(map[any]any); ok {
			converted := normalizeMap(s)
			if dm, ok := dst[k].(map[string]any); ok {
				dst[k] = mergeTree(dm, converted)
				continue
			}
			dst[k] = converted
			continue
		}
		dst[k] = v
	}
	return dst
}

// normalizeMap 将 map[any]any（YAML 中可能出现）递归转为 map[string]any。
func normalizeMap(in map[any]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		key := fmt.Sprintf("%v", k)
		switch t := v.(type) {
		case map[any]any:
			out[key] = normalizeMap(t)
		case map[string]any:
			out[key] = normalizeStringMap(t)
		default:
			out[key] = v
		}
	}
	return out
}

// normalizeStringMap 递归确保 map[string]any 内层嵌套也被规范化。
func normalizeStringMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch t := v.(type) {
		case map[any]any:
			out[k] = normalizeMap(t)
		case map[string]any:
			out[k] = normalizeStringMap(t)
		default:
			out[k] = v
		}
	}
	return out
}

// fileExists 报告路径存在且为普通文件。
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
