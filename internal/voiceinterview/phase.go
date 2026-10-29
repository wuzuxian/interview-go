// 阶段机推进判据（票 05）：INTRO→TECH→PROJECT→HR→COMPLETED 的转移判据以纯函数
// 暴露，供票 06 的回合管道在生成下一问前消费。判据与 Java
// VoiceInterviewService.shouldTransitionToNextPhase（L510-545）/ getPhaseConfig
// （L614-623）逐字对齐；阶段顺延（getNextPhase，含四阶段开关）已存在于 service.go。
//
// 注意：Java 侧 shouldTransitionToNextPhase 为公开判据方法，阶段内计时起点由调用方
// 传入（phaseStartTime）；本实现同样只收"阶段内耗时"，不持有计时状态。
package voiceinterview

import "time"

// PhaseDuration 单阶段推进判据五参（Java VoiceInterviewProperties.DurationConfig）：
// 时长三档单位为分钟，题量两档为该阶段内已问轮数。
type PhaseDuration struct {
	MinDuration       int
	SuggestedDuration int
	MaxDuration       int
	MinQuestions      int
	MaxQuestions      int
}

// PhaseConfig 四阶段推进判据（Java VoiceInterviewProperties.PhaseConfig）。
// 四个阶段各自以内联结构体声明默认值标签：Java 侧差异默认值由 PhaseConfig 字段
// 构造器（new DurationConfig(3,5,8,2,5) 等）表达，Go 侧标签机制即构造器，
// 默认值逐字取 Java application.yml app.voice-interview.phase.* 段原文
// （INTRO 3/5/8 分钟 2/5 题；TECH 8/10/15 分钟 3/8 题；
// PROJECT 8/10/15 分钟 2/5 题；HR 3/5/8 分钟 2/5 题）。
type PhaseConfig struct {
	Intro struct {
		MinDuration       int `config:"min_duration" default:"3"`
		SuggestedDuration int `config:"suggested_duration" default:"5"`
		MaxDuration       int `config:"max_duration" default:"8"`
		MinQuestions      int `config:"min_questions" default:"2"`
		MaxQuestions      int `config:"max_questions" default:"5"`
	} `config:"intro"`
	Tech struct {
		MinDuration       int `config:"min_duration" default:"8"`
		SuggestedDuration int `config:"suggested_duration" default:"10"`
		MaxDuration       int `config:"max_duration" default:"15"`
		MinQuestions      int `config:"min_questions" default:"3"`
		MaxQuestions      int `config:"max_questions" default:"8"`
	} `config:"tech"`
	Project struct {
		MinDuration       int `config:"min_duration" default:"8"`
		SuggestedDuration int `config:"suggested_duration" default:"10"`
		MaxDuration       int `config:"max_duration" default:"15"`
		MinQuestions      int `config:"min_questions" default:"2"`
		MaxQuestions      int `config:"max_questions" default:"5"`
	} `config:"project"`
	HR struct {
		MinDuration       int `config:"min_duration" default:"3"`
		SuggestedDuration int `config:"suggested_duration" default:"5"`
		MaxDuration       int `config:"max_duration" default:"8"`
		MinQuestions      int `config:"min_questions" default:"2"`
		MaxQuestions      int `config:"max_questions" default:"5"`
	} `config:"hr"`
}

// durationFor 取指定阶段的判据（Java getPhaseConfig L614-623：COMPLETED 与未知
// 阶段返回零值判据 DurationConfig(0,0,0,0,0)）。
func (c PhaseConfig) durationFor(phase VoicePhase) PhaseDuration {
	switch phase {
	case PhaseIntro:
		return PhaseDuration{c.Intro.MinDuration, c.Intro.SuggestedDuration, c.Intro.MaxDuration, c.Intro.MinQuestions, c.Intro.MaxQuestions}
	case PhaseTech:
		return PhaseDuration{c.Tech.MinDuration, c.Tech.SuggestedDuration, c.Tech.MaxDuration, c.Tech.MinQuestions, c.Tech.MaxQuestions}
	case PhaseProject:
		return PhaseDuration{c.Project.MinDuration, c.Project.SuggestedDuration, c.Project.MaxDuration, c.Project.MinQuestions, c.Project.MaxQuestions}
	case PhaseHR:
		return PhaseDuration{c.HR.MinDuration, c.HR.SuggestedDuration, c.HR.MaxDuration, c.HR.MinQuestions, c.HR.MaxQuestions}
	default:
		return PhaseDuration{}
	}
}

// shouldTransitionToNextPhase 阶段推进判据（Java shouldTransitionToNextPhase
// L510-545 逐字）：当前阶段为空或 COMPLETED 恒不推进；否则按三条规则——
// 规则 1 阶段内时长（分钟截断）≥ max-duration 强制推进；规则 2 已问轮数 ≥
// max-questions 推进；规则 3 时长 ≥ suggested-duration 且轮数 ≥ min-questions
// 推进；其余保持。elapsed 为阶段内耗时（Java phaseStartTime → now）。
func shouldTransitionToNextPhase(phase VoicePhase, c PhaseDuration, questionCount int, elapsed time.Duration) bool {
	if phase == "" || phase == PhaseCompleted {
		return false
	}
	minutes := int(elapsed.Minutes()) // Java Duration.toMinutes() 向零截断
	if minutes >= c.MaxDuration {
		return true
	}
	if questionCount >= c.MaxQuestions {
		return true
	}
	if minutes >= c.SuggestedDuration && questionCount >= c.MinQuestions {
		return true
	}
	return false
}

// nextPhaseState 判据消费入口（票 06 回合管道生成下一问前调用）：判据不满足返回
// 当前阶段（保持）；满足则按四阶段开关顺延（getNextPhase：无更多启用阶段时收尾
// COMPLETED）。输入为会话实体（当前阶段 + 四阶段开关）、阶段判据配置、该阶段内
// 已问轮数与阶段内耗时。
func nextPhaseState(e *SessionEntity, cfg PhaseConfig, questionCount int, elapsed time.Duration) VoicePhase {
	current := VoicePhase(phaseValue(e.CurrentPhase))
	if !shouldTransitionToNextPhase(current, cfg.durationFor(current), questionCount, elapsed) {
		return current
	}
	return getNextPhase(e)
}
