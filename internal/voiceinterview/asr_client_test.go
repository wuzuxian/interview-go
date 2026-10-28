// ASR 协议客户端契约测试：真客户端 × 进程内假 DashScope 服务端，全程无外网
// （spec 测试决策：协议客户端契约测试）。只断言外部可观察行为：握手与线协议内容、
// 事件解析回调、ready 监督、重启与多会话隔离；不断言客户端内部实现细节。
package voiceinterview

import (
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"
)

// asrTestCallbacks 录制全部回调事件（并发安全），按会话归属断言。
type asrTestCallbacks struct {
	mu         sync.Mutex
	ready      []string
	partials   []asrTextEvent
	finals     []asrTextEvent
	errors     []asrErrorEvent
	reconnects []string
	closes     []asrCloseEvent
}

type asrTextEvent struct {
	sessionID string
	text      string
}

type asrErrorEvent struct {
	sessionID string
	err       error
}

type asrCloseEvent struct {
	sessionID string
	code      int
	reason    string
}

func (r *asrTestCallbacks) cbs() AsrCallbacks {
	return AsrCallbacks{
		OnReady:        func(sessionID string) { r.record(&r.ready, sessionID) },
		OnPartial:      func(sessionID, text string) { r.appendPartial(sessionID, text) },
		OnFinal:        func(sessionID, text string) { r.appendFinal(sessionID, text) },
		OnError:        func(sessionID string, err error) { r.appendError(sessionID, err) },
		OnReconnecting: func(sessionID string) { r.record(&r.reconnects, sessionID) },
		OnClose:        func(sessionID string, code int, reason string) { r.appendClose(sessionID, code, reason) },
	}
}

func (r *asrTestCallbacks) record(dst *[]string, v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*dst = append(*dst, v)
}

func (r *asrTestCallbacks) appendPartial(sessionID, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.partials = append(r.partials, asrTextEvent{sessionID, text})
}

func (r *asrTestCallbacks) appendFinal(sessionID, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finals = append(r.finals, asrTextEvent{sessionID, text})
}

func (r *asrTestCallbacks) appendError(sessionID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, asrErrorEvent{sessionID, err})
}

func (r *asrTestCallbacks) appendClose(sessionID string, code int, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closes = append(r.closes, asrCloseEvent{sessionID, code, reason})
}

// waitFor 轮询直到 cond 成立或超时（回调来自客户端 goroutine，测试以轮询同步）。
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待条件超时（3s）")
}

// fastAsrConfig 快速行为参数（Config 注入，测试不必等真实 10s/200ms/1s）。
func fastAsrConfig() Config {
	return Config{
		AsrReadyTimeout:        80 * time.Millisecond,
		AsrReadyRetries:        2,
		AsrRestartPause:        10 * time.Millisecond,
		AsrRestartReadyTimeout: 300 * time.Millisecond,
	}
}

// testAsrSettings 对齐 Java application.yml 默认值（qwen.asr.*）的业务参数。
func testAsrSettings(url string) AsrSettings {
	return AsrSettings{
		URL:                            url,
		Model:                          "qwen3-asr-flash-realtime",
		APIKey:                         "sk-test-1234567890",
		Language:                       "zh",
		Format:                         "pcm",
		SampleRate:                     16000,
		EnableTurnDetection:            true,
		TurnDetectionType:              "server_vad",
		TurnDetectionThreshold:         0.5,
		TurnDetectionSilenceDurationMs: 2000,
	}
}

// startReadyClient 起一个已 ready 的客户端会话（超时即失败）。
func startReadyClient(t *testing.T, client *AsrClient, rec *asrTestCallbacks, sessionID string) {
	t.Helper()
	if err := client.Start(sessionID, rec.cbs()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	waitFor(t, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.ready) > 0
	})
}

// newTestClient 以默认测试配置创建指向假服务端的客户端。
func newTestClient(t *testing.T, srv *fakeAsrServer) *AsrClient {
	t.Helper()
	client, err := NewAsrClient(fastAsrConfig(), testAsrSettings(srv.wsURL()), nil)
	if err != nil {
		t.Fatalf("NewAsrClient: %v", err)
	}
	return client
}

