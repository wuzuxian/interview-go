// WS 网关管道测试（spec 测试决策的封闭式管道例外）：httptest 起真网关（gin +
// gorilla + 真实网关代码）× 进程内假 DashScope ASR 服务端（真协议客户端打假端点）
// × 内存三件套（fakeStore/fakeCache/fakeStream，复用 service_test.go 夹具）。
// 全程无真 DB、无外网；只断言外部行为：下发消息序列、ASR 收到的帧与落库状态。
//
// 注意（gorilla 语义）：SetReadDeadline 超时后连接即损坏、后续读全部报错
// （conn.go L1101-1104），故下行消息由常驻读协程收进队列，断言按超时从队列取用，
// 从不对连接本身设读超时。
package voiceinterview

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// ===== 夹具 =====

// staticConfigSource 静态语音配置源（测试注入，URL 指向进程内假 ASR/TTS 服务端）。
type staticConfigSource struct {
	asr AsrSettings
	tts TtsSettings
}

func (s staticConfigSource) VoiceAsrSettings(context.Context) (AsrSettings, error) {
	return s.asr, nil
}

func (s staticConfigSource) VoiceTtsSettings(context.Context) (TtsSettings, error) {
	return s.tts, nil
}

// turnRecorder 回合挂钩记录器：block 非 nil 时挂起 hook 模拟模型回合执行中。
type turnRecorder struct {
	mu     sync.Mutex
	inputs []TurnInput
	block  chan struct{}
}

func (r *turnRecorder) hook(_ context.Context, in TurnInput) {
	r.mu.Lock()
	r.inputs = append(r.inputs, in)
	block := r.block
	r.mu.Unlock()
	if block != nil {
		<-block
	}
}

func (r *turnRecorder) calls() []TurnInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]TurnInput(nil), r.inputs...)
}

func (r *turnRecorder) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.block != nil {
		close(r.block)
		r.block = nil
	}
}

// gatewayFixture 管道测试夹具。
type gatewayFixture struct {
	f       svcFixture
	asr     *fakeAsrServer
	ttsSrv  *fakeTtsServer
	turn    *turnRecorder
	gw      *Gateway
	srv     *httptest.Server
	origins []string
}

// newGatewayFixture 组装真网关 + 假 ASR/TTS + 内存存储；mutate 可改配置与白名单。
// 假 TTS 默认脚本为一段两字节 PCM 的正常合成（开场白等合成都走真协议客户端）。
func newGatewayFixture(t *testing.T, mutate func(cfg *Config, origins *[]string)) *gatewayFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := newFixture()
	asr := newFakeAsrServer(t)
	ttsSrv := newFakeTtsServer(t)
	ttsSrv.scripts = []func(*fakeTtsServer, *websocket.Conn){scriptSynthDone([]byte{0x01, 0x02})}
	turn := &turnRecorder{}
	origins := []string{"https://front.test"}
	cfg := Config{
		AsrReadyTimeout:        300 * time.Millisecond,
		AsrReadyRetries:        2,
		AsrRestartPause:        10 * time.Millisecond,
		AsrRestartReadyTimeout: 150 * time.Millisecond,
		AsrResendMaxAttempts:   15,
		AsrResendInterval:      10 * time.Millisecond,
		EchoCooldown:           60 * time.Millisecond,
		TTSConnectTimeout:      time.Second,
		TTSTimeout:             2 * time.Second,
		TTSFullTimeout:         3 * time.Second,
		WSMessageLimit:         262144,
		WSWriteTimeout:         2 * time.Second,
		WSSendBufferLimit:      524288,
	}
	if mutate != nil {
		mutate(&cfg, &origins)
	}
	gw := NewGateway(GatewayDeps{
		Service: f.svc,
		Settings: staticConfigSource{
			asr: AsrSettings{
				URL: asr.wsURL(), Model: "qwen3-asr-flash-realtime", APIKey: "sk-test",
				Format: "pcm", SampleRate: 16000, Language: "zh", EnableTurnDetection: true,
				TurnDetectionType: "server_vad", TurnDetectionSilenceDurationMs: 2000,
			},
			tts: TtsSettings{
				URL: ttsSrv.URL(), Model: "qwen3-tts-flash-realtime", APIKey: "sk-test",
				Voice: "Cherry", Format: "pcm", SampleRate: 24000, Mode: "commit",
				LanguageType: "Chinese", SpeechRate: 1, Volume: 60,
			},
		},
		Config:         cfg,
		AllowedOrigins: origins,
		Turn:           turn.hook,
		Logger:         discardLogger(),
	})
	r := gin.New()
	gw.RegisterRoutes(r.Group("/ws"))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &gatewayFixture{f: f, asr: asr, ttsSrv: ttsSrv, turn: turn, gw: gw, srv: srv, origins: origins}
}

