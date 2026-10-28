// Package gormdb 基于 gorm 封装的 PostgreSQL 数据库模块：
// 与本仓库的 config 配置管理模块联动，内置连接池参数、基于 slog 的 SQL 日志适配，
// 并提供 pgvector 向量扩展的启用与模型示例。
package gormdb

import (
	"fmt"
	"net"
	"time"

	"github.com/wuzuxian/interview/config"
)

// defaultConfigKey 是 Config 在配置文件中的默认键。
const defaultConfigKey = "postgres"

// Config 是数据库连接配置。
//
// 对应的配置文件示例（YAML，键名可通过 WithConfigKey 调整）：
//
//	postgres:
//	  host: 127.0.0.1
//	  port: 5432
//	  user: postgres
//	  password: postgres
//	  dbname: interview
//	  sslmode: disable
//	  time_zone: Asia/Shanghai
//	  max_open_conns: 20
//	  max_idle_conns: 5
//	  conn_max_lifetime: 1h
//	  conn_max_idle_time: 30m
//	  log_level: warn          # silent / error / warn / info
//	  slow_threshold: 200ms
type Config struct {
	Host            string        `config:"host" default:"127.0.0.1"`
	Port            int           `config:"port" default:"5432"`
	User            string        `config:"user" default:"postgres"`
	Password        string        `config:"password" default:"postgres"`
	DBName          string        `config:"dbname" default:"postgres"`
	SSLMode         string        `config:"sslmode" default:"disable"`
	TimeZone        string        `config:"time_zone" default:"Asia/Shanghai"`
	MaxOpenConns    int           `config:"max_open_conns" default:"20"`
	MaxIdleConns    int           `config:"max_idle_conns" default:"5"`
	ConnMaxLifetime time.Duration `config:"conn_max_lifetime" default:"1h"`
	ConnMaxIdleTime time.Duration `config:"conn_max_idle_time" default:"30m"`
	// LogLevel 控制 gorm SQL 日志级别：silent / error / warn / info。
	LogLevel string `config:"log_level" default:"warn"`
	// SlowThreshold 为慢查询阈值，超过该时长的 SQL 会以告警级别记录。
	SlowThreshold time.Duration `config:"slow_threshold" default:"200ms"`
}

// LoadConfig 从配置管理器的 key 子树（默认 "postgres"）绑定数据库配置。
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

// DSN 生成 postgres 驱动连接串。
func (c Config) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s TimeZone=%s",
		c.Host, c.Port, c.User, c.Password, c.DBName, c.SSLMode, c.TimeZone,
	)
}

// Addr 返回 host:port 形式的地址（IPv6 自动加方括号）。
func (c Config) Addr() string {
	return net.JoinHostPort(c.Host, fmt.Sprintf("%d", c.Port))
}

// defaultConfig 返回全默认值的数据库配置。
func defaultConfig() Config {
	c := Config{}
	_ = config.New().Unmarshal(&c) // 经由 default 标签填充默认值，不会失败
	return c
}