func mustBase64Decode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	return b
}

func withType(msg map[string]any, typ string) map[string]any {
	msg["type"] = typ
	return msg
}

// ===== 契约：握手与会话更新参数 =====

func TestAsrClient握手与会话更新参数逐字对齐(t *testing.T) {
	srv := newFakeAsrServer(t)
	client, err := NewAsrClient(fastAsrConfig(), testAsrSettings(srv.wsURL()), nil)
	if err != nil {
		t.Fatalf("NewAsrClient: %v", err)
	}
	defer client.Close()
	startReadyClient(t, client, &asrTestCallbacks{}, "s1")

	conns, handshakes, auths, inspects, _, _, _ := srv.snapshot()
	if len(conns) != 1 {
		t.Fatalf("应只建立 1 条连接，实际 %d", len(conns))
	}
	// OmniRealtimeParam.getUrl()：url + "?model=" + model。
	if handshakes[0] != "/?model=qwen3-asr-flash-realtime" {
		t.Errorf("升级请求 URI 应为 /?model=qwen3-asr-flash-realtime，实际 %q", handshakes[0])
	}
	// DashScopeHeaders.buildWebSocketHeaders：Bearer 鉴权 + 数据巡检开关。
	if auths[0] != "Bearer sk-test-1234567890" {
		t.Errorf("Authorization 头应为 Bearer <key>，实际 %q", auths[0])
	}
	if inspects[0] != "enable" {
		t.Errorf("X-DashScope-DataInspection 头应为 enable，实际 %q", inspects[0])
	}

	session := srv.lastSessionUpdate()
	// OmniRealtimeConfig.getConfig()：modalities=[TEXT]（QwenAsrService 传 TEXT 单元素）。
	mods, ok := session["modalities"].([]any)
	if !ok || len(mods) != 1 || mods[0] != "TEXT" {
		t.Errorf("modalities 应为 [\"TEXT\"]，实际 %v", session["modalities"])
	}
	// 打断检测：type=server_vad、threshold、prefix_padding_ms=0（SDK builder 默认）、静音 2000ms。
	td, ok := session["turn_detection"].(map[string]any)
	if !ok {
		t.Fatalf("应包含 turn_detection 对象，实际 session=%v", session)
	}
	if td["type"] != "server_vad" {
		t.Errorf("turn_detection.type 应为 server_vad，实际 %v", td["type"])
	}
	if td["threshold"] != 0.5 {
		t.Errorf("turn_detection.threshold 应为 0.5，实际 %v", td["threshold"])
	}
	if td["prefix_padding_ms"] != float64(0) {
		t.Errorf("turn_detection.prefix_padding_ms 应为 0（SDK builder 默认），实际 %v", td["prefix_padding_ms"])
	}
	if td["silence_duration_ms"] != float64(2000) {
		t.Errorf("turn_detection.silence_duration_ms 应为 2000，实际 %v", td["silence_duration_ms"])
	}
	// 转写参数：采样率/格式/语言。
	tr, ok := session["input_audio_transcription"].(map[string]any)
	if !ok {
		t.Fatalf("应包含 input_audio_transcription 对象，实际 session=%v", session)
	}
	if tr["sample_rate"] != float64(16000) || tr["input_audio_format"] != "pcm" || tr["language"] != "zh" {
		t.Errorf("input_audio_transcription 应为 {16000, pcm, zh}，实际 %v", tr)
	}
	// SDK 对 null 字段（voice、input_audio_format、output_audio_format）序列化时省略。
	for _, absent := range []string{"voice", "input_audio_format", "output_audio_format"} {
		if _, exists := session[absent]; exists {
			t.Errorf("session 不应包含未配置字段 %q（SDK gson 省略 null）", absent)
		}
	}
}

