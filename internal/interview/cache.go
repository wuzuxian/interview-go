// SessionCache Redis 会话缓存：对齐 Java InterviewSessionCache 的键结构、
// TTL 与双写语义，外加创建锁/幂等结果键（对齐 Java RedisService.executeWithLock +
// CREATE_RESULT_* 常量）。锁为手写 SET NX PX + 比较令牌删除，不引入 redsync。
package interview

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/wuzuxian/interview/internal/apperr"
)

// Redis 键前缀与 TTL，对齐 Java InterviewSessionCache / InterviewSessionService 常量。
const (
	sessionKeyPrefix       = "interview:session:"
	resumeSessionKeyPrefix = "interview:resume:"
	createLockPrefix       = "interview:create:"
	createResultPrefix     = "interview:create:result:"
)

// CachedSession 缓存的会话数据，字段对齐 Java InterviewSessionCache.CachedSession。
type CachedSession struct {
	SessionID         string        `json:"sessionId"`
	ResumeText        string        `json:"resumeText"`
	ResumeID          *int64        `json:"resumeId"`
	KnowledgeBaseID   *int64        `json:"knowledgeBaseId"`
	InterviewCategory *string       `json:"interviewCategory"`
	QuestionsJSON     string        `json:"questionsJson"`
	CurrentIndex      int           `json:"currentIndex"`
	Status            SessionStatus `json:"status"`
}

// SessionCache 场次缓存与创建锁。
type SessionCache struct {
	rdb    redis.Cmdable
	logger *slog.Logger
	cfg    Config
}

// NewSessionCache 创建缓存（rdb 为 *redisx.Client 或任何 redis.Cmdable）。
func NewSessionCache(rdb redis.Cmdable, cfg Config, logger *slog.Logger) *SessionCache {
	return &SessionCache{rdb: rdb, cfg: cfg, logger: logger}
}

// SaveSession 写会话缓存；未完成状态且有 resumeId 时同步建立 resume→session 映射
// （对齐 Java saveSession）。
func (c *SessionCache) SaveSession(ctx context.Context, s CachedSession) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := c.rdb.Set(ctx, sessionKeyPrefix+s.SessionID, b, c.cfg.SessionCacheTTL).Err(); err != nil {
		return err
	}
	if s.ResumeID != nil && IsUnfinished(s.Status) {
		if err := c.SaveResumeMapping(ctx, *s.ResumeID, s.SessionID); err != nil {
			return err
		}
	}
	return nil
}

// GetSession 读会话缓存；未命中返回 (nil, nil)。
func (c *SessionCache) GetSession(ctx context.Context, sessionID string) (*CachedSession, error) {
	b, err := c.rdb.Get(ctx, sessionKeyPrefix+sessionID).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s := &CachedSession{}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	return s, nil
}

// RefreshTTL 读路径命中后刷新过期时间（对齐 Java refreshSessionTTL）。
func (c *SessionCache) RefreshTTL(ctx context.Context, sessionID string) error {
	return c.rdb.Expire(ctx, sessionKeyPrefix+sessionID, c.cfg.SessionCacheTTL).Err()
}

// SaveResumeMapping 建立 resumeId → sessionId 映射（用于查找未完成会话）。
func (c *SessionCache) SaveResumeMapping(ctx context.Context, resumeID int64, sessionID string) error {
	return c.rdb.Set(ctx, resumeSessionKey(resumeID), sessionID, c.cfg.SessionCacheTTL).Err()
}

// RemoveResumeMapping 删除 resumeId → sessionId 映射。
func (c *SessionCache) RemoveResumeMapping(ctx context.Context, resumeID int64) error {
	return c.rdb.Del(ctx, resumeSessionKey(resumeID)).Err()
}

// FindUnfinishedSessionID 从映射查未完成会话 ID；会话已不存在或已完成时清理映射
// （对齐 Java findUnfinishedSessionId）。
func (c *SessionCache) FindUnfinishedSessionID(ctx context.Context, resumeID int64) (*string, error) {
	sessionID, err := c.rdb.Get(ctx, resumeSessionKey(resumeID)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s, err := c.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if s != nil && IsUnfinished(s.Status) {
		return &sessionID, nil
	}
	if err := c.rdb.Del(ctx, resumeSessionKey(resumeID)).Err(); err != nil {
		return nil, err
	}
	return nil, nil
}

// GetCreateResult 读幂等结果键（requestId → sessionId）；未命中返回 ("" , nil)。
func (c *SessionCache) GetCreateResult(ctx context.Context, requestID string) (string, error) {
	v, err := c.rdb.Get(ctx, createResultPrefix+requestID).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

// SetCreateResult 写幂等结果键（TTL 对齐 Java CREATE_RESULT_TTL = 1 天）。
func (c *SessionCache) SetCreateResult(ctx context.Context, requestID, sessionID string) error {
	return c.rdb.Set(ctx, createResultPrefix+requestID, sessionID, c.cfg.CreateResultTTL).Err()
}

// lockPollInterval 获取锁失败的轮询间隔（Redisson tryLock 默认重试间隔 1.5s 过粗，
// 创建秒级完成，取 100ms 让并发重复请求尽快回到结果键读取）。
const lockPollInterval = 100 * time.Millisecond

// releaseLockScript 释放锁前校验令牌，避免租期过期后误删他人的锁。
var releaseLockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// WithLock 手写 SET NX PX 分布式锁：等待 waitTime 内轮询获取，获取失败返回
// INTERNAL_ERROR "获取锁失败: <key>"（对齐 Java RedisService.executeWithLock 语义）；
// 持有 leaseTime 后自动过期，操作正常返回时主动释放。
func (c *SessionCache) WithLock(ctx context.Context, key string, waitTime, leaseTime time.Duration, op func() error) error {
	token := uuid.NewString()
	deadline := time.Now().Add(waitTime)

	for {
		ok, err := c.rdb.SetNX(ctx, key, token, leaseTime).Result()
		if err != nil {
			// Redis 异常：与限流器一致的兜底——不因缓存故障完全阻断创建。
			if c.logger != nil {
				c.logger.Warn("创建锁 Redis 访问失败，直接执行操作", slog.String("key", key), slog.String("error", err.Error()))
			}
			return op()
		}
		if ok {
			defer releaseLockScript.Run(ctx, c.rdb, []string{key}, token)
			return op()
		}
		if time.Now().After(deadline) {
			return apperr.Internal("获取锁失败: " + key)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}

func resumeSessionKey(resumeID int64) string {
	return resumeSessionKeyPrefix + strconv.FormatInt(resumeID, 10)
}