// addMidwaySession 添加一个"对话进行中"的会话（含一条历史消息）：用于与开场白
// 无关的管道测试，抑制建连自动开场对消息流的干扰。
func (gf *gatewayFixture) addMidwaySession(startAgo time.Duration) int64 {
	id := gf.f.store.addSession(inProgressSession(startAgo))
	gf.f.store.setDialogues(id, []MessageEntity{voiceMsg(id, 1, stringPtr("此前的问题"), nil)})
	return id
}

// dialWS 建立语音 WS 连接（非 101 升级时返回响应供状态码断言）。
func (gf *gatewayFixture) dialWS(t *testing.T, sessionID int64, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(gf.srv.URL, "http") + "/ws/voice-interview/" + strconv.FormatInt(sessionID, 10)
	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return websocket.DefaultDialer.Dial(u, header)
}

// mustWSClient 以白名单内来源建连并启动常驻下行收集协程。
func (gf *gatewayFixture) mustWSClient(t *testing.T, sessionID int64) *wsClient {
	t.Helper()
	conn, resp, err := gf.dialWS(t, sessionID, "https://front.test")
	if err != nil {
		t.Fatalf("建连失败: %v (resp=%v)", err, resp)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return newWSClient(conn)
}

// wsClient 下行消息收集器：常驻读协程把消息收进队列，断言按超时从队列取。
type wsClient struct {
	conn   *websocket.Conn
	ch     chan map[string]any
	closed chan struct{}
	once   sync.Once
}

func newWSClient(conn *websocket.Conn) *wsClient {
	c := &wsClient{
		conn:   conn,
		ch:     make(chan map[string]any, 128),
		closed: make(chan struct{}),
	}
	go func() {
		defer close(c.closed)
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				continue
			}
			c.ch <- m
		}
	}()
	return c
}

// next 取下一条下行消息；超时或连接关闭返回 nil。关闭与最后一条下行消息几乎同时
// 到达时（如暂停超时提示后紧跟断连），先取已入队消息再认定关闭。
func (c *wsClient) next(timeout time.Duration) map[string]any {
	select {
	case m := <-c.ch:
		return m
	default:
	}
	select {
	case m := <-c.ch:
		return m
	case <-c.closed:
		select {
		case m := <-c.ch:
			return m
		default:
			return nil
		}
	case <-time.After(timeout):
		return nil
	}
}

// assertSilent 断言窗口内无任何下行消息。
func (c *wsClient) assertSilent(t *testing.T, timeout time.Duration) {
	t.Helper()
	if m := c.next(timeout); m != nil {
		t.Fatalf("窗口内不应有下行消息: %v", m)
	}
}

// nextOfAction 取下一条指定 action 的 control 消息（跳过其他消息）。
func (c *wsClient) nextOfAction(t *testing.T, action string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			t.Fatalf("超时未收到 action=%s 的 control 消息", action)
		}
		m := c.next(remain)
		if m == nil {
			t.Fatalf("超时未收到 action=%s 的 control 消息（连接可能已关闭）", action)
		}
		if m["type"] == "control" && m["action"] == action {
			return m
		}
	}
}

// nextSubtitle 取下一条 subtitle 消息。
func (c *wsClient) nextSubtitle(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			t.Fatalf("超时未收到 subtitle 消息")
		}
		m := c.next(remain)
		if m == nil {
			t.Fatalf("超时未收到 subtitle 消息（连接可能已关闭）")
		}
		if m["type"] == "subtitle" {
			return m
		}
	}
}

// waitErrorText 轮询至出现指定话术的 error 消息（跳过其他消息）。
func (c *wsClient) waitErrorText(t *testing.T, text string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			t.Fatalf("超时未收到 error 消息: %s", text)
		}
		m := c.next(remain)
		if m == nil {
			t.Fatalf("超时未收到 error 消息（连接可能已关闭）: %s", text)
		}
		if m["type"] == "error" && m["message"] == text {
			return
		}
	}
}