func TestAsrClient关闭打断检测时不发送turn_detection(t *testing.T) {
	srv := newFakeAsrServer(t)
	settings := testAsrSettings(srv.wsURL())
	settings.EnableTurnDetection = false
	client, err := NewAsrClient(fastAsrConfig(), settings, nil)
	if err != nil {
		t.Fatalf("NewAsrClient: %v", err)
	}
	defer client.Close()
	startReadyClient(t, client, &asrTestCallbacks{}, "s1")

	session := srv.lastSessionUpdate()
	if _, exists := session["turn_detection"]; exists {
		t.Errorf("关闭打断检测时不应发送 turn_detection（SDK ifeq 跳过），实际 %v", session["turn_detection"])
	}
}

func TestAsrClient缺少APIKey时构造失败(t *testing.T) {
	settings := testAsrSettings("ws://127.0.0.1:1")
	settings.APIKey = "  "
	if _, err := NewAsrClient(fastAsrConfig(), settings, nil); err == nil {
		t.Fatal("空 APIKey 构造应报错（Java init IllegalStateException 语义）")
	}
}

// ===== 契约：音频推送与事件解析 =====

func TestAsrClient音频以base64推送且服务端可还原(t *testing.T) {
	srv := newFakeAsrServer(t)
	client := newTestClient(t, srv)
	defer client.Close()
	startReadyClient(t, client, &asrTestCallbacks{}, "s1")

	pcm := []byte{0x01, 0x00, 0x02, 0x00, 0xFF, 0xFF}
	if err := client.SendAudio("s1", pcm); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}
	waitFor(t, func() bool { return len(srv.appendPayloads()) == 1 })

	appends := srv.appendPayloads()
	decoded := mustBase64Decode(t, appends[0])
	if string(decoded) != string(pcm) {
		t.Errorf("服务端收到的音频应与原始 PCM 一致，实际 %v", decoded)
	}
}

func TestAsrClient部分识别事件文本提取语义(t *testing.T) {
	cases := []struct {
		name string
		msg  map[string]any // conversation.item.input_audio_transcription.{text,delta} 事件负载
		want string
		skip bool // 期望不产生回调（空白/无文本）
	}{
		{
			name: "transcript直取",
			msg:  map[string]any{"transcript": "你好"},
			want: "你好",
		},
		{
			name: "text与stash拼接",
			msg:  map[string]any{"text": "已确认前缀", "stash": "草稿后缀"},
			want: "已确认前缀草稿后缀",
		},
		{
			name: "仅stash也回调",
			msg:  map[string]any{"stash": "只有草稿"},
			want: "只有草稿",
		},
		{
			name: "delta为字符串",
			msg:  map[string]any{"delta": "增量"},
			want: "增量",
		},
		{
			name: "delta为对象取text",
			msg:  map[string]any{"delta": map[string]any{"text": "对象增量"}},
			want: "对象增量",
		},
		{
			name: "delta为对象取transcript",
			msg:  map[string]any{"delta": map[string]any{"transcript": "对象转写"}},
			want: "对象转写",
		},
		{
			name: "item内transcript兜底",
			msg:  map[string]any{"item": map[string]any{"transcript": "条目转写"}},
			want: "条目转写",
		},
		{
			name: "空白文本跳过回调",
			msg:  map[string]any{"text": "  ", "stash": ""},
			skip: true,
		},
		{
			name: "无可提取文本跳过回调",
			msg:  map[string]any{"other": "x"},
			skip: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeAsrServer(t)
			srv.script = []asrFakeEvent{{msg: withType(tc.msg, "conversation.item.input_audio_transcription.text")}}
			rec := &asrTestCallbacks{}
			client := newTestClient(t, srv)
			defer client.Close()
			startReadyClient(t, client, rec, "s1")

			if tc.skip {
				waitFor(t, func() bool {
					rec.mu.Lock()
					defer rec.mu.Unlock()
					return len(rec.partials) == 0 && len(rec.finals) == 0
				})
				return
			}
			waitFor(t, func() bool {
				rec.mu.Lock()
				defer rec.mu.Unlock()
				return len(rec.partials) == 1
			})
			rec.mu.Lock()
			got := rec.partials[0]
			rec.mu.Unlock()
			if got.sessionID != "s1" || got.text != tc.want {
				t.Errorf("部分识别回调应为 (s1, %q)，实际 (%s, %q)", tc.want, got.sessionID, got.text)
			}
		})
	}
}

