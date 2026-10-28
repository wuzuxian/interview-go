// 阶段机推进判据单测（票 05）：shouldTransitionToNextPhase 三规则与边界（对齐
// Java shouldTransitionToNextPhase L510-545）、durationFor 阶段取参（对齐
// getPhaseConfig L614-623）、四阶段默认值逐字对齐 Java application.yml phase.* 段、
// nextPhaseState 保持/顺延/收尾组合。
package voiceinterview

import (
	"testing"
	"time"

	"github.com/wuzuxian/interview/config"
)

// 四阶段判据默认值（config 库对零配置树做 Unmarshal，default 标签生效）逐字对齐
// Java application.yml app.voice-interview.phase.* 段：
// INTRO 3/5/8 分钟 2/5 题；TECH 8/10/15 分钟 3/8 题；PROJECT 8/10/15 分钟 2/5 题；
// HR 3/5/8 分钟 2/5 题。
func TestPhaseConfig默认值对齐Java(t *testing.T) {
	var cfg Config
	if err := config.New().Unmarshal(&cfg); err != nil {
		t.Fatalf("解码空配置树失败: %v", err)
	}
	cases := []struct {
		name string
		got  [5]int
		want [5]int // min/suggested/max 时长（分钟）+ min/max 题量
	}{
		{"INTRO", phaseArray(cfg.Phase.Intro.MinDuration, cfg.Phase.Intro.SuggestedDuration, cfg.Phase.Intro.MaxDuration, cfg.Phase.Intro.MinQuestions, cfg.Phase.Intro.MaxQuestions), [5]int{3, 5, 8, 2, 5}},
		{"TECH", phaseArray(cfg.Phase.Tech.MinDuration, cfg.Phase.Tech.SuggestedDuration, cfg.Phase.Tech.MaxDuration, cfg.Phase.Tech.MinQuestions, cfg.Phase.Tech.MaxQuestions), [5]int{8, 10, 15, 3, 8}},
		{"PROJECT", phaseArray(cfg.Phase.Project.MinDuration, cfg.Phase.Project.SuggestedDuration, cfg.Phase.Project.MaxDuration, cfg.Phase.Project.MinQuestions, cfg.Phase.Project.MaxQuestions), [5]int{8, 10, 15, 2, 5}},
		{"HR", phaseArray(cfg.Phase.HR.MinDuration, cfg.Phase.HR.SuggestedDuration, cfg.Phase.HR.MaxDuration, cfg.Phase.HR.MinQuestions, cfg.Phase.HR.MaxQuestions), [5]int{3, 5, 8, 2, 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("默认值应逐字对齐 Java（min/suggested/max 分钟 + min/max 题）: got %v want %v", tc.got, tc.want)
			}
		})
	}
}

func phaseArray(vs ...int) [5]int { return [5]int(vs) }

// durationFor 阶段取参（Java getPhaseConfig：COMPLETED/未知 → 零值判据）。
func TestDurationFor阶段取参(t *testing.T) {
	var cfg Config
	if err := config.New().Unmarshal(&cfg); err != nil {
		t.Fatalf("解码空配置树失败: %v", err)
	}

	intro := cfg.Phase.durationFor(PhaseIntro)
	if intro.MinDuration != 3 || intro.SuggestedDuration != 5 || intro.MaxDuration != 8 || intro.MinQuestions != 2 || intro.MaxQuestions != 5 {
		t.Errorf("INTRO 判据取参不符: %+v", intro)
	}
	tech := cfg.Phase.durationFor(PhaseTech)
	if tech.MinDuration != 8 || tech.MaxQuestions != 8 {
		t.Errorf("TECH 判据取参不符: %+v", tech)
	}
	zero := PhaseDuration{}
	for _, p := range []VoicePhase{PhaseCompleted, VoicePhase("BOGUS"), ""} {
		if got := cfg.Phase.durationFor(p); got != zero {
			t.Errorf("阶段 %q 应取零值判据: %+v", p, got)
		}
	}
}