// drainEstablish 消费建连期消息，返回收到的 action 集（应含 welcome 与 asr_ready）。
func (c *wsClient) drainEstablish(t *testing.T) []string {
	t.Helper()
	var actions []string
	for _, want := range []string{"welcome", "asr_ready"} {
		m := c.nextOfAction(t, want, 3*time.Second)
		actions = append(actions, m["action"].(string))
	}
	return actions
}

func sendWSMsg(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	if err := conn.WriteJSON(v); err != nil {
		t.Fatalf("发送上行消息失败: %v", err)
	}
}

// waitForCond 轮询断言（deadline 内条件成立，否则失败）。
func waitForCond(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func audioFrame(t *testing.T, pcm []byte) map[string]any {
	t.Helper()
	return map[string]any{"type": "audio", "data": base64.StdEncoding.EncodeToString(pcm)}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ===== 建连 =====

// 契约（Java afterConnectionEstablished L149-179 + createWelcomeMessage/sendAsrReady）：
// 建连校验会话存在与 IN_PROGRESS；welcome 与 asr_ready 控制消息形状逐字。
func TestGateway建连下发welcome与asr_ready(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id := gf.addMidwaySession(time.Hour)
	c := gf.mustWSClient(t, id)

	actions := c.drainEstablish(t)
	if !containsStr(actions, "welcome") || !containsStr(actions, "asr_ready") {
		t.Fatalf("建连应收到 welcome 与 asr_ready，实际: %v", actions)
	}
	c.assertSilent(t, 300*time.Millisecond)
}

// 建连消息原文形状（welcome/asr_ready 键集与话术）逐字核对。
func TestGateway建连消息形状逐字(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)

	welcome := c.nextOfAction(t, "welcome", 3*time.Second)
	asrReady := c.nextOfAction(t, "asr_ready", 3*time.Second)

	assertMsgShape(t, mustJSON(t, welcome),
		[]string{"action", "message", "timestamp", "type"},
		map[string]any{"type": "control", "action": "welcome", "message": "连接成功，准备开始语音面试"})
	assertMsgShape(t, mustJSON(t, asrReady),
		[]string{"action", "message", "timestamp", "type"},
		map[string]any{"type": "control", "action": "asr_ready", "message": "语音识别已就绪"})
	if ts, ok := welcome["timestamp"].(float64); !ok || ts <= 0 {
		t.Errorf("welcome.timestamp 应为正毫秒数: %v", welcome["timestamp"])
	}
}

// 建连校验：会话不存在 404；非 IN_PROGRESS 409（票面要求，Java 原版缺校验的补齐）。
func TestGateway拒绝不存在的会话与非进行中状态(t *testing.T) {
	gf := newGatewayFixture(t, nil)

	_, resp, err := gf.dialWS(t, 99999, "https://front.test")
	if err == nil {
		t.Fatal("不存在的会话应拒绝升级")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("应返回 404，实际: %v", resp)
	}

	done := VoiceStatusCompleted
	ended := inProgressSession(time.Hour)
	ended.Status = &done
	completedID := gf.f.store.addSession(ended)
	_, resp, err = gf.dialWS(t, completedID, "https://front.test")
	if err == nil {
		t.Fatal("非进行中会话应拒绝升级")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("应返回 409，实际: %v", resp)
	}
}

// 升级来源校验沿 CORS 白名单（Java setAllowedOrigins）：白名单外 403、
// 白名单内放行、无 Origin（非浏览器）放行、白名单为空放行全部。
func TestGateway升级来源校验(t *testing.T) {
	t.Run("白名单外拒绝", func(t *testing.T) {
		gf := newGatewayFixture(t, nil)
		id := gf.f.store.addSession(inProgressSession(time.Hour))
		_, resp, err := gf.dialWS(t, id, "https://evil.test")
		if err == nil {
			t.Fatal("白名单外来源应拒绝升级")
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("应返回 403，实际: %v", resp)
		}
	})
	t.Run("白名单内放行", func(t *testing.T) {
		gf := newGatewayFixture(t, nil)
		id := gf.f.store.addSession(inProgressSession(time.Hour))
		c := gf.mustWSClient(t, id)
		c.drainEstablish(t)
	})
	t.Run("无Origin放行", func(t *testing.T) {
		gf := newGatewayFixture(t, nil)
		id := gf.f.store.addSession(inProgressSession(time.Hour))
		conn, resp, err := gf.dialWS(t, id, "")
		if err != nil {
			t.Fatalf("无 Origin（非浏览器客户端）应放行: %v (resp=%v)", err, resp)
		}
		_ = conn.Close()
	})
	t.Run("白名单为空放行全部", func(t *testing.T) {
		gf := newGatewayFixture(t, func(cfg *Config, origins *[]string) { *origins = nil })
		id := gf.f.store.addSession(inProgressSession(time.Hour))
		conn, resp, err := gf.dialWS(t, id, "https://anything.test")
		if err != nil {
			t.Fatalf("空白名单应放行全部来源: %v (resp=%v)", err, resp)
		}
		_ = conn.Close()
	})
}

// ===== 上行音频 → ASR =====

// 上行 audio 帧解码转发 ASR（Java handleUserAudio）；ASR 未就绪前静默丢弃。
func TestGateway上行音频转发ASR(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	sendWSMsg(t, c.conn, audioFrame(t, pcm))
	waitForCond(t, 3*time.Second, "假 ASR 收到音频帧", func() bool {
		return len(gf.asr.appendPayloads()) == 1
	})
	got, err := base64.StdEncoding.DecodeString(gf.asr.appendPayloads()[0])
	if err != nil {
		t.Fatalf("假 ASR 收到的 audio 应为 base64: %v", err)
	}
	if string(got) != string(pcm) {
		t.Errorf("ASR 收到字节与上行不一致: %v", got)
	}
}

// ASR 未就绪期间音频帧静默丢弃（假服务端挂起握手，SendAudio 等待 1200ms 后
// 返回 not-ready → 丢块，连接与会话不受影响）。
func TestGatewayASR未就绪期间音频静默丢弃(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	gf.asr.stallHandshake = true
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)

	// 只会收到 welcome（ASR 永不就绪，无 asr_ready；就绪监督 300ms×3 后才有 error，
	// 此处不等待）。
	m := c.nextOfAction(t, "welcome", 3*time.Second)
	if m["action"] != "welcome" {
		t.Fatalf("应先收到 welcome: %v", m)
	}

	// 音频帧：SendAudio 最多等 1200ms → not ready → 丢弃；随后连接仍可收发。
	sendWSMsg(t, c.conn, audioFrame(t, []byte{9, 9, 9}))
	time.Sleep(1400 * time.Millisecond)
	if got := gf.asr.appendPayloads(); len(got) != 0 {
		t.Errorf("未就绪期间音频应被丢弃，实际转发: %v", got)
	}
	sendWSMsg(t, c.conn, map[string]any{"type": "control", "action": "end_interview"})
	waitForCond(t, 3*time.Second, "连接仍存活且 end_interview 落库", func() bool {
		return len(gf.f.store.updateEnd) == 1
	})
}

