// 知识库向量化与出题两条 Stream 链路的共享骨架（code-review 去重，行为契约不变）：
// Redis 消费组适配器（key/group 参数化；两流接口同形——QuestionGenMessage 为
// VectorizeMessage 别名，单一实现同时满足）、消费循环（Start/Stop/loop/
// consumeOnce/ack，日志文案与消息处理函数参数化）与恢复调度（Start/Stop，
// 巡检间隔与每轮动作参数化）。
package knowledgebase

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ===== Redis Stream 适配器 =====

// redisStreamAdapter *redis.Client 消费组适配器；XADD 按 MAXLEN 1000 近似裁剪
// （Java RedisService.streamAdd trimNonStrict）。
type redisStreamAdapter struct {
	rdb       *redis.Client
	streamKey string
	groupName string
}

// newRedisStreamAdapter 创建共用 Stream 适配器。
func newRedisStreamAdapter(rdb *redis.Client, streamKey, groupName string) *redisStreamAdapter {
	return &redisStreamAdapter{rdb: rdb, streamKey: streamKey, groupName: groupName}
}

func (s *redisStreamAdapter) EnsureGroup(ctx context.Context) error {
	err := s.rdb.XGroupCreateMkStream(ctx, s.streamKey, s.groupName, "0").Err()
	if err != nil && !isBusyGroup(err) {
		return err
	}
	return nil
}

func isBusyGroup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "BUSYGROUP")
}

func (s *redisStreamAdapter) XAddTask(ctx context.Context, values map[string]any) (string, error) {
	return s.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: s.streamKey,
		MaxLen: StreamMaxLen,
		Approx: true,
		Values: values,
	}).Result()
}

func (s *redisStreamAdapter) XAck(ctx context.Context, messageID string) error {
	return s.rdb.XAck(ctx, s.streamKey, s.groupName, messageID).Err()
}

func (s *redisStreamAdapter) XReadNew(ctx context.Context, consumer string, count int64) ([]VectorizeMessage, error) {
	res, err := s.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    s.groupName,
		Consumer: consumer,
		Streams:  []string{s.streamKey, ">"},
		Count:    count,
		Block:    500 * time.Millisecond,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	msgs := []VectorizeMessage{}
	for _, stream := range res {
		for _, msg := range stream.Messages {
			msgs = append(msgs, VectorizeMessage{ID: msg.ID, Values: msg.Values})
		}
	}
	return msgs, nil
}

func (s *redisStreamAdapter) XClaimPending(ctx context.Context, consumer string, count int64) ([]VectorizeMessage, error) {
	claimed, _, err := s.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   s.streamKey,
		Group:    s.groupName,
		Consumer: consumer,
		MinIdle:  PendingIdleTime,
		Start:    "0-0",
		Count:    count,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	msgs := make([]VectorizeMessage, 0, len(claimed))
	for _, msg := range claimed {
		msgs = append(msgs, VectorizeMessage{ID: msg.ID, Values: msg.Values})
	}
	return msgs, nil
}

// ===== 消费循环骨架 =====

// streamLoop Stream 消费循环骨架（向量化/出题两消费者共用）：一轮新消息 →
// pending 认领（宕机恢复语义），启停与 ACK；日志文案前缀 label 与消息处理
// handle 参数化。消费方经嵌入复用本类型字段（stream/logger/consumerName）。
type streamLoop struct {
	stream VectorizeStream
	logger *slog.Logger
	// label 日志固定文案中的流类型（「向量化」/「出题」）。
	label string
	// claimedLog 回收 pending 消息的固定文案（两流历史文案逐字保留）。
	claimedLog string
	// handle 单条消息处理（各自消费者的消费骨架入口）。
	handle func(ctx context.Context, msg VectorizeMessage)

	consumerName string
	stopCh       chan struct{}
	stopOnce     sync.Once
	doneCh       chan struct{}
}

