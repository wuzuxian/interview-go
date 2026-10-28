package voiceinterview

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wuzuxian/interview/config"
)

// testTtsSettings 返回指向假服务端的运行时参数（字段值取 Java application.yml
// app.voice-interview.qwen.tts 默认）。
func testTtsSettings(url string) TtsSettings {
	return TtsSettings{
		URL:          url,
		Model:        "qwen3-tts-flash-realtime",
		APIKey:       "sk-test-secret-key-123",
		Voice:        "Cherry",
		Format:       "pcm",
		SampleRate:   24000,
		Mode:         "commit",
		LanguageType: "Chinese",
		SpeechRate:   1.0,
		Volume:       60,
	}
}

// fastTtsConfig 返回缩短超时的客户端配置，保证测试确定且快速；
// 生产默认值（5s/8s/30s）由 TestTtsConfigDefaults 单独锁定。
func fastTtsConfig() Config {
	return Config{TTSConnectTimeout: 2 * time.Second, TTSTimeout: 500 * time.Millisecond, TTSFullTimeout: 500 * time.Millisecond}
}

// captureLogger 返回把日志写入 buf 的 slog.Logger（验证 APIKey 不进日志）。
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestTtsClientSynthesize 单次合成闭环：连服务端 → session.update（运行时参数）→
// append 文本 → commit → 收齐 response.audio.delta → response.done 后返回完整 PCM；
// APIKey 只出现在握手 Authorization 头。
func TestTtsClientSynthesize(t *testing.T) {
	fs := newFakeTtsServer(t)
	chunk1 := []byte{0x00, 0x11, 0x22, 0x33}
	chunk2 := []byte{0x44, 0x55, 0x66, 0x77, 0x88}
	fs.scripts = append(fs.scripts, scriptSynthDone(chunk1, chunk2))

	client := NewTtsClient(fastTtsConfig(), slog.Default())
	got, err := client.Synthesize(context.Background(), testTtsSettings(fs.URL()), "你好，请介绍一下你自己")
	if err != nil {
		t.Fatalf("Synthesize 报错: %v", err)
	}
	if want := append(append([]byte{}, chunk1...), chunk2...); !bytes.Equal(got, want) {
		t.Fatalf("PCM 不等于增量拼接\n got: %v\nwant: %v", got, want)
	}

	if n := fs.ConnectionCount(); n != 1 {
		t.Fatalf("连接数 = %d，want 1", n)
	}
	if header := fs.AuthHeader(0); header != "Bearer sk-test-secret-key-123" {
		t.Fatalf("握手 Authorization = %q，want Bearer 形式", header)
	}
	if got := fs.AppendedText(); got != "你好，请介绍一下你自己" {
		t.Fatalf("append 文本 = %q，want 原文", got)
	}

	// 帧序与字段：session.update（含全部运行时参数）→ append → commit。
	frames := fs.Frames()
	if len(frames) != 3 {
		t.Fatalf("客户端帧数 = %d，want 3（update/append/commit）", len(frames))
	}
	update := frames[0]
	if update["type"] != "session.update" {
		t.Fatalf("首帧 type = %v，want session.update", update["type"])
	}
	session, _ := update["session"].(map[string]any)
	wantSession := map[string]any{
		"voice":           "Cherry",
		"mode":            "commit",
		"response_format": "pcm",
		"sample_rate":     float64(24000),
		"speech_rate":     float64(1.0),
		"volume":          float64(60),
		"language_type":   "Chinese",
	}
	for key, want := range wantSession {
		if session[key] != want {
			t.Errorf("session.%s = %v，want %v（DashScope 字段名）", key, session[key], want)
		}
	}
	if _, ok := update["event_id"]; !ok {
		t.Errorf("session.update 缺少 event_id（协议要求每个客户端事件携带）")
	}
	if frames[1]["type"] != "input_text_buffer.append" {
		t.Errorf("第二帧 type = %v，want input_text_buffer.append", frames[1]["type"])
	}
	if frames[2]["type"] != "input_text_buffer.commit" {
		t.Errorf("第三帧 type = %v，want input_text_buffer.commit（commit 模式）", frames[2]["type"])
	}
}