// ===== 字幕两级下发与多段拼接 =====

// 部分 → subtitle(isFinal=false)；最终 → 拼入合并缓冲并下发合并后字幕
// （isFinal 同为 false：Java handleSttResult L567/576，isFinal=true 由模型回合
// 提交后下发——票面「最终→isFinal=true」与 Java/前端行为相悖，按 Java 对齐，
// 见票 04 汇报 deviations）。
func TestGateway两级字幕下发与多段拼接(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	// 部分级。
	gf.asr.push(0, map[string]any{"type": asrEvtTransText, "text": "今天天气"})
	sub := c.nextSubtitle(t, 2*time.Second)
	if sub["text"] != "今天天气" || sub["isFinal"] != false {
		t.Fatalf("部分字幕错误: %v", sub)
	}

	// 最终级一：入缓冲，字幕为缓冲预览（isFinal=false）。
	gf.asr.push(0, map[string]any{"type": asrEvtTransCompleted, "transcript": "今天天气不错"})
	sub = c.nextSubtitle(t, 2*time.Second)
	if sub["text"] != "今天天气不错" || sub["isFinal"] != false {
		t.Fatalf("最终字幕一错误: %v", sub)
	}

	// 最终级二：逗号拼接（joinSegments）。
	gf.asr.push(0, map[string]any{"type": asrEvtTransCompleted, "transcript": "我们开始吧"})
	sub = c.nextSubtitle(t, 2*time.Second)
	if sub["text"] != "今天天气不错，我们开始吧" || sub["isFinal"] != false {
		t.Fatalf("多段拼接字幕错误: %v", sub)
	}

	// 识别自我修正：后段包含前段 → 取后段。
	gf.asr.push(0, map[string]any{"type": asrEvtTransCompleted, "transcript": "今天天气不错，我们开始吧，请回答"})
	sub = c.nextSubtitle(t, 2*time.Second)
	if sub["text"] != "今天天气不错，我们开始吧，请回答" || sub["isFinal"] != false {
		t.Fatalf("前缀修正拼接错误: %v", sub)
	}

	// 最终结果不自动触发模型。
	time.Sleep(200 * time.Millisecond)
	if calls := gf.turn.calls(); len(calls) != 0 {
		t.Fatalf("最终结果不应自动触发模型回合: %v", calls)
	}
}

