// 语音冒烟 WS 消息收集器的单元测试（票 10）：等待/记录/断言统计的外部行为，
// 表驱动 + 中文用例名。收集器是 voice 模式「任一步失败即停」判定的数据面。
package main

import (
	"errors"
	"testing"
	"time"
)

func TestWSCollectorWaitFor(t *testing.T) {
	t.Run("命中已记录的历史消息", func(t *testing.T) {
		c := newWSCollector()
		c.Record(wsDownMessage{Type: "control", Action: "welcome"})

		got, err := c.waitFor(func(m wsDownMessage) bool {
			return m.Type == "control" && m.Action == "welcome"
		}, time.Second)
		if err != nil {
			t.Fatalf("应命中已记录的 welcome: %v", err)
		}
		if got.Action != "welcome" {
			t.Fatalf("应命中 welcome，实际 %v", got)
		}
	})

	t.Run("阻塞等待后续消息命中", func(t *testing.T) {
		c := newWSCollector()
		go func() {
			time.Sleep(20 * time.Millisecond)
			c.Record(wsDownMessage{Type: "control", Action: "asr_ready"})
		}()

		got, err := c.waitFor(func(m wsDownMessage) bool {
			return m.Type == "control" && m.Action == "asr_ready"
		}, time.Second)
		if err != nil {
			t.Fatalf("应等到后续 asr_ready: %v", err)
		}
		if got.Action != "asr_ready" {
			t.Fatalf("应命中 asr_ready，实际 %v", got)
		}
	})

	t.Run("超时未命中返回错误", func(t *testing.T) {
		c := newWSCollector()
		if _, err := c.waitFor(func(wsDownMessage) bool { return false }, 30*time.Millisecond); err == nil {
			t.Fatal("超时应返回错误")
		}
	})

	t.Run("连接读错误时终止等待", func(t *testing.T) {
		c := newWSCollector()
		go func() {
			time.Sleep(20 * time.Millisecond)
			c.RecordError(errors.New("websocket: close 1006"))
		}()
		if _, err := c.waitFor(func(wsDownMessage) bool { return false }, time.Second); err == nil {
			t.Fatal("读错误应终止等待并返回错误")
		}
		if got := c.terminalError(); got == nil {
			t.Fatal("读错误应被记录")
		}
	})
}

func TestWSCollectorTurnAssertions(t *testing.T) {
	// 模拟一次完整链路的下行消息序列（开场 + 回合）。
	c := newWSCollector()
	for _, m := range []wsDownMessage{
		{Type: "control", Action: "welcome", Message: "连接成功，准备开始语音面试"},
		{Type: "control", Action: "asr_ready"},
		{Type: "text", Content: "请先做个自我介绍", Final: true},
		{Type: "audio", Data: "d291bGRiZXdhdmlk", Text: "请先做个自我介绍"},
		{Type: "subtitle", Text: "请先", IsFinal: false},
		{Type: "subtitle", Text: "请先做个自我介绍", IsFinal: false},
		{Type: "subtitle", Text: "请先做个自我介绍", IsFinal: true},
		{Type: "text", Content: "好的", Final: false},
		{Type: "text", Content: "好的，我们开始。", Final: true},
		{Type: "audio_chunk", Data: "Y2h1bmsx", Index: 0},
		{Type: "audio_chunk", Data: "Y2h1bmsy", Index: 1},
		{Type: "control", Action: "audio_complete"},
	} {
		c.Record(m)
	}

	if !c.hasNonEmptySubtitle() {
		t.Fatal("应判定收到过非空字幕")
	}
	if got := c.lastSubtitleText(); got != "请先做个自我介绍" {
		t.Fatalf("最后字幕应为合并文本，实际 %q", got)
	}
	if final := c.subtitleFinalText(); final != "请先做个自我介绍" {
		t.Fatalf("subtitle(isFinal=true) 文本应为 %q，实际 %q", "请先做个自我介绍", final)
	}
	if got := c.textFinalContent(); got != "好的，我们开始。" {
		t.Fatalf("text(final=true) 内容应为最终回复，实际 %q", got)
	}
	if n := c.audioChunkCount(); n != 2 {
		t.Fatalf("audio_chunk 应为 2 条，实际 %d", n)
	}
	if !c.hasAudioComplete() {
		t.Fatal("应判定收到 audio_complete")
	}
	if got := c.openingAudioBase64(); got != "d291bGRiZXdhdmlk" {
		t.Fatalf("开场音频应为首条非空 audio data，实际 %q", got)
	}
	if msg := c.firstErrorMessage(); msg != "" {
		t.Fatalf("无 error 消息时应返回空串，实际 %q", msg)
	}
}

func TestWSCollectorEdgeCases(t *testing.T) {
	t.Run("空字幕不参与非空判定", func(t *testing.T) {
		c := newWSCollector()
		c.Record(wsDownMessage{Type: "subtitle", Text: "", IsFinal: true})
		if c.hasNonEmptySubtitle() {
			t.Fatal("空字幕不应判定为非空")
		}
		if c.subtitleFinalText() != "" {
			t.Fatal("空 final 字幕不应产出文本")
		}
	})
	t.Run("提取首条 error 话术", func(t *testing.T) {
		c := newWSCollector()
		c.Record(wsDownMessage{Type: "control", Action: "welcome"})
		c.Record(wsDownMessage{Type: "error", Message: "语音识别失败: bad key"})
		if got := c.firstErrorMessage(); got != "语音识别失败: bad key" {
			t.Fatalf("应提取首条 error 话术，实际 %q", got)
		}
	})
}

func TestWSURLFromBase(t *testing.T) {
	cases := []struct {
		name string
		base string
		path string
		want string
		err  bool
	}{
		{name: "http 转 ws", base: "http://127.0.0.1:8080", path: "/ws/voice-interview/3", want: "ws://127.0.0.1:8080/ws/voice-interview/3"},
		{name: "https 转 wss", base: "https://example.com", path: "/ws/voice-interview/3", want: "wss://example.com/ws/voice-interview/3"},
		{name: "base 尾部斜杠容忍", base: "http://127.0.0.1:8080/", path: "/ws/voice-interview/3", want: "ws://127.0.0.1:8080/ws/voice-interview/3"},
		{name: "非相对路径报错", base: "http://127.0.0.1:8080", path: "ftp://x/y", err: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := wsURLFromBase(tc.base, tc.path)
			if tc.err {
				if err == nil {
					t.Fatal("应报错")
				}
				return
			}
			if err != nil {
				t.Fatalf("应成功: %v", err)
			}
			if got != tc.want {
				t.Fatalf("WS 地址应为 %s，实际 %s", tc.want, got)
			}
		})
	}
}