// TestTtsClientNewConnectionPerSynthesis 每次合成独立新建连接：第一次失败
// （error 事件）不影响第二次（同客户端实例复用）。
func TestTtsClientNewConnectionPerSynthesis(t *testing.T) {
	fs := newFakeTtsServer(t)
	fs.scripts = append(fs.scripts,
		scriptErrorEvent("server_error", "internal_error", "boom"),
		scriptSynthDone([]byte{0xAA, 0xBB}),
	)

	client := NewTtsClient(fastTtsConfig(), slog.Default())
	settings := testTtsSettings(fs.URL())

	if _, err := client.Synthesize(context.Background(), settings, "第一句"); err == nil {
		t.Fatal("第一次合成（error 事件）应失败")
	}
	got, err := client.Synthesize(context.Background(), settings, "第二句")
	if err != nil {
		t.Fatalf("第二次合成应成功，却报错: %v", err)
	}
	if !bytes.Equal(got, []byte{0xAA, 0xBB}) {
		t.Fatalf("第二次合成 PCM = %v，want [170 187]", got)
	}
	if n := fs.ConnectionCount(); n != 2 {
		t.Fatalf("连接数 = %d，want 2（每次合成新建连接）", n)
	}
	if appended := fs.AppendedText(); appended != "第一句第二句" {
		t.Fatalf("两次合成的 append 文本 = %q，want 各自独立提交", appended)
	}
}

// TestTtsClientSynthesizeTimeout 合成超时：增量中途停滞时按超时失败返回错误；
// Synthesize 用分句超时（TTSTimeout），SynthesizeFull 用整段超时（TTSFullTimeout）。
func TestTtsClientSynthesizeTimeout(t *testing.T) {
	tests := []struct {
		name    string
		syn     func(*TtsClient, context.Context, TtsSettings, string) ([]byte, error)
		timeout time.Duration
	}{
		{"分句合成走 TTSTimeout", (*TtsClient).Synthesize, 300 * time.Millisecond},
		{"整段合成走 TTSFullTimeout", (*TtsClient).SynthesizeFull, 300 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFakeTtsServer(t)
			fs.scripts = append(fs.scripts, scriptStall([]byte{0x01, 0x02}))

			cfg := fastTtsConfig()
			cfg.TTSTimeout = tt.timeout
			cfg.TTSFullTimeout = tt.timeout
			client := NewTtsClient(cfg, slog.Default())

			start := time.Now()
			_, err := tt.syn(client, context.Background(), testTtsSettings(fs.URL()), " stalled")
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("合成超时应返回错误")
			}
			if !strings.Contains(err.Error(), "超时") {
				t.Fatalf("错误应说明超时，got: %v", err)
			}
			if elapsed < tt.timeout {
				t.Fatalf("过早返回（%v < 超时 %v）", elapsed, tt.timeout)
			}
			if elapsed > 5*time.Second {
				t.Fatalf("超时后未及时返回（耗时 %v）", elapsed)
			}
			if strings.Contains(err.Error(), "sk-test-secret-key-123") {
				t.Fatal("错误信息不得包含 APIKey")
			}
		})
	}
}

// TestTtsClientConnectTimeout 建连超时：服务端挂起握手时按超时失败（对齐 Java
// connectWithTimeout 的 5s 等待），测试以缩短的配置值验证机制。
func TestTtsClientConnectTimeout(t *testing.T) {
	url := newHangingTtsListener(t)
	cfg := Config{TTSConnectTimeout: 250 * time.Millisecond, TTSTimeout: 10 * time.Second, TTSFullTimeout: 10 * time.Second}
	client := NewTtsClient(cfg, slog.Default())

	start := time.Now()
	_, err := client.Synthesize(context.Background(), testTtsSettings(url), "握手挂起")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("挂起握手应按建连超时失败")
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("过早返回（%v < 建连超时 250ms）", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("建连超时后未及时返回（耗时 %v）", elapsed)
	}
	if !strings.Contains(err.Error(), "建连失败") {
		t.Fatalf("错误应说明建连失败，got: %v", err)
	}
}

// TestTtsClientDialRejected 拒连（HTTP 403，未升级）应返回带状态码的错误。
func TestTtsClientDialRejected(t *testing.T) {
	url := newRejectingTtsServer(t)
	client := NewTtsClient(fastTtsConfig(), slog.Default())

	_, err := client.Synthesize(context.Background(), testTtsSettings(url), "拒绝我吧")
	if err == nil {
		t.Fatal("拒连应返回错误")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("错误应带 HTTP 状态码（对齐 Java handshake failed: HTTP %d），got: %v", 403, err)
	}
	if strings.Contains(err.Error(), "sk-test-secret-key-123") {
		t.Fatal("错误信息不得包含 APIKey")
	}
}