// ===== submit：挂钩、修正覆盖、CAS 防双答 =====

// submit 取走合并缓冲触发回合挂钩；data.text 覆盖缓冲（Java handleControl submit 分支）。
func TestGatewaysubmit触发挂钩与修正文本覆盖(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	gf.asr.push(0, map[string]any{"type": asrEvtTransCompleted, "transcript": "识别出的话语"})
	c.nextSubtitle(t, 2*time.Second) // 消费字幕

	sendWSMsg(t, c.conn, map[string]any{"type": "control", "action": "submit"})
	waitForCond(t, 2*time.Second, "submit 触发挂钩", func() bool { return len(gf.turn.calls()) == 1 })
	if got := gf.turn.calls()[0].UserText; got != "识别出的话语" {
		t.Errorf("挂钩应收到合并缓冲文本: %q", got)
	}
	if got := gf.turn.calls()[0].SessionID; got != id {
		t.Errorf("挂钩 SessionID 错误: %d", got)
	}

	// 第二回合：越过回声冷却（夹具 60ms）后 final，再携带 data.text 提交 → 覆盖缓冲。
	time.Sleep(150 * time.Millisecond)
	gf.asr.push(0, map[string]any{"type": asrEvtTransCompleted, "transcript": "口误文本"})
	c.nextSubtitle(t, 2*time.Second)
	sendWSMsg(t, c.conn, map[string]any{
		"type": "control", "action": "submit", "data": map[string]any{"text": "修正后的回答"},
	})
	waitForCond(t, 2*time.Second, "修正文本触发挂钩", func() bool { return len(gf.turn.calls()) == 2 })
	if got := gf.turn.calls()[1].UserText; got != "修正后的回答" {
		t.Errorf("data.text 应覆盖合并缓冲: %q", got)
	}

	// 空缓冲 submit 不触发（Java：userText 空白 → 释放处理锁返回）。
	time.Sleep(150 * time.Millisecond) // 越过冷却
	sendWSMsg(t, c.conn, map[string]any{"type": "control", "action": "submit"})
	time.Sleep(300 * time.Millisecond)
	if calls := gf.turn.calls(); len(calls) != 2 {
		t.Errorf("空缓冲 submit 不应触发挂钩: %v", calls)
	}
}

// CAS 处理锁防双答：回合执行中重复 submit，待处理锁释放后重试发现缓冲已空，
// 不产生第二次回合（Java flushMergedUtteranceToLlm compareAndSet + 400ms 重试）。
func TestGateway处理中CAS防双答(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	gf.turn.block = make(chan struct{})
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	gf.asr.push(0, map[string]any{"type": asrEvtTransCompleted, "transcript": "问题A"})
	c.nextSubtitle(t, 2*time.Second)

	sendWSMsg(t, c.conn, map[string]any{"type": "control", "action": "submit"})
	sendWSMsg(t, c.conn, map[string]any{"type": "control", "action": "submit"})
	waitForCond(t, 2*time.Second, "首个回合进入挂钩", func() bool { return len(gf.turn.calls()) == 1 })

	gf.turn.release()
	// 等待超过 400ms 重试窗口：第二次 submit 应以空缓冲结束，不再触发回合。
	time.Sleep(700 * time.Millisecond)
	if calls := gf.turn.calls(); len(calls) != 1 {
		t.Errorf("同一话语不得双答: %v", calls)
	}

	// 处理锁已释放：新话语可再次提交（越过冷却）。
	time.Sleep(120 * time.Millisecond)
	gf.asr.push(0, map[string]any{"type": asrEvtTransCompleted, "transcript": "问题B"})
	c.nextSubtitle(t, 2*time.Second)
	sendWSMsg(t, c.conn, map[string]any{"type": "control", "action": "submit"})
	waitForCond(t, 2*time.Second, "锁释放后可提交新话语", func() bool { return len(gf.turn.calls()) == 2 })
	if got := gf.turn.calls()[1].UserText; got != "问题B" {
		t.Errorf("第二回合文本错误: %q", got)
	}
}

