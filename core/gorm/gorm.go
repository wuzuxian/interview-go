package gormdb

import (
	"context"
	"fmt"
	"time"

	"github.com/pgvector/pgvector-go"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/wuzuxian/interview/config"
)

// DB 是封装后的数据库句柄，内嵌 *gorm.DB，
// 可直接使用 db.Where(...).First(...) 等 gorm 全部能力，并附加 Close/Ping 等生命周期方法。
type DB struct {
	*gorm.DB
	cfg         Config
	dsnOverride string
}

// New 创建数据库连接（默认验证连通性）。使用 WithConfig 时从配置管理器绑定，
// WithDSN / WithHost 等显式选项优先级高于配置值，与书写顺序无关。
func New(opts ...Option) (*DB, error) {
	o := &options{key: defaultConfigKey}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}

	d := &DB{cfg: defaultConfig(), dsnOverride: o.dsn}

	// 配置管理器绑定（显式 DSN 时跳过）。
	if o.config != nil && o.dsn == "" {
		c, err := LoadConfig(o.config, o.key)
		if err != nil {
			return nil, fmt.Errorf("gormdb: 绑定数据库配置 %q 失败: %w", o.key, err)
		}
		d.cfg = c
	}
	applyOverrides(&d.cfg, o.override, o.set)

	// slog 适配 gorm 日志。
	var gl gormlogger.Interface
	if o.gormLogger != nil {
		gl = o.gormLogger
	} else {
		gl = newSlogLogger(o.logger, d.cfg.LogLevel, d.cfg.SlowThreshold)
	}

	gdb, err := gorm.Open(postgres.New(postgres.Config{DSN: d.dsn()}), &gorm.Config{
		Logger:                                   gl,
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return nil, fmt.Errorf("gormdb: 连接数据库 %s 失败: %w", d.cfg.Addr(), err)
	}
	d.DB = gdb

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("gormdb: 获取底层连接池失败: %w", err)
	}
	sqlDB.SetMaxOpenConns(d.cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(d.cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(d.cfg.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(d.cfg.ConnMaxIdleTime)

	if !o.skipPing {
		if err := d.Ping(context.Background()); err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("gormdb: 数据库 %s 连接验证失败: %w", d.cfg.Addr(), err)
		}
	}
	return d, nil
}

// NewWithConfig 从配置管理器创建连接的快捷方式，key 为空时使用 "postgres"。
func NewWithConfig(cfg *config.Config, key string, opts ...Option) (*DB, error) {
	return New(append(opts, WithConfig(cfg), WithConfigKey(key))...)
}

// dsn 返回实际使用的连接串：显式 DSN 优先，否则由配置生成。
func (d *DB) dsn() string {
	if d.dsnOverride != "" {
		return d.dsnOverride
	}
	return d.cfg.DSN()
}

// Config 返回当前生效的数据库配置。
func (d *DB) Config() Config {
	return d.cfg
}

// Ping 验证数据库连通性，可用于健康检查端点。
func (d *DB) Ping(ctx context.Context) error {
	sqlDB, err := d.DB.DB()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return sqlDB.PingContext(ctx)
}

// Close 关闭底层连接池。
func (d *DB) Close() error {
	sqlDB, err := d.DB.DB()
	if err != nil {
		return err
	}
	if err := sqlDB.Close(); err != nil {
		return fmt.Errorf("gormdb: 关闭连接池失败: %w", err)
	}
	return nil
}

// EnableVectorExtension 执行 CREATE EXTENSION IF NOT EXISTS vector，
// 启用 pgvector 向量扩展。需要数据库安装了 pgvector 且账号具备相应权限，
// 通常在应用启动迁移阶段调用一次。
func (d *DB) EnableVectorExtension(ctx context.Context) error {
	if err := d.DB.WithContext(ctx).Exec("CREATE EXTENSION IF NOT EXISTS vector").Error; err != nil {
		return fmt.Errorf("gormdb: 启用 pgvector 扩展失败: %w", err)
	}
	return nil
}

// Vector 是 pgvector 向量类型的别名，模型字段直接使用：
//
//	type Doc struct {
//	    ID        int64
//	    Content   string
//	    Embedding gormdb.Vector `gorm:"type:vector(1536)"`
//	}
type Vector = pgvector.Vector

// NewVector 从 float32 切片创建向量值。
// 向量检索的常用写法（向量字面量由 float 切片生成，可直接拼接）：
//
//	q := gormdb.NewVector(query).String()
//	db.Order(fmt.Sprintf("embedding <=> '%s'::vector", q)). // 余弦相似度
//	    Limit(5).Find(&docs)                                // 或 <-> 为 L2 距离
func NewVector(v []float32) Vector {
	return pgvector.NewVector(v)
}
