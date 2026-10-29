// 语音冒烟全流程的流程级测试（票 10）：进程内 httptest 起假服务端（REST + 假
// WS 端点），验证 runVoice 的整条编排——前置检查 → 创建场次 → WS 开场白 →
// 喂帧（TTS→重采样→ASR 回环的客户端半程）→ submit → 回合断言 → 结束 → 轮询
// 评估 → 报告校验 → 清理——以及失败即停路径。只测外部行为：假服务端按真实
// 契约下发/接收 JSON，断言冒烟命令的可见产出与发出的请求。
package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wuzuxian/interview/internal/voiceinterview"
)

// shortenFeedPacing 注入极短喂帧节奏并返回还原函数（流程测试不必按真实时长跑）。
func shortenFeedPacing(t *testing.T) {
	t.Helper()
	origInterval, origSilence, origGrace := feedFrameInterval, feedTrailingSilence, feedFinalizeGrace
	feedFrameInterval = time.Millisecond
	feedTrailingSilence = 10 * time.Millisecond
	feedFinalizeGrace = 20 * time.Millisecond
	t.Cleanup(func() {
		feedFrameInterval, feedTrailingSilence, feedFinalizeGrace = origInterval, origSilence, origGrace
	})
}

func okJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success", "data": data})
}

// fakeVoiceServer 语音链路假服务端：按真实契约应答 REST 并以脚本化 WS 端点
// 模拟网关下行。记录收到的上行音频帧数、submit 修正文本与删除调用。
type fakeVoiceServer struct {
	srv *httptest.Server

	mu          sync.Mutex
	created     int
	deleted     int
	audioFrames int
	submitText  string
	ended       bool
	wsErr       error // WS 读循环退出原因（诊断用）
}