// ===== 回声冷却 =====

// AI 说话（回合挂钩执行中）与结束后冷却窗口内丢弃上行音频（Java
// isAiSpeakingOrCooldown）；窗口恢复后转发。
func TestGateway回声冷却丢弃音频(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	gf.turn.block = make(chan struct{})
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	gf.asr.push(0, map[string]any{"type": asrEvtTransCompleted, "transcript": "问题"})
	c.nextSubtitle(t, 2*time.Second)
	sendWSMsg(t, c.conn, map[string]any{"type": "control", "action": "submit"})
	waitForCond(t, 2*time.Second, "回合进入挂钩（AI 说话中）", func() bool { return len(gf.turn.calls()) == 1 })

	// AI 说话期间：音频被丢弃。
	sendWSMsg(t, c.conn, audioFrame(t, []byte{1}))
	time.Sleep(250 * time.Millisecond)
	if got := gf.asr.appendPayloads(); len(got) != 0 {
		t.Fatalf("AI 说话期间音频应被丢弃: %v", got)
	}

	gf.turn.release()
	// 冷却窗口内（cfg.EchoCooldown=60ms）：立即发的帧仍被丢弃。
	sendWSMsg(t, c.conn, audioFrame(t, []byte{2}))
	time.Sleep(150 * time.Millisecond)
	if got := gf.asr.appendPayloads(); len(got) != 0 {
		t.Fatalf("回声冷却窗口内音频应被丢弃: %v", got)
	}

	// 冷却过后：转发。
	time.Sleep(120 * time.Millisecond)
	sendWSMsg(t, c.conn, audioFrame(t, []byte{3}))
	waitForCond(t, 3*time.Second, "冷却后音频转发", func() bool {
		return len(gf.asr.appendPayloads()) == 1
	})
}

// 处理中/AI 说话期间迟到的识别结果被丢弃，不污染下一轮字幕（Java handleSttResult
// L559-563）。
func TestGateway处理中丢弃迟到识别结果(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	gf.turn.block = make(chan struct{})
	id := gf.addMidwaySession(time.Hour)
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	gf.asr.push(0, map[string]any{"type": asrEvtTransCompleted, "transcript": "问题"})
	c.nextSubtitle(t, 2*time.Second)
	sendWSMsg(t, c.conn, map[string]any{"type": "control", "action": "submit"})
	waitForCond(t, 2*time.Second, "回合进入挂钩", func() bool { return len(gf.turn.calls()) == 1 })

	// 迟到的部分/最终结果：无任何字幕下发。
	gf.asr.push(0, map[string]any{"type": asrEvtTransText, "text": "迟到的部分"})
	gf.asr.push(0, map[string]any{"type": asrEvtTransCompleted, "transcript": "迟到的最终"})
	c.assertSilent(t, 400*time.Millisecond)

	gf.turn.release()
	time.Sleep(150 * time.Millisecond) // 越过冷却窗口
	gf.asr.push(0, map[string]any{"type": asrEvtTransText, "text": "新一轮"})
	sub := c.nextSubtitle(t, 2*time.Second)
	if sub["text"] != "新一轮" {
		t.Fatalf("下一轮字幕不应被迟到结果污染: %v", sub)
	}
}

// ===== ASR 失效：自动重启 + 当前块重发 =====

// 连接失效后下一次推流触发重启（02 的重启语义）并重发当前块；重发成功无感恢复。
func TestGatewayASR失效自动重启并重发当前块(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	chunk1 := []byte{1, 1, 1, 1}
	sendWSMsg(t, c.conn, audioFrame(t, chunk1))
	waitForCond(t, 3*time.Second, "首块转发", func() bool { return len(gf.asr.appendPayloads()) == 1 })

	// 服务端断开当前 ASR 连接；等客户端读泵清理会话（实例比对移除）。
	gf.asr.closeConn(0, 1011, "boom")
	time.Sleep(300 * time.Millisecond)

	chunk2 := []byte{2, 2, 2, 2}
	sendWSMsg(t, c.conn, audioFrame(t, chunk2))
	// 读泵移除失效会话后，chunk2 走「无会话 → 重启 → 重发」路径：服务端共收到
	// chunk1（旧连接）+ chunk2（新连接重发）两帧。
	waitForCond(t, 5*time.Second, "重启后重发当前块", func() bool {
		return len(gf.asr.appendPayloads()) == 2
	})
	appends := gf.asr.appendPayloads()
	if appends[0] != base64.StdEncoding.EncodeToString(chunk1) {
		t.Errorf("首块内容错误: %v", appends)
	}
	if appends[1] != base64.StdEncoding.EncodeToString(chunk2) {
		t.Errorf("当前块应在新连接上重发: %v", appends)
	}
	if got := gf.asr.connCount(); got < 2 {
		t.Errorf("应建立新的 ASR 连接，实际连接数: %d", got)
	}
}