func TestAsrClient最终识别与错误脱敏回调(t *testing.T) {
	srv := newFakeAsrServer(t)
	srv.script = []asrFakeEvent{
		{msg: map[string]any{
			"type":       "conversation.item.input_audio_transcription.completed",
			"transcript": "请介绍一下你的项目经验",
			"language":   "zh",
			"emotion":    "neutral",
		}},
		{delay: 30 * time.Millisecond, msg: map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "invalid_request", "code": "param_error", "message": "secret-内部敏感详情-不得外泄"},
		}},
	}
	rec := &asrTestCallbacks{}
	client := newTestClient(t, srv)
	defer client.Close()
	startReadyClient(t, client, rec, "s1")

	waitFor(t, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.finals) == 1
	})
	rec.mu.Lock()
	final := rec.finals[0]
	rec.mu.Unlock()
	if final.sessionID != "s1" || final.text != "请介绍一下你的项目经验" {
		t.Errorf("最终识别回调应为 (s1, 请介绍一下你的项目经验)，实际 %v", final)
	}

	// error 事件：文案对齐 Java「ASR Error [%s/%s]: %s」且原文被脱敏为 external-error。
	waitFor(t, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.errors) == 1
	})
	rec.mu.Lock()
	errEv := rec.errors[0]
	rec.mu.Unlock()
	want := "ASR Error [invalid_request/param_error]: external-error"
	if errEv.sessionID != "s1" || errEv.err == nil || errEv.err.Error() != want {
		t.Errorf("错误回调应为 (s1, %q)，实际 %v", want, errEv)
	}
	if errEv.err != nil && strings.Contains(errEv.err.Error(), "secret-内部敏感详情") {
		t.Error("错误回调不得包含服务端原始 message（脱敏语义）")
	}
}

func TestAsrClient单句转写失败事件仅记录不回调(t *testing.T) {
	srv := newFakeAsrServer(t)
	srv.script = []asrFakeEvent{
		{msg: map[string]any{"type": "conversation.item.input_audio_transcription.failed"}},
	}
	rec := &asrTestCallbacks{}
	client := newTestClient(t, srv)
	defer client.Close()
	startReadyClient(t, client, rec, "s1")

	// Java：该事件只记 error 日志，不触发 onError/onFinal/onPartial。
	waitFor(t, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.finals) == 0 && len(rec.errors) == 0 && len(rec.partials) == 0 && len(rec.ready) > 0
	})
}

// ===== 契约：SendAudio 错误语义（管道据此分类：静默丢块 / 重启重发） =====

func TestAsrClient送入音频的错误语义(t *testing.T) {
	t.Run("无活动会话", func(t *testing.T) {
		srv := newFakeAsrServer(t)
		client := newTestClient(t, srv)
		defer client.Close()
		err := client.SendAudio("ghost", []byte{1})
		if err == nil || !strings.Contains(err.Error(), "No active session found") {
			t.Errorf("无会话应报 No active session found，实际 %v", err)
		}
	})

	t.Run("会话未就绪等待1200ms后报错", func(t *testing.T) {
		srv := newFakeAsrServer(t)
		srv.stallHandshake = true // 永远不就绪
		client := newTestClient(t, srv)
		defer client.Close()
		if err := client.Start("s1", (&asrTestCallbacks{}).cbs()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		start := time.Now()
		err := client.SendAudio("s1", []byte{1})
		if err == nil || !strings.Contains(err.Error(), "ASR session not ready") {
			t.Errorf("未就绪应报 ASR session not ready，实际 %v", err)
		}
		if elapsed := time.Since(start); elapsed < 1100*time.Millisecond {
			t.Errorf("未就绪应等待约 1200ms（Java awaitReady(1200)），实际 %v", elapsed)
		}
	})

	t.Run("重启停起间隙送入语义为失败可重发", func(t *testing.T) {
		srv := newFakeAsrServer(t)
		client := newTestClient(t, srv)
		defer client.Close()
		startReadyClient(t, client, &asrTestCallbacks{}, "s1")
		client.Stop("s1")
		err := client.SendAudio("s1", []byte{1})
		if err == nil || !strings.Contains(err.Error(), "No active session found") {
			t.Errorf("Stop 后送入应报 No active session found（可重发语义），实际 %v", err)
		}
	})
}

// ===== 契约：连接关闭与多会话隔离 =====

func TestAsrClient服务端关闭触发回调并清理状态(t *testing.T) {
	srv := newFakeAsrServer(t)
	rec := &asrTestCallbacks{}
	client := newTestClient(t, srv)
	defer client.Close()
	startReadyClient(t, client, rec, "s1")

	srv.closeConn(0, 1000, "bye")
	waitFor(t, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.closes) == 1
	})
	rec.mu.Lock()
	closed := rec.closes[0]
	rec.mu.Unlock()
	if closed.sessionID != "s1" || closed.code != 1000 {
		t.Errorf("关闭回调应为 (s1, 1000, …)，实际 %v", closed)
	}
	waitFor(t, func() bool { return !client.HasActiveSession("s1") })
	if client.IsReady("s1") {
		t.Error("连接关闭后 IsReady 应为 false")
	}
}