// TestTtsClientServerErrorEvent 服务端 error 事件按 Java 的
// "TTS Error [type/code]: message" 形状返回错误，且不含 APIKey。
func TestTtsClientServerErrorEvent(t *testing.T) {
	fs := newFakeTtsServer(t)
	fs.scripts = append(fs.scripts, scriptErrorEvent("invalid_request", "model_not_exist", "Model not exist"))

	buf := &bytes.Buffer{}
	client := NewTtsClient(fastTtsConfig(), captureLogger(buf))
	_, err := client.Synthesize(context.Background(), testTtsSettings(fs.URL()), "错误事件")
	if err == nil {
		t.Fatal("error 事件应返回错误")
	}
	want := "TTS Error [invalid_request/model_not_exist]: Model not exist"
	if err.Error() != want {
		t.Fatalf("错误形状与 Java 不一致\n got: %q\nwant: %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), "sk-test-secret-key-123") {
		t.Fatal("错误信息不得包含 APIKey")
	}
	if strings.Contains(buf.String(), "sk-test-secret-key-123") {
		t.Fatal("日志不得包含 APIKey")
	}
}

// TestTtsClientEmptyTextShortCircuits 空文本直接返回空 PCM 不建连
// （对齐 Java synthesize 对空文本的短路）。
func TestTtsClientEmptyTextShortCircuits(t *testing.T) {
	fs := newFakeTtsServer(t)
	client := NewTtsClient(fastTtsConfig(), slog.Default())
	settings := testTtsSettings(fs.URL())

	for _, text := range []string{"", "   ", "\t\n"} {
		got, err := client.Synthesize(context.Background(), settings, text)
		if err != nil {
			t.Fatalf("空文本 %q 不应报错: %v", text, err)
		}
		if len(got) != 0 {
			t.Fatalf("空文本 %q 应返回空 PCM，got %d 字节", text, len(got))
		}
	}
	if n := fs.ConnectionCount(); n != 0 {
		t.Fatalf("空文本不应建连，连接数 = %d", n)
	}
}

// TestTtsClientResponseDoneWithoutAudio 只有 response.done 没有增量时返回空 PCM
// 且不报错（DashScope 可能对空合成结果直接收尾）。
func TestTtsClientResponseDoneWithoutAudio(t *testing.T) {
	fs := newFakeTtsServer(t)
	fs.scripts = append(fs.scripts, scriptSynthDone())

	client := NewTtsClient(fastTtsConfig(), slog.Default())
	got, err := client.Synthesize(context.Background(), testTtsSettings(fs.URL()), "没有音频的合成")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("应返回空 PCM，got %d 字节", len(got))
	}
}

// TestTtsClientAPIKeyNeverLogged 失败路径全程 APIKey 不进日志（握手头/URL/事件均非密钥出口）。
func TestTtsClientAPIKeyNeverLogged(t *testing.T) {
	fs := newFakeTtsServer(t)
	fs.scripts = append(fs.scripts, scriptErrorEvent("server_error", "quota_exceeded", "quota"))

	buf := &bytes.Buffer{}
	client := NewTtsClient(fastTtsConfig(), captureLogger(buf))
	if _, err := client.Synthesize(context.Background(), testTtsSettings(fs.URL()), "日志检查"); err == nil {
		t.Fatal("预期失败路径")
	}
	if strings.Contains(buf.String(), "sk-test-secret-key-123") {
		t.Fatalf("日志包含 APIKey:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "TTS") {
		t.Fatalf("客户端应产出中文结构化日志，got:\n%s", buf.String())
	}
}

// TestTtsConfigDefaults 锁定 TTS 超时默认值：建连 5s、分句 8s、整段 30s
// （Java application.yml tts-connect-timeout-seconds=5 / tts-timeout-seconds=8 与
// QwenTtsService 的 30s latch 常量）。
func TestTtsConfigDefaults(t *testing.T) {
	cfg := config.New()
	var appCfg struct {
		VoiceInterview Config `config:"voice_interview"`
	}
	if err := cfg.UnmarshalKey("app", &appCfg); err != nil {
		t.Fatalf("绑定默认配置失败: %v", err)
	}
	got := appCfg.VoiceInterview
	if got.TTSConnectTimeout != 5*time.Second {
		t.Errorf("TTSConnectTimeout 默认 = %v，want 5s", got.TTSConnectTimeout)
	}
	if got.TTSTimeout != 8*time.Second {
		t.Errorf("TTSTimeout 默认 = %v，want 8s", got.TTSTimeout)
	}
	if got.TTSFullTimeout != 30*time.Second {
		t.Errorf("TTSFullTimeout 默认 = %v，want 30s", got.TTSFullTimeout)
	}
}

// TestSynthesizeToWav 闭环辅助：PCM → WAV 头长度与字段（Java synthesizeToWav 路径）。
func TestSynthesizeToWav(t *testing.T) {
	pcm := bytes.Repeat([]byte{0x11, 0x22}, 100)
	wav := PcmToWav(pcm)
	if len(wav) != 44+len(pcm) {
		t.Fatalf("WAV 长度 = %d，want %d", len(wav), 44+len(pcm))
	}
	if byteRate := binary.LittleEndian.Uint32(wav[28:32]); byteRate != 48000 {
		t.Fatalf("byte rate = %d，want 48000（24000Hz × 16bit × 单声道）", byteRate)
	}
}