// 重发耗尽（此处配置 3 次 × 5ms，假服务端持续拒绝握手）→ 下发 error 且会话不断
// （Java L530-533：sendError 后不结束会话）。
func TestGateway重发耗尽下发错误且会话不断(t *testing.T) {
	gf := newGatewayFixture(t, func(cfg *Config, _ *[]string) {
		cfg.AsrResendMaxAttempts = 3
		cfg.AsrResendInterval = 5 * time.Millisecond
	})
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	gf.asr.rejectHandshake = true
	gf.asr.closeConn(0, 1011, "boom")
	time.Sleep(300 * time.Millisecond)

	sendWSMsg(t, c.conn, audioFrame(t, []byte{7, 7, 7}))
	c.waitErrorText(t, "语音识别连接中断，请刷新页面后重试", 5*time.Second)

	// 会话不断：连接仍可收发控制消息。
	sendWSMsg(t, c.conn, map[string]any{"type": "control", "action": "end_interview"})
	waitForCond(t, 3*time.Second, "会话仍可结束", func() bool {
		return len(gf.f.store.updateEnd) == 1
	})
}

// 就绪监督：ASR 建连慢时下发 asr_reconnecting，重试耗尽下发「连接准备超时」error
// （Java checkAsrReadyOrRetry L449-461；客户端 300ms×2 次重试的测试配置）。
func TestGateway建连慢推送asr_reconnecting与超时错误(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	gf.asr.stallHandshake = true
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)

	m := c.nextOfAction(t, "welcome", 3*time.Second)
	if m["action"] != "welcome" {
		t.Fatalf("应先收到 welcome: %v", m)
	}

	// asr_reconnecting（300ms 后第一轮）。
	m = c.nextOfAction(t, "asr_reconnecting", 3*time.Second)
	if m["message"] != "语音识别连接较慢，正在自动重连" {
		t.Fatalf("asr_reconnecting 话术错误: %v", m)
	}

	// 重试耗尽（2 次后）→ 超时 error。
	c.waitErrorText(t, "语音识别连接准备超时，请检查语音服务配置或稍后重试", 8*time.Second)
}

// ===== 结束语义 =====

// end_interview → 复用票 01 结束语义：COMPLETED + 评估 PENDING + 投递（Java
// handleControl end_interview → interviewService.endSession）。
func TestGatewayend_interview结束会话并投递评估(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	sendWSMsg(t, c.conn, map[string]any{"type": "control", "action": "end_interview"})
	waitForCond(t, 3*time.Second, "end_interview 落库", func() bool {
		return len(gf.f.store.updateEnd) == 1
	})

	e := gf.f.store.updateEnd[0]
	if statusEnum(e.Status) != VoiceStatusCompleted {
		t.Errorf("结束应置 COMPLETED: %v", e.Status)
	}
	if e.EndTime == nil {
		t.Error("结束应写 endTime")
	}
	if e.EvaluateStatus == nil || *e.EvaluateStatus != VoiceEvaluatePending {
		t.Errorf("结束应置评估 PENDING: %v", e.EvaluateStatus)
	}
	waitForCond(t, 2*time.Second, "评估任务已投递", func() bool {
		gf.f.stream.mu.Lock()
		defer gf.f.stream.mu.Unlock()
		return len(gf.f.stream.adds) == 1
	})
	task := gf.f.stream.addAt(0)
	if task["voiceSessionId"] != strconv.FormatInt(id, 10) {
		t.Errorf("投递的会话 ID 错误: %v", task["voiceSessionId"])
	}
}