// 推进判据三规则与边界（Java L527-545）：分钟数按 Duration.toMinutes 向零截断；
// 规则 1 max 时长强制；规则 2 max 题量；规则 3 suggested 时长 + min 题量。
func TestShouldTransitionToNextPhase判据(t *testing.T) {
	intro := PhaseDuration{MinDuration: 3, SuggestedDuration: 5, MaxDuration: 8, MinQuestions: 2, MaxQuestions: 5}

	cases := []struct {
		name      string
		phase     VoicePhase
		cfg       PhaseDuration
		questions int
		elapsed   time.Duration
		want      bool
	}{
		{"COMPLETED 恒不推进", PhaseCompleted, intro, 99, time.Hour, false},
		{"空阶段恒不推进", "", intro, 99, time.Hour, false},
		{"规则1时长达到max强制推进", PhaseIntro, intro, 0, 8 * time.Minute, true},
		{"规则1边界差一分钟不推进", PhaseIntro, intro, 0, 7*time.Minute + 59*time.Second, false},
		{"规则2题量达到max推进", PhaseIntro, intro, 5, time.Minute, true},
		{"规则2边界差一题不推进", PhaseIntro, intro, 4, time.Minute, false},
		{"规则3suggested时长加min题量推进", PhaseIntro, intro, 2, 5 * time.Minute, true},
		{"规则3时长足但题量不足保持", PhaseIntro, intro, 1, 5 * time.Minute, false},
		{"规则3题量足但时长不足保持", PhaseIntro, intro, 2, 4*time.Minute + 59*time.Second, false},
		{"零值判据任何时刻都推进", PhaseHR, PhaseDuration{}, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldTransitionToNextPhase(tc.phase, tc.cfg, tc.questions, tc.elapsed); got != tc.want {
				t.Errorf("shouldTransitionToNextPhase(%s, %+v, %d 题, %v) = %v, want %v",
					tc.phase, tc.cfg, tc.questions, tc.elapsed, got, tc.want)
			}
		})
	}
}

// nextPhaseState 组合：判据保持 → 原阶段；判据通过 → 按四阶段开关顺延；
// 尾段通过 → 收尾 COMPLETED；COMPLETED 恒保持。
func TestNextPhaseState保持顺延收尾(t *testing.T) {
	allOn := func() *SessionEntity {
		return &SessionEntity{
			CurrentPhase:   phasePtr(PhaseTech),
			IntroEnabled:   boolPtr(true),
			TechEnabled:    boolPtr(true),
			ProjectEnabled: boolPtr(true),
			HrEnabled:      boolPtr(true),
		}
	}
	cfg := PhaseConfig{}
	setDefaults := func(c *PhaseConfig) {
		c.Intro.MinDuration, c.Intro.SuggestedDuration, c.Intro.MaxDuration, c.Intro.MinQuestions, c.Intro.MaxQuestions = 3, 5, 8, 2, 5
		c.Tech.MinDuration, c.Tech.SuggestedDuration, c.Tech.MaxDuration, c.Tech.MinQuestions, c.Tech.MaxQuestions = 8, 10, 15, 3, 8
		c.Project.MinDuration, c.Project.SuggestedDuration, c.Project.MaxDuration, c.Project.MinQuestions, c.Project.MaxQuestions = 8, 10, 15, 2, 5
		c.HR.MinDuration, c.HR.SuggestedDuration, c.HR.MaxDuration, c.HR.MinQuestions, c.HR.MaxQuestions = 3, 5, 8, 2, 5
	}
	setDefaults(&cfg)

	cases := []struct {
		name      string
		entity    *SessionEntity
		questions int
		elapsed   time.Duration
		want      VoicePhase
	}{
		{"判据未满足保持 TECH", allOn(), 1, time.Minute, PhaseTech},
		{"题量达 max 顺延 PROJECT", allOn(), 8, time.Minute, PhaseProject},
		{"suggested 加 min 题量顺延", allOn(), 3, 10 * time.Minute, PhaseProject},
		{"尾段 HR 通过收尾 COMPLETED", func() *SessionEntity {
			e := allOn()
			e.CurrentPhase = phasePtr(PhaseHR)
			return e
		}(), 99, time.Hour, PhaseCompleted},
		{"开关关闭跳过被关阶段", func() *SessionEntity {
			e := allOn()
			e.CurrentPhase = phasePtr(PhaseIntro)
			e.TechEnabled = boolPtr(false)
			return e
		}(), 99, time.Hour, PhaseProject},
		{"COMPLETED 恒保持", func() *SessionEntity {
			e := allOn()
			e.CurrentPhase = phasePtr(PhaseCompleted)
			return e
		}(), 99, time.Hour, PhaseCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextPhaseState(tc.entity, cfg, tc.questions, tc.elapsed); got != tc.want {
				t.Errorf("nextPhaseState = %s, want %s", got, tc.want)
			}
		})
	}
}