// newStreamLoop 创建消费循环骨架（consumerName 构造由调用方注入）。
func newStreamLoop(stream VectorizeStream, logger *slog.Logger, label, claimedLog, consumerName string,
	handle func(ctx context.Context, msg VectorizeMessage)) *streamLoop {
	return &streamLoop{
		stream: stream, logger: logger, label: label, claimedLog: claimedLog,
		handle: handle, consumerName: consumerName,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
}

// Start 启动消费循环（后台 goroutine）。
func (c *streamLoop) Start(ctx context.Context) {
	go func() {
		defer close(c.doneCh)
		if err := c.stream.EnsureGroup(ctx); err != nil {
			c.logger.Warn("创建知识库"+c.label+" Stream Group 失败", slog.String("error", err.Error()))
		}
		c.logger.Info("知识库"+c.label+"消费者已启动", slog.String("consumer", c.consumerName))
		c.loop(ctx)
		c.logger.Info("知识库"+c.label+"消费者已停止", slog.String("consumer", c.consumerName))
	}()
}

// Stop 停止消费循环（等待退出；sync.Once 防二次 close panic）。
func (c *streamLoop) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	select {
	case <-c.doneCh:
	case <-time.After(5 * time.Second):
	}
}

func (c *streamLoop) loop(ctx context.Context) {
	for {
		select {
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}
		c.consumeOnce(ctx)
		time.Sleep(PollInterval)
	}
}

// consumeOnce 一轮消费：新消息 → pending 认领（宕机恢复语义）。
func (c *streamLoop) consumeOnce(ctx context.Context) {
	msgs, err := c.stream.XReadNew(ctx, c.consumerName, ConsumeBatch)
	if err != nil {
		c.logger.Error("消费知识库"+c.label+"消息失败", slog.String("error", err.Error()))
	} else if len(msgs) > 0 {
		for _, msg := range msgs {
			c.handle(ctx, msg)
		}
		return
	}
	claimed, err := c.stream.XClaimPending(ctx, c.consumerName, PendingClaimSize)
	if err != nil {
		c.logger.Error("认领知识库"+c.label+"pending 消息失败", slog.String("error", err.Error()))
		return
	}
	for _, msg := range claimed {
		c.logger.Info(c.claimedLog, slog.String("messageId", msg.ID))
		c.handle(ctx, msg)
	}
}

// ack Stream 消息确认。
func (c *streamLoop) ack(ctx context.Context, messageID string) {
	if err := c.stream.XAck(ctx, messageID); err != nil {
		c.logger.Error("ACK 失败", slog.String("messageId", messageID), slog.String("error", err.Error()))
	}
}

// ===== 恢复调度骨架 =====

// recoveryLoop 恢复调度骨架（向量化/出题两个恢复调度器共用）：定时巡检 + 停止
// 等待；巡检间隔与每轮动作 round 参数化（Java fixedDelay = initialDelay 语义）。
type recoveryLoop struct {
	logger   *slog.Logger
	interval time.Duration
	round    func(ctx context.Context)

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}
}

// newRecoveryLoop 创建恢复调度骨架。
func newRecoveryLoop(logger *slog.Logger, interval time.Duration, round func(ctx context.Context)) *recoveryLoop {
	return &recoveryLoop{logger: logger, interval: interval, round: round,
		stopCh: make(chan struct{}), doneCh: make(chan struct{})}
}

// Start 启动调度（初始延迟 = 巡检间隔）。
func (s *recoveryLoop) Start(ctx context.Context) {
	go func() {
		defer close(s.doneCh)
		timer := time.NewTimer(s.interval)
		defer timer.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-ctx.Done():
				return
			case <-timer.C:
				s.round(ctx)
				timer.Reset(s.interval)
			}
		}
	}()
}

// Stop 停止调度（等待退出；sync.Once 防二次 close panic）。
func (s *recoveryLoop) Stop() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	select {
	case <-s.doneCh:
	case <-time.After(3 * time.Second):
	}
}