func TestAsrClient旧连接迟到关闭不误删新连接(t *testing.T) {
	srv := newFakeAsrServer(t)
	rec := &asrTestCallbacks{}
	client := newTestClient(t, srv)
	defer client.Close()
	startReadyClient(t, client, rec, "s1")

	// 重启产生第二条连接（Java restartTranscription：旧 onClose 与新连接竞态的根因场景）。
	client.Restart("s1")
	if !client.HasActiveSession("s1") {
		t.Fatal("重启后应有活动会话")
	}

	// 迟到的旧连接关闭。OnClose 回调在客户端处理完关闭（含状态清理）之后触发，
	// 因此回调到达后再断言状态，能确定性地暴露「误删新连接」的缺陷。
	srv.closeConn(0, 1001, "late close of old conn")
	waitFor(t, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return len(rec.closes) == 1
	})
	rec.mu.Lock()
	closed := rec.closes[0]
	rec.mu.Unlock()
	if closed.sessionID != "s1" {
		t.Errorf("迟到关闭的回调归属应为 s1，实际 %v", closed)
	}
	if !client.HasActiveSession("s1") {
		t.Error("旧连接迟到 onClose 不得移除新连接的会话状态")
	}
	if !client.IsReady("s1") {
		t.Error("新连接就绪状态不得被旧连接关闭破坏")
	}
}

func TestAsrClient多会话隔离与事件归属(t *testing.T) {
	srv := newFakeAsrServer(t)
	recA, recB := &asrTestCallbacks{}, &asrTestCallbacks{}
	client := newTestClient(t, srv)
	defer client.Close()
	startReadyClient(t, client, recA, "sa")
	startReadyClient(t, client, recB, "sb")

	if n := srv.connCount(); n != 2 {
		t.Fatalf("两个会话应建立 2 条连接，实际 %d", n)
	}
	// 只给 A 的连接推最终事件：仅 A 收到，且回调归属 sa。
	srv.push(0, map[string]any{
		"type":       "conversation.item.input_audio_transcription.completed",
		"transcript": "甲会话的识别结果",
	})
	waitFor(t, func() bool {
		recA.mu.Lock()
		defer recA.mu.Unlock()
		return len(recA.finals) == 1
	})
	recB.mu.Lock()
	bFinals := len(recB.finals)
	recB.mu.Unlock()
	if bFinals != 0 {
		t.Errorf("B 会话不应收到 A 连接上的事件，实际 %d 条", bFinals)
	}
	recA.mu.Lock()
	final := recA.finals[0]
	recA.mu.Unlock()
	if final.sessionID != "sa" || final.text != "甲会话的识别结果" {
		t.Errorf("回调归属应为 sa，实际 %v", final)
	}

	// Stop 其一不影响另一个。
	client.Stop("sa")
	waitFor(t, func() bool { return !client.HasActiveSession("sa") })
	if !client.HasActiveSession("sb") {
		t.Error("Stop sa 不得影响 sb")
	}
}

