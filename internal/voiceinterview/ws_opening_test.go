// 开场白管道测试（票 05，封闭式管道例外复用假 TTS 服务端）：建连自动开场的下发
// 顺序（text(final=true) 先于 audio）、先落库再推送、有历史不重复开场、同方向
// 缓存命中不重复合成、无匹配方向走默认文案。契约来源：Java Handler
// triggerOpeningQuestionIfNeeded L218-261 / buildOpeningQuestion L283-307 /
// getOpeningWavAudio L263-281。
package voiceinterview

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// openingMsgEvent 下行事件顺序断言用的最小记录。
type openingMsgEvent struct {
	typ string // text / audio / control / subtitle / error
	m   map[string]any
}

// awaitOpening 消费下行直至同时收到 text 与 audio（容忍 welcome/asr_ready/subtitle
// 交错），返回事件序列。
func awaitOpening(t *testing.T, c *wsClient) []openingMsgEvent {
	t.Helper()
	var evs []openingMsgEvent
	hasText, hasAudio := false, false
	deadline := time.After(5 * time.Second)
	for !hasText || !hasAudio {
		select {
		case m := <-c.ch:
			typ, _ := m["type"].(string)
			evs = append(evs, openingMsgEvent{typ, m})
			switch typ {
			case "text":
				hasText = true
			case "audio":
				hasAudio = true
			}
		case <-c.closed:
			t.Fatalf("连接提前关闭，已收到: %v", evs)
		case <-deadline:
			t.Fatalf("超时未收齐开场 text+audio，已收到: %v", evs)
		}
	}
	return evs
}

// 开场白端到端主链路：无历史建连自动收到开场——text(final=true) 先于音频、
// 开场消息先落库（DIALOGUE、会话当前阶段、序号 1、用户侧为空）再推送、
// 音频为合成 PCM 包 44 字节 WAV 头的 base64。
func TestWSOpening无历史自动开场文本先于音频且先落库(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	e := inProgressSession(time.Hour) // 当前阶段 TECH
	e.SkillID = stringPtr("java-backend")
	id := gf.f.store.addSession(e)
	c := gf.mustWSClient(t, id)

	want := buildOpeningQuestion("java-backend", embeddedOpeningConfig())
	evs := awaitOpening(t, c)

	textIdx, audioIdx := -1, -1
	for i, ev := range evs {
		switch ev.typ {
		case "text":
			if textIdx >= 0 {
				t.Fatalf("开场 text 不应重复: %v", evs)
			}
			textIdx = i
			if ev.m["content"] != want {
				t.Errorf("开场文案应为 java-backend 方向原文: %q", ev.m["content"])
			}
			if ev.m["final"] != true {
				t.Errorf("开场 text 应为 final=true: %v", ev.m["final"])
			}
			// 先落库再推前端（Java L246 注释原意）：text 到达时 DB 已有开场行。
			rows := gf.f.store.dialogueRows(id)
			if len(rows) != 1 {
				t.Fatalf("text 推送前应已有 1 条开场消息，实际 %d 条", len(rows))
			}
			row := rows[0]
			if row.MessageType != MessageDialogue {
				t.Errorf("开场消息类型应为 DIALOGUE: %s", row.MessageType)
			}
			if phaseValue(row.Phase) != "TECH" {
				t.Errorf("开场消息阶段应取会话当前阶段 TECH: %v", row.Phase)
			}
			if row.SequenceNum == nil || *row.SequenceNum != 1 {
				t.Errorf("开场消息序号应为 1: %v", row.SequenceNum)
			}
			if row.UserRecognizedText != nil {
				t.Errorf("开场消息用户侧应为空: %v", row.UserRecognizedText)
			}
			// Java 落库走 trimToNull（L286），下发为原文（含折叠标量收尾换行）——
			// 库内文案为 trim 版。
			if row.AiGeneratedText == nil || *row.AiGeneratedText != strings.TrimSpace(want) {
				t.Errorf("开场消息 AI 侧应为 trim 后的开场文案: %v", row.AiGeneratedText)
			}
		case "audio":
			if audioIdx >= 0 {
				t.Fatalf("开场 audio 不应重复: %v", evs)
			}
			audioIdx = i
			if ev.m["text"] != want {
				t.Errorf("开场 audio.text 应为开场文案: %q", ev.m["text"])
			}
			data, _ := ev.m["data"].(string)
			wav, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				t.Fatalf("audio.data 应为 base64: %v", err)
			}
			if want2 := PcmToWav([]byte{0x01, 0x02}); string(wav) != string(want2) {
				t.Errorf("开场音频应为合成 PCM 包 44 字节 WAV 头:\n got %x\nwant %x", wav, want2)
			}
		}
	}
	if textIdx == -1 || audioIdx == -1 {
		t.Fatalf("开场 text+audio 未收齐: %v", evs)
	}
	if textIdx > audioIdx {
		t.Errorf("text(final) 应先于音频下发: %v", evs)
	}

	// TTS 服务端恰好合成一次，文本即开场文案。
	waitForCond(t, 3*time.Second, "假 TTS 收到开场文本", func() bool {
		return gf.ttsSrv.AppendedText() == want
	})
	if n := gf.ttsSrv.ConnectionCount(); n != 1 {
		t.Errorf("首次开场应恰好一次 TTS 合成连接，实际 %d", n)
	}
}

