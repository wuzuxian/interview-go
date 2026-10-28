// 语音评估 Redis Stream 入队（票 01 提供生产者侧）：常量与消息字段对齐 Java
// AsyncTaskStreamConstants（L18 retryCount / L123 voice:evaluate:stream /
// L138 voiceSessionId / L50 STREAM_MAX_LEN=1000）与
// VoiceEvaluateStreamProducer.buildMessage。本票只做入队：end 端点在事务提交后
// XADD，无消费者时消息静默堆积（MAXLEN 1000 环形裁剪）；消费侧属票 08。
package voiceinterview

import (
	"context"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// 评估流常量，与 Java 版 AsyncTaskStreamConstants 逐字对齐。
const (
	VoiceEvalStreamKey       = "voice:evaluate:stream"
	VoiceEvalFieldSessionID  = "voiceSessionId"
	VoiceEvalFieldRetryCount = "retryCount"
	VoiceEvalStreamMaxLen    = 1000
)

// VoiceEvalStream 评估流入队接缝（沿 internal/interview 的 EvalStreamClient 做法：
// 接口在模块内，生产实现为本文件适配器，测试注入内存替身）。
type VoiceEvalStream interface {
	XAddTask(ctx context.Context, values map[string]any) (string, error)
}

// buildEvaluateTaskMessage 入队消息体（纯函数，字段表逐字对齐 Java buildMessage）：
// {"voiceSessionId": "<id>", "retryCount": "0"}，值均为字符串。
func buildEvaluateTaskMessage(sessionID int64) map[string]any {
	return map[string]any{
		VoiceEvalFieldSessionID:  strconv.FormatInt(sessionID, 10),
		VoiceEvalFieldRetryCount: "0",
	}
}

// redisEvalStream *redis.Client 适配器，XADD 时按 MAXLEN 1000 近似裁剪
// （对齐 Java RedisService.streamAdd(stream, message, STREAM_MAX_LEN)）。
type redisEvalStream struct {
	rdb *redis.Client
}

// NewRedisEvalStream 创建 Stream 适配器（实现 VoiceEvalStreamClient 全集：
// 票 01 的 XAddTask 入队 + 票 08 的消费组读写，见 evaluation_consumer.go）。
func NewRedisEvalStream(rdb *redis.Client) VoiceEvalStreamClient {
	return &redisEvalStream{rdb: rdb}
}

func (s *redisEvalStream) XAddTask(ctx context.Context, values map[string]any) (string, error) {
	return s.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: VoiceEvalStreamKey,
		MaxLen: VoiceEvalStreamMaxLen,
		Values: values,
	}).Result()
}