// ===== 契约：ready 监督（10s×2 重试对齐 Java，测试以 Config 注入缩短） =====

func TestAsrClient就绪超时重启至多重试两次后报错(t *testing.T) {
	srv := newFakeAsrServer(t)
	srv.stallHandshake = true
	rec := &asrTestCallbacks{}
	cfg := fastAsrConfig()
	client, err := NewAsrClient(cfg, testAsrSettings(srv.wsURL()), nil)
	if err != nil {
		t.Fatalf("NewAsrClient: %v", err)
	}
	defer client.Close()
	if err := client.Start("s1", rec.cbs()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Java checkAsrReadyOrRetry：初始检查 + 至多 2 次（重连提示 + 重启），耗尽后报错。
	// 注意：耗尽后的监督终态错误与最后一次建连失败的 OnError 并发到达，
	// 因此断言「出现过终态错误」而非「最后一条是终态错误」。
	var readyTimeoutErr error
	waitFor(t, func() bool {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if len(rec.reconnects) != int(cfg.AsrReadyRetries) {
			return false
		}
		for _, ev := range rec.errors {
			if ev.err != nil && strings.Contains(ev.err.Error(), "not ready after") {
				readyTimeoutErr = ev.err
				return true
			}
		}
		return false
	})
	if readyTimeoutErr != nil && strings.Contains(readyTimeoutErr.Error(), "sk-test-1234567890") {
		t.Error("错误信息不得泄漏 APIKey")
	}
}

func TestAsrClient就绪后不再重启(t *testing.T) {
	srv := newFakeAsrServer(t)
	rec := &asrTestCallbacks{}
	client := newTestClient(t, srv)
	defer client.Close()
	startReadyClient(t, client, rec, "s1")

	time.Sleep(5 * fastAsrConfig().AsrReadyTimeout)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reconnects) != 0 {
		t.Errorf("已就绪会话不应触发重连，实际 %d 次", len(rec.reconnects))
	}
	if len(rec.errors) != 0 {
		t.Errorf("已就绪会话不应报错，实际 %v", rec.errors)
	}
}

// ===== 契约：重启流程与 Stop =====

func TestAsrClient重启建立新连接并停发session_finish(t *testing.T) {
	srv := newFakeAsrServer(t)
	client := newTestClient(t, srv)
	defer client.Close()
	startReadyClient(t, client, &asrTestCallbacks{}, "s1")

	client.Restart("s1")

	if n := srv.connCount(); n != 2 {
		t.Fatalf("重启应建立第二条连接，实际 %d 条", n)
	}
	handshakes := srv.handshakeURIs()
	if handshakes[1] != handshakes[0] {
		t.Errorf("重启后握手参数应一致，实际 %q vs %q", handshakes[0], handshakes[1])
	}
	if n := srv.sessionUpdateCount(); n != 2 {
		t.Fatalf("重启后应重发 session.update，实际 %d 次", n)
	}
	// 旧连接应被客户端关闭（Java stopTranscription：endSession + close）。
	waitFor(t, func() bool { return srv.connAt(0).isClosedByServer() })

	// 重启后新连接可继续送音频。
	if err := client.SendAudio("s1", []byte{0x03, 0x00}); err != nil {
		t.Fatalf("重启后 SendAudio: %v", err)
	}
	waitFor(t, func() bool { return len(srv.appendPayloads()) == 1 })

	// Stop：先发 session.finish 再关连接（Java stopTranscription）。
	// 累计 2 次：重启用 stop 挂断旧连接时已发过一次（Java restartTranscription 同样经
	// stopTranscription → endSession），本次 Stop 是第二次。
	client.Stop("s1")
	waitFor(t, func() bool { return srv.finishTotal() == 2 })
	waitFor(t, func() bool { return !client.HasActiveSession("s1") })
}

func TestAsrClientStop不存在的会话仅告警(t *testing.T) {
	srv := newFakeAsrServer(t)
	client := newTestClient(t, srv)
	defer client.Close()
	client.Stop("ghost") // Java：log.warn 后直接返回，不得 panic
}