// 已有历史对话（重连/恢复场景）建连不重复开场（Java L232-235）：无 text/audio
// 下发、无新消息落库、无 TTS 调用。
func TestWSOpening有历史不重复开场(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	e := inProgressSession(time.Hour)
	e.SkillID = stringPtr("java-backend")
	id := gf.f.store.addSession(e)
	gf.f.store.setDialogues(id, []MessageEntity{voiceMsg(id, 1, stringPtr("旧问题"), stringPtr("旧回答"))})

	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)
	c.assertSilent(t, 500*time.Millisecond)

	if rows := gf.f.store.dialogueRows(id); len(rows) != 1 {
		t.Errorf("不应新增落库消息: %d 条", len(rows))
	}
	if n := gf.ttsSrv.ConnectionCount(); n != 0 {
		t.Errorf("不应触发 TTS 合成: %d 次", n)
	}
}

// 开场音频按文案（方向唯一决定）缓存：同方向第二个无历史会话建连不重复合成
// （Java openingAudioCache L263-281）。
func TestWSOpening同方向缓存命中不重复合成(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	mk := func() int64 {
		e := inProgressSession(time.Hour)
		e.SkillID = stringPtr("java-backend")
		return gf.f.store.addSession(e)
	}
	id1, id2 := mk(), mk()

	c1 := gf.mustWSClient(t, id1)
	awaitOpening(t, c1)

	c2 := gf.mustWSClient(t, id2)
	awaitOpening(t, c2)

	if n := gf.ttsSrv.ConnectionCount(); n != 1 {
		t.Errorf("同方向第二个会话应命中缓存，TTS 合成连接应仍为 1 次，实际 %d", n)
	}
	waitForCond(t, 3*time.Second, "缓存回填生效", func() bool {
		return len(gf.gw.openingCache.get(buildOpeningQuestion("java-backend", embeddedOpeningConfig()))) > 0
	})
}

// 方向不在 skill-questions 且不在 algorithm-skills → 走后端默认题原文
// （Java buildOpeningQuestion L303-307）。
func TestWSOpening无匹配方向走默认后端文案(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	e := inProgressSession(time.Hour)
	e.SkillID = stringPtr("ruby-backend")
	id := gf.f.store.addSession(e)
	c := gf.mustWSClient(t, id)

	want := buildOpeningQuestion("ruby-backend", embeddedOpeningConfig())
	evs := awaitOpening(t, c)
	for _, ev := range evs {
		if ev.typ == "text" {
			if ev.m["content"] != want {
				t.Errorf("无匹配方向应走默认文案: %q", ev.m["content"])
			}
			return
		}
	}
	t.Fatalf("未收到开场 text: %v", evs)
}

// start_phase 经 WS 切到 COMPLETED：不是可主动开始的阶段，忽略不落库
// （票面验收：仅接受 INTRO/TECH/PROJECT/HR）。
func TestWSStartPhaseCompleted忽略不落库(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id := gf.addMidwaySession(time.Hour)
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	sendWSMsg(t, c.conn, map[string]any{
		"type": "control", "action": "start_phase", "phase": "COMPLETED",
	})
	time.Sleep(200 * time.Millisecond)
	if len(gf.f.store.updatePhase) != 0 {
		t.Errorf("COMPLETED 不应作为 start_phase 目标落库: %v", gf.f.store.updatePhase)
	}
}

// 开场音频预热（Java warmupOpeningAudioCache L111-137）：开关默认关，不产生任何
// TTS 调用；开启时对全部去重后的开场文案逐条合成回填缓存（10 方向文案 + 算法默认
// 题 + 后端默认题，共 12 条互不重复）。
func TestWSOpening预热开关(t *testing.T) {
	t.Run("默认关不调用TTS", func(t *testing.T) {
		gf := newGatewayFixture(t, nil)
		gf.gw.WarmupOpeningAudio(context.Background())
		if n := gf.ttsSrv.ConnectionCount(); n != 0 {
			t.Errorf("预热关闭不应产生 TTS 连接: %d", n)
		}
		if gf.gw.openingCache.size() != 0 {
			t.Errorf("预热关闭不应有缓存条目: %d", gf.gw.openingCache.size())
		}
	})
	t.Run("开启后全量回填", func(t *testing.T) {
		gf := newGatewayFixture(t, func(cfg *Config, _ *[]string) {
			cfg.OpeningAudioWarmupEnabled = true
		})
		gf.gw.WarmupOpeningAudio(context.Background())
		if n := gf.ttsSrv.ConnectionCount(); n != 12 {
			t.Errorf("开启预热应合成全部 12 条去重文案，实际 %d 条连接", n)
		}
		if got, want := gf.gw.openingCache.size(), 12; got != want {
			t.Errorf("缓存应回填 %d 条，实际 %d", want, got)
		}
	})
}
