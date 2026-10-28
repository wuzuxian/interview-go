// Package redisx 基于 go-redis v9 封装的 Redis 模块：
// 与本仓库的 config 配置管理模块联动，创建时验证连通性，
// 内嵌 *redis.Client 透出全部原生能力。
package redisx

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wuzuxian/interview/config"
)

// defaultConfigKey 是 Config 在配置文件中的默认键。
const defaultConfigKey = "redis"

// Config 是 Redis 连接配置。
//
// 对应的配置文件示例（YAML）：
//
//	redis:
//	  addr: 127.0.0.1:6379
//	  password: ""
//	  db: 0
//	  pool_size: 20
//	  min_idle_conns: 5
//	  dial_timeout: 5s
//	  read_timeout: 3s
//	  write_timeout: 3s
//	  conn_max_idle_time: 5m
//	  conn_max_lifetime: 1h
type Config struct {
	Addr            string        `config:"addr" default:"127.0.0.1:6379"`
	Password        string        `config:"password"`
	DB              int           `config:"db"`
	PoolSize        int           `config:"pool_size" default:"20"`
	MinIdleConns    int           `config:"min_idle_conns" default:"5"`
	DialTimeout     time.Duration `config:"dial_timeout" default:"5s"`
	ReadTimeout     time.Duration `config:"read_timeout" default:"3s"`
	WriteTimeout    time.Duration `config:"write_timeout" default:"3s"`
	ConnMaxIdleTime time.Duration `config:"conn_max_idle_time" default:"5m"`
	ConnMaxLifetime time.Duration `config:"conn_max_lifetime" default:"1h"`
}

// LoadConfig 从配置管理器的 key 子树（默认 "redis"）绑定 Redis 配置。
func LoadConfig(cfg *config.Config, key string) (Config, error) {
	if key == "" {
		key = defaultConfigKey
	}
	c := Config{}
	if err := cfg.UnmarshalKey(key, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Client 是封装后的 Redis 客户端，内嵌 *redis.Client，
// 可直接使用 c.Get / c.Set / c.Publish 等全部原生方法，并附加 Ping/Close。
type Client struct {
	*redis.Client
	cfg Config
}

// New 创建 Redis 客户端（已验证连通性）。使用 WithConfig 时从配置管理器绑定，
// WithAddr 等显式选项优先级高于配置值，与书写顺序无关。
func New(opts ...Option) (*Client, error) {
	o := &options{key: defaultConfigKey}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}

	c := &Client{cfg: defaultConfig()}
	if o.config != nil && !o.cfgExplicit {
		conf, err := LoadConfig(o.config, o.key)
		if err != nil {
			return nil, fmt.Errorf("redisx: 绑定 Redis 配置 %q 失败: %w", o.key, err)
		}
		c.cfg = conf
	}
	applyOverrides(&c.cfg, o.override, o.set)

	rc := redis.NewClient(&redis.Options{
		Addr:            c.cfg.Addr,
		Password:        c.cfg.Password,
		DB:              c.cfg.DB,
		PoolSize:        c.cfg.PoolSize,
		MinIdleConns:    c.cfg.MinIdleConns,
		DialTimeout:     c.cfg.DialTimeout,
		ReadTimeout:     c.cfg.ReadTimeout,
		WriteTimeout:    c.cfg.WriteTimeout,
		ConnMaxIdleTime: c.cfg.ConnMaxIdleTime,
		ConnMaxLifetime: c.cfg.ConnMaxLifetime,
	})
	c.Client = rc

	if !o.skipPing {
		if err := c.Ping(context.Background()); err != nil {
			_ = rc.Close()
			return nil, fmt.Errorf("redisx: Redis %s 连接验证失败: %w", c.cfg.Addr, err)
		}
	}

	if o.logger != nil {
		o.logger.Debug("redisx: 客户端已创建", slog.String("addr", c.cfg.Addr), slog.Int("db", c.cfg.DB))
	}
	return c, nil
}

// NewWithConfig 从配置管理器创建客户端的快捷方式，key 为空时使用 "redis"。
func NewWithConfig(cfg *config.Config, key string, opts ...Option) (*Client, error) {
	return New(append(opts, WithConfig(cfg), WithConfigKey(key))...)
}

// Config 返回当前生效的 Redis 配置。
func (c *Client) Config() Config {
	return c.cfg
}

// Ping 验证 Redis 连通性，可用于健康检查端点。
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.Client.Ping(ctx).Err()
}

// Close 关闭客户端连接池。
func (c *Client) Close() error {
	if err := c.Client.Close(); err != nil {
		return fmt.Errorf("redisx: 关闭客户端失败: %w", err)
	}
	return nil
}

// defaultConfig 返回全默认值的 Redis 配置。
func defaultConfig() Config {
	c := Config{}
	_ = config.New().Unmarshal(&c) // 经由 default 标签填充默认值，不会失败
	return c
}
