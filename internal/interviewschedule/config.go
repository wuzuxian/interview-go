// 面试日程模块配置（app.interview_schedule 段）。Java 侧该模块无 yml 配置键，
// 定时节奏为 ScheduleStatusUpdater 的 @Scheduled(cron = "0 0 * * * ?") 每小时整点；
// Go 侧按仓库惯例（voice 不活动巡检先例）收敛为可配间隔，默认 1h 行为等价。
package interviewschedule

import "time"

// Config 模块配置，default 标签值与 Java 行为对齐。
type Config struct {
	// SweeperInterval 过期待面试巡检间隔（Java @Scheduled cron 每小时整点）。
	SweeperInterval time.Duration `config:"sweeper_interval" default:"1h"`
}
