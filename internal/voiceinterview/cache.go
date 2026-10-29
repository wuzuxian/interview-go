// SessionCache Redis 会话缓存：键 voice:interview:session:{id}、TTL 1h、
// 读命中 / 写后更新 / 状态变更后失效，全部对齐 Java VoiceInterviewService 的
// cacheSession / invalidateSessionCache / getSession（L59-60, L172-189, L695-710）。
// Java 用 Redisson RBucket 缓存整个实体（Jackson JSON）；Go 缓存 CachedSession
// 的 JSON，字段名沿用 Java 实体属性名。会话实体的读路径不回填缓存（对齐 Java
// getSession：缓存未命中仅回源 DB），重建只发生在创建/恢复/阶段切换之后。
package voiceinterview

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// sessionCacheKeyPrefix 与 Java SESSION_CACHE_KEY_PREFIX 逐字一致。
const sessionCacheKeyPrefix = "voice:interview:session:"

// sessionCacheKey 生成缓存键（纯函数，单测锁死格式）。
func sessionCacheKey(sessionID int64) string {
	return sessionCacheKeyPrefix + strconv.FormatInt(sessionID, 10)
}

// SessionCacheStore 会话缓存的消费方接缝（spec 决策 19 的测试接缝清单外，
// 沿 internal/interview 的 EvalStore 做法：接口在模块内，生产实现为本文件
// 的 *SessionCache，测试注入内存替身）。
type SessionCacheStore interface {
	// Get 读缓存；未命中返回 (nil, nil)。
	Get(ctx context.Context, sessionID int64) (*CachedSession, error)
	// Save 整体写入并刷新 TTL（对齐 Java bucket.set(session, 1h)）。
	Save(ctx context.Context, s CachedSession) error
	// Invalidate 删除缓存（对齐 Java bucket.delete()）。
	Invalidate(ctx context.Context, sessionID int64) error
}

// CachedSession 缓存的会话实体快照，字段与 Java VoiceInterviewSessionEntity 一致
// （JSON 键 = Java 属性名，Redisson 序列化形状）。
type CachedSession struct {
	ID              int64                `json:"id"`
	UserID          *string              `json:"userId"`
	RoleType        string               `json:"roleType"`
	SkillID         *string              `json:"skillId"`
	Difficulty      *string              `json:"difficulty"`
	CustomJdText    *string              `json:"customJdText"`
	ResumeID        *int64               `json:"resumeId"`
	IntroEnabled    *bool                `json:"introEnabled"`
	TechEnabled     *bool                `json:"techEnabled"`
	ProjectEnabled  *bool                `json:"projectEnabled"`
	HrEnabled       *bool                `json:"hrEnabled"`
	LLMProvider     *string              `json:"llmProvider"`
	CurrentPhase    *VoicePhase          `json:"currentPhase"`
	Status          *VoiceSessionStatus  `json:"status"`
	PlannedDuration *int                 `json:"plannedDuration"`
	ActualDuration  *int                 `json:"actualDuration"`
	StartTime       *time.Time           `json:"startTime"`
	EndTime         *time.Time           `json:"endTime"`
	CreatedAt       *time.Time           `json:"createdAt"`
	UpdatedAt       *time.Time           `json:"updatedAt"`
	PausedAt        *time.Time           `json:"pausedAt"`
	ResumedAt       *time.Time           `json:"resumedAt"`
	EvaluateStatus  *VoiceEvaluateStatus `json:"evaluateStatus"`
	EvaluateError   *string              `json:"evaluateError"`
}

// entityToCached 落库实体 → 缓存快照。
func entityToCached(e *SessionEntity) CachedSession {
	return CachedSession{
		ID: e.ID, UserID: e.UserID, RoleType: e.RoleType, SkillID: e.SkillID,
		Difficulty: e.Difficulty, CustomJdText: e.CustomJdText, ResumeID: e.ResumeID,
		IntroEnabled: e.IntroEnabled, TechEnabled: e.TechEnabled,
		ProjectEnabled: e.ProjectEnabled, HrEnabled: e.HrEnabled,
		LLMProvider: e.LLMProvider, CurrentPhase: e.CurrentPhase, Status: e.Status,
		PlannedDuration: e.PlannedDuration, ActualDuration: e.ActualDuration,
		StartTime: e.StartTime, EndTime: e.EndTime, CreatedAt: e.CreatedAt,
		UpdatedAt: e.UpdatedAt, PausedAt: e.PausedAt, ResumedAt: e.ResumedAt,
		EvaluateStatus: e.EvaluateStatus, EvaluateError: e.EvaluateError,
	}
}

// cachedToEntity 缓存快照 → 实体（读路径响应构造与实体同形）。
func cachedToEntity(c *CachedSession) *SessionEntity {
	return &SessionEntity{
		ID: c.ID, UserID: c.UserID, RoleType: c.RoleType, SkillID: c.SkillID,
		Difficulty: c.Difficulty, CustomJdText: c.CustomJdText, ResumeID: c.ResumeID,
		IntroEnabled: c.IntroEnabled, TechEnabled: c.TechEnabled,
		ProjectEnabled: c.ProjectEnabled, HrEnabled: c.HrEnabled,
		LLMProvider: c.LLMProvider, CurrentPhase: c.CurrentPhase, Status: c.Status,
		PlannedDuration: c.PlannedDuration, ActualDuration: c.ActualDuration,
		StartTime: c.StartTime, EndTime: c.EndTime, CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt, PausedAt: c.PausedAt, ResumedAt: c.ResumedAt,
		EvaluateStatus: c.EvaluateStatus, EvaluateError: c.EvaluateError,
	}
}

// SessionCache Redis 会话缓存生产实现（rdb 为 *redisx.Client 内嵌的原生客户端
// 或任何 redis.Cmdable）。TTL 取 app.voice_interview.session_cache_ttl（默认 1h，
// 对齐 Java CACHE_TTL_HOURS）。
type SessionCache struct {
	rdb    redis.Cmdable
	ttl    time.Duration
	logger *slog.Logger
}

// NewSessionCache 创建缓存；TTL 非正时回落 Java 默认 1h。
func NewSessionCache(rdb redis.Cmdable, cfg Config, logger *slog.Logger) *SessionCache {
	ttl := cfg.SessionCacheTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &SessionCache{rdb: rdb, ttl: ttl, logger: logger}
}

// Get 读缓存；未命中返回 (nil, nil)。
func (c *SessionCache) Get(ctx context.Context, sessionID int64) (*CachedSession, error) {
	b, err := c.rdb.Get(ctx, sessionCacheKey(sessionID)).Bytes()
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

// Save 整体写入并刷新 TTL。
func (c *SessionCache) Save(ctx context.Context, s CachedSession) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, sessionCacheKey(s.ID), b, c.ttl).Err()
}

// Invalidate 删除缓存。
func (c *SessionCache) Invalidate(ctx context.Context, sessionID int64) error {
	return c.rdb.Del(ctx, sessionCacheKey(sessionID)).Err()
}