// 异常断开 → 停 ASR + 自动结束进行中的会话并投递评估（Java afterConnectionClosed
// L373-400）；随后再建连被 409 拒绝。
func TestGateway断开自动结束会话并停ASR(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	_ = c.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(1000, "client bye"), time.Now().Add(time.Second))
	_ = c.conn.Close()

	waitForCond(t, 3*time.Second, "断开后自动结束会话", func() bool {
		return len(gf.f.store.updateEnd) == 1
	})
	e := gf.f.store.updateEnd[0]
	if statusEnum(e.Status) != VoiceStatusCompleted {
		t.Errorf("断开应置 COMPLETED: %v", e.Status)
	}
	if e.EvaluateStatus == nil || *e.EvaluateStatus != VoiceEvaluatePending {
		t.Errorf("断开应置评估 PENDING: %v", e.EvaluateStatus)
	}
	waitForCond(t, 2*time.Second, "断开后评估已投递", func() bool {
		gf.f.stream.mu.Lock()
		defer gf.f.stream.mu.Unlock()
		return len(gf.f.stream.adds) == 1
	})
	waitForCond(t, 2*time.Second, "ASR 已停止（session.finish）", func() bool {
		return gf.asr.finishTotal() >= 1
	})

	// 已结束的会话再建连：409。
	_, resp, err := gf.dialWS(t, id, "https://front.test")
	if err == nil {
		t.Fatal("已结束会话应拒绝重建连")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("应返回 409，实际: %v", resp)
	}
}

// start_phase 透传会话域（Java handleControl start_phase → interviewService.startPhase）。
func TestGatewaystart_phase切换阶段(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	sendWSMsg(t, c.conn, map[string]any{
		"type": "control", "action": "start_phase", "phase": "HR",
	})
	waitForCond(t, 2*time.Second, "阶段切换落库", func() bool {
		return len(gf.f.store.updatePhase) == 1
	})
	if got := gf.f.store.updatePhase[0]; got.id != id || got.phase != PhaseHR {
		t.Errorf("start_phase 参数错误: %+v", got)
	}

	// 非法阶段仅记日志不改状态（Java startPhase 捕获 valueOf 异常只 log）。
	sendWSMsg(t, c.conn, map[string]any{
		"type": "control", "action": "start_phase", "phase": "NOT_A_PHASE",
	})
	time.Sleep(200 * time.Millisecond)
	if len(gf.f.store.updatePhase) != 1 {
		t.Errorf("非法阶段不应落库: %v", gf.f.store.updatePhase)
	}
}

// ===== 上行消息上限 =====

// 单条上行超过 256KB → 连接被关闭（Java setTextMessageSizeLimit(256KB)，超限按
// TOO_BIG 断开），断开路径自动结束会话。
func TestGateway上行超过256KB断连并自动结束(t *testing.T) {
	gf := newGatewayFixture(t, func(cfg *Config, _ *[]string) {
		cfg.WSMessageLimit = 262144
	})
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	big := make([]byte, 220*1024) // base64 后约 293KB，超过 256KB 上限
	sendWSMsg(t, c.conn, audioFrame(t, big))

	// 服务端关闭连接（gorilla 回 1009 close）：常驻读协程随后退出。
	waitForCond(t, 3*time.Second, "超限断开自动结束会话", func() bool {
		return len(gf.f.store.updateEnd) == 1
	})
}

// ===== 未知类型与关闭保护 =====

// 未知上行类型仅告警忽略，连接不断（Java default 分支 log.warn）。
func TestGateway未知上行类型忽略(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id := gf.addMidwaySession(time.Hour)
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	sendWSMsg(t, c.conn, map[string]any{"type": "mystery", "data": "x"})
	c.assertSilent(t, 300*time.Millisecond)
	// 连接仍存活。
	sendWSMsg(t, c.conn, map[string]any{"type": "control", "action": "end_interview"})
	waitForCond(t, 2*time.Second, "连接仍存活", func() bool {
		return len(gf.f.store.updateEnd) == 1
	})
}

// Gateway.Close 优雅关闭：关闭全部活动连接并触发断开收尾（自动结束会话）。
func TestGateway优雅关闭自动结束会话(t *testing.T) {
	gf := newGatewayFixture(t, nil)
	id := gf.f.store.addSession(inProgressSession(time.Hour))
	c := gf.mustWSClient(t, id)
	c.drainEstablish(t)

	gf.gw.Close()
	waitForCond(t, 3*time.Second, "关闭后自动结束会话", func() bool {
		return len(gf.f.store.updateEnd) == 1
	})
}