func newFakeVoiceServer(t *testing.T, openingPCM []int16) *fakeVoiceServer {
	t.Helper()
	f := &fakeVoiceServer{}

	// 开场音频：真实 TTS 产物形状 = 44 字节 WAV（24kHz）内包 PCM。
	raw := make([]byte, 0, len(openingPCM)*2)
	for _, s := range openingPCM {
		raw = binary.LittleEndian.AppendUint16(raw, uint16(s))
	}
	openingWavB64 := base64.StdEncoding.EncodeToString(voiceinterview.PcmToWav(raw))

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	writeJSON := func(conn *websocket.Conn, v any) {
		_ = conn.WriteJSON(v)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) { okJSON(w, "pong") })
	mux.HandleFunc("/api/llm-provider/voice/asr", func(w http.ResponseWriter, r *http.Request) {
		okJSON(w, map[string]any{"maskedApiKey": "sk-***abc"})
	})
	mux.HandleFunc("/api/llm-provider/voice/tts", func(w http.ResponseWriter, r *http.Request) {
		okJSON(w, map[string]any{"maskedApiKey": "sk-***abc"})
	})
	mux.HandleFunc("/api/voice-interview/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			f.mu.Lock()
			f.created++
			f.mu.Unlock()
			okJSON(w, map[string]any{
				"sessionId": 1, "roleType": "java-backend", "currentPhase": "TECH",
				"status": "IN_PROGRESS", "webSocketUrl": "/ws/voice-interview/1",
			})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/voice-interview/sessions/1", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			status := "IN_PROGRESS"
			if f.ended {
				status = "COMPLETED"
			}
			f.mu.Unlock()
			okJSON(w, map[string]any{"sessionId": 1, "status": status})
		case http.MethodDelete:
			f.mu.Lock()
			f.deleted++
			f.mu.Unlock()
			okJSON(w, nil)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/voice-interview/sessions/1/evaluation", func(w http.ResponseWriter, r *http.Request) {
		okJSON(w, map[string]any{
			"evaluateStatus": "COMPLETED", "evaluateError": nil,
			"evaluation": json.RawMessage(sampleVoiceReport(1)),
		})
	})
	mux.HandleFunc("/ws/voice-interview/1", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("[fake-ws] upgrade 失败: %v", err)
			return
		}
		defer conn.Close()
		// 开场序列：welcome → asr_ready → 开场白文本 → 开场音频（真实契约形状）。
		writeJSON(conn, map[string]any{"type": "control", "action": "welcome",
			"message": "连接成功，准备开始语音面试", "timestamp": 1})
		writeJSON(conn, map[string]any{"type": "control", "action": "asr_ready",
			"message": "语音识别已就绪", "timestamp": 2})
		writeJSON(conn, map[string]any{"type": "text", "content": "请先做个自我介绍", "final": true})
		writeJSON(conn, map[string]any{"type": "audio", "data": openingWavB64, "text": "请先做个自我介绍"})
		for {
			_, rawUp, err := conn.ReadMessage()
			if err != nil {
				t.Logf("[fake-ws] 读循环退出: %v", err)
				f.mu.Lock()
				f.wsErr = err
				f.mu.Unlock()
				return
			}
			var up struct {
				Type   string          `json:"type"`
				Action string          `json:"action"`
				Data   json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(rawUp, &up); err != nil {
				t.Logf("[fake-ws] 上行 JSON 解析失败: %v", err)
				return
			}
			switch up.Type {
			case "audio":
				f.mu.Lock()
				f.audioFrames++
				first := f.audioFrames == 1
				f.mu.Unlock()
				// 首帧后回字幕（ASR 识别结果的客户端可见产物）。
				if first {
					writeJSON(conn, map[string]any{"type": "subtitle",
						"text": "请先做个自我介绍", "isFinal": false})
				}
			case "control":
				switch up.Action {
				case "submit":
					var data map[string]any
					_ = json.Unmarshal(up.Data, &data)
					if text, ok := data["text"].(string); ok {
						f.mu.Lock()
						f.submitText = text
						f.mu.Unlock()
					}
					// 模型回合序列：subtitle(final) → text(final) → 分块音频 → 完成。
					writeJSON(conn, map[string]any{"type": "subtitle",
						"text": "请先做个自我介绍", "isFinal": true})
					writeJSON(conn, map[string]any{"type": "text",
						"content": "好的，我们开始。", "final": true})
					writeJSON(conn, map[string]any{"type": "audio_chunk",
						"data": "Y2h1bmsx", "index": 0, "isLast": false})
					writeJSON(conn, map[string]any{"type": "control",
						"action": "audio_complete", "message": "面试官语音播放完成", "timestamp": 3})
				case "end_interview":
					f.mu.Lock()
					f.ended = true
					f.mu.Unlock()
				}
			}
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVoiceServer) counters() (created, deleted, frames int, submit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created, f.deleted, f.audioFrames, f.submitText
}

func TestRunVoiceFlowHappyPath(t *testing.T) {
	shortenFeedPacing(t)
	// 开场音频 9600 样本@24kHz（400ms）→ 重采样 16kHz 得 6400 样本 → 2 帧。
	pcm := make([]int16, 9600)
	for i := range pcm {
		pcm[i] = int16(i % 97)
	}
	f := newFakeVoiceServer(t, pcm)

	out := &strings.Builder{}
	opts := &options{
		mode: "voice", baseURL: f.srv.URL, timeout: 5 * time.Second, evalWait: 2 * time.Second,
		skillID: "java-backend", difficulty: "mid",
	}
	err := runVoice(opts, out)
	f.mu.Lock()
	if f.wsErr != nil {
		t.Logf("假服务端 WS 读循环退出原因: %v", f.wsErr)
	}
	f.mu.Unlock()
	if err != nil {
		t.Fatalf("语音全链路应通过: %v\n输出:\n%s", err, out.String())
	}
	created, deleted, frames, submit := f.counters()
	if created != 1 {
		t.Fatalf("应创建 1 个场次，实际 %d", created)
	}
	if frames < 2 {
		t.Fatalf("应向 WS 喂入 ≥2 帧音频，实际 %d", frames)
	}
	if submit != "请先做个自我介绍" {
		t.Fatalf("submit 修正文本应携带 ASR 字幕，实际 %q", submit)
	}
	if deleted != 1 {
		t.Fatalf("默认应 DELETE 清理 1 次，实际 %d", deleted)
	}
	for _, marker := range []string{
		"前置检查语音配置", "TTS→重采样→ASR 回环", "模型回合断言", "轮询评估状态",
		"报告结构校验", "清理场次", "各步耗时汇总",
	} {
		if !strings.Contains(out.String(), marker) {
			t.Fatalf("步骤化输出缺少 %q\n输出:\n%s", marker, out.String())
		}
	}
}

func TestRunVoiceFlowKeepSkipsDelete(t *testing.T) {
	shortenFeedPacing(t)
	f := newFakeVoiceServer(t, make([]int16, 9600))

	out := &strings.Builder{}
	opts := &options{
		mode: "voice", baseURL: f.srv.URL, timeout: 5 * time.Second, evalWait: 2 * time.Second,
		skillID: "java-backend", difficulty: "mid", keep: true,
	}
	if err := runVoice(opts, out); err != nil {
		t.Fatalf("-keep 模式链路应通过: %v\n输出:\n%s", err, out.String())
	}
	if _, deleted, _, _ := f.counters(); deleted != 0 {
		t.Fatalf("-keep 应跳过 DELETE，实际删除 %d 次", deleted)
	}
	if !strings.Contains(out.String(), "-keep 指定，保留场次 1") {
		t.Fatalf("-keep 输出缺失\n输出:\n%s", out.String())
	}
}

func TestRunVoiceFlowFailsWhenKeyMissing(t *testing.T) {
	// 前置检查门禁：ASR 掩码 Key 为 "***"（缺失）时清晰报错且不创建场次
	//（前置检查在创建场次之前，服务端不应收到 POST）。
	mux := http.NewServeMux()
	created := 0
	var mu sync.Mutex
	mux.HandleFunc("/api/llm-provider/voice/asr", func(w http.ResponseWriter, r *http.Request) {
		okJSON(w, map[string]any{"maskedApiKey": "***"})
	})
	mux.HandleFunc("/api/llm-provider/voice/tts", func(w http.ResponseWriter, r *http.Request) {
		okJSON(w, map[string]any{"maskedApiKey": "sk-***abc"})
	})
	mux.HandleFunc("/api/voice-interview/sessions", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		created++
		mu.Unlock()
		okJSON(w, map[string]any{"sessionId": 1, "status": "IN_PROGRESS",
			"webSocketUrl": "/ws/voice-interview/1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := &strings.Builder{}
	opts := &options{mode: "voice", baseURL: srv.URL, timeout: 5 * time.Second, evalWait: 2 * time.Second}
	err := runVoice(opts, out)
	if err == nil {
		t.Fatal("Key 缺失时应失败")
	}
	if !strings.Contains(err.Error(), "ASR API Key 未配置") {
		t.Fatalf("错误信息应指明 ASR Key 未配置，实际: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if created != 0 {
		t.Fatalf("前置检查失败不应创建场次，实际创建 %d", created)
	}
}

func TestRunVoiceFlowStopsOnServerError(t *testing.T) {
	shortenFeedPacing(t)
	// WS 端点开场后下发 error（如 ASR 初始化失败话术）→ 连接与开场白步骤失败即停，
	// 且错误信息透出服务端话术。
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) { okJSON(w, "pong") })
	mux.HandleFunc("/api/llm-provider/voice/asr", func(w http.ResponseWriter, r *http.Request) {
		okJSON(w, map[string]any{"maskedApiKey": "sk-***abc"})
	})
	mux.HandleFunc("/api/llm-provider/voice/tts", func(w http.ResponseWriter, r *http.Request) {
		okJSON(w, map[string]any{"maskedApiKey": "sk-***abc"})
	})
	mux.HandleFunc("/api/voice-interview/sessions", func(w http.ResponseWriter, r *http.Request) {
		okJSON(w, map[string]any{"sessionId": 7, "roleType": "x", "currentPhase": "TECH",
			"status": "IN_PROGRESS", "webSocketUrl": "/ws/voice-interview/7"})
	})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux.HandleFunc("/ws/voice-interview/7", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"type": "control", "action": "welcome",
			"message": "连接成功，准备开始语音面试", "timestamp": 1})
		_ = conn.WriteJSON(map[string]any{"type": "error",
			"message": "初始化语音识别失败: bad credentials"})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out := &strings.Builder{}
	opts := &options{mode: "voice", baseURL: srv.URL, timeout: 5 * time.Second, evalWait: 2 * time.Second}
	err := runVoice(opts, out)
	if err == nil {
		t.Fatal("服务端下发 error 时应失败即停")
	}
	if !strings.Contains(err.Error(), "初始化语音识别失败") {
		t.Fatalf("错误信息应透出服务端话术，实际: %v", err)
	}
}


