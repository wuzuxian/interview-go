// LLM 回合管道测试：封闭式管道（spec 测试决策）——真网关 × stub 流模型（eino
// schema.Pipe）× 进程内假 DashScope TTS 服务端 × 内存 store。覆盖：正常轮完成
// 顺序契约（subtitle final → text final → 落库 → 音频）、清洗截断、单句失败跳句、
// 全句失败整段回退、分块开关关闭走整段、LLM 错误话术与会话不中断、回声冷却、
// 历史与简历进 prompt、答案回填最近未答问题。
package voiceinterview

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/wuzuxian/interview/internal/interview"
	"github.com/wuzuxian/interview/internal/resume"
)

// ===== 替身 =====

// voiceConfigStub 静态语音配置源（ASR 假服务端 + TTS 假服务端地址）。
type voiceConfigStub struct {
	asr AsrSettings
	tts TtsSettings
}

func (s voiceConfigStub) VoiceAsrSettings(context.Context) (AsrSettings, error) { return s.asr, nil }
func (s voiceConfigStub) VoiceTtsSettings(context.Context) (TtsSettings, error) { return s.tts, nil }

// stubVoiceResolver 流式模型解析替身：按入队顺序逐次给出模型（末尾复用）。
type stubVoiceResolver struct {
	mu    sync.Mutex
	queue []StreamChatModel
	asked []string
}

func (r *stubVoiceResolver) VoiceStreamModelFor(_ context.Context, providerID string) (StreamChatModel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, providerID)
	if len(r.queue) == 0 {
		return nil, errString("无可用模型")
	}
	m := r.queue[0]
	if len(r.queue) > 1 {
		r.queue = r.queue[1:]
	}
	return m, nil
}

func (r *stubVoiceResolver) askCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asked)
}

// stubResumeSource 简历文本来源替身。
type stubResumeSource struct {
	text string
}

func (s stubResumeSource) FindByID(_ context.Context, id int64) (*resume.ResumeEntity, error) {
	t := s.text
	return &resume.ResumeEntity{ID: id, ResumeText: &t}, nil
}

// ===== 夹具 =====

// voicePersona 夹具方向内容。
var voicePersona = "# Java 后端面试官\n围绕并发、JVM 与 Spring 提问。"

func voiceTurnSession() *SessionEntity {
	e := inProgressSession(time.Hour)
	skill := "java-backend"
	e.SkillID = &skill
	return e
}

// pipelineFixture 管道测试夹具（真网关 + 真 TTS/ASR 客户端 + stub 流模型）。
type pipelineFixture struct {
	f      svcFixture
	asr    *fakeAsrServer
	tts    *fakeTtsServer
	models *stubVoiceResolver
	pipe   *TurnPipeline
	gw     *Gateway
	srv    *httptest.Server
}

// newPipelineFixture 组装完整管道；mutate 可改配置（如关闭分块开关）。
func newPipelineFixture(t *testing.T, mutate func(cfg *Config)) *pipelineFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := newFixture()
	asr := newFakeAsrServer(t)
	tts := newFakeTtsServer(t)
	models := &stubVoiceResolver{}

	cfg := Config{
		AsrReadyTimeout:            300 * time.Millisecond,
		AsrReadyRetries:            2,
		AsrRestartPause:            10 * time.Millisecond,
		AsrRestartReadyTimeout:     150 * time.Millisecond,
		AsrResendMaxAttempts:       15,
		AsrResendInterval:          10 * time.Millisecond,
		EchoCooldown:               60 * time.Millisecond,
		ChunkedAudioEnabled:        true,
		AIQuestionMaxChars:         120,
		LLMStreamPushInterval:      80 * time.Millisecond,
		LLMStreamMinCharsDelta:     12,
		MaxConcurrentTTSPerSession: 3,
		TTSConnectTimeout:          300 * time.Millisecond,
		TTSTimeout:                 400 * time.Millisecond,
		TTSFullTimeout:             800 * time.Millisecond,
		WSMessageLimit:             262144,
		WSWriteTimeout:             2 * time.Second,
		WSSendBufferLimit:          524288,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	asrSettings := AsrSettings{URL: asr.wsURL(), Model: "qwen3-asr-flash-realtime", APIKey: "sk-test", Format: "pcm", SampleRate: 16000, Language: "zh"}
	ttsSettings := TtsSettings{URL: tts.URL(), Model: "qwen3-tts-flash-realtime", APIKey: "sk-test", Voice: "Cherry", Format: "pcm", SampleRate: 24000, Mode: "commit"}

	pipe := NewTurnPipeline(TurnPipelineDeps{
		Svc:      f.svc,
		Store:    f.store,
		Models:   models,
		Settings: voiceConfigStub{asr: asrSettings, tts: ttsSettings},
		TTS:      NewTtsClient(cfg, discardLogger()),
		Prompts:  NewPromptService(interview.NewPromptSanitizer(nil), stubSkillPersona{persona: &voicePersona}, discardLogger()),
		Resumes:  stubResumeSource{text: "候选人简历：五年 Java 后端经验"},
		Cfg:      cfg,
		Logger:   discardLogger(),
	})

	gw := NewGateway(GatewayDeps{
		Service:        f.svc,
		Settings:       voiceConfigStub{asr: asrSettings, tts: ttsSettings},
		Config:         cfg,
		AllowedOrigins: []string{"https://front.test"},
		Turn:           pipe.Turn,
		Logger:         discardLogger(),
	})
	r := gin.New()
	gw.RegisterRoutes(r.Group("/ws"))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &pipelineFixture{f: f, asr: asr, tts: tts, models: models, pipe: pipe, gw: gw, srv: srv}
}

// ttsSettings 夹具的 TTS 运行时配置（直连管道测试用）。
func (pf *pipelineFixture) ttsSettings() TtsSettings {
	return TtsSettings{URL: pf.tts.URL(), Model: "qwen3-tts-flash-realtime", APIKey: "sk-test",
		Voice: "Cherry", Format: "pcm", SampleRate: 24000, Mode: "commit"}
}

// addMidwayTurnSession 添加「对话进行中」的会话并预置一条历史消息（与
// ws_gateway_test 的 addMidwaySession 同构）：抑制建连自动开场（票 05 合并引入）
// 对回合管道消息流与落库断言的干扰。种子行 user 已填、ai 为空，不触发答案回填。
func (pf *pipelineFixture) addMidwayTurnSession() int64 {
	id := pf.f.store.addSession(voiceTurnSession())
	pf.f.store.setDialogues(id, []MessageEntity{voiceMsg(id, 1, nil, stringPtr("此前的问题"))})
	return id
}

func (pf *pipelineFixture) mustWS(t *testing.T, sessionID int64) *wsClient {
	t.Helper()
	u := "ws" + strings.TrimPrefix(pf.srv.URL, "http") + "/ws/voice-interview/" + strconv.FormatInt(sessionID, 10)
	conn, resp, err := websocket.DefaultDialer.Dial(u, http.Header{"Origin": []string{"https://front.test"}})
	if err != nil {
		t.Fatalf("建连失败: %v (resp=%v)", err, resp)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := newWSClient(conn)
	c.drainEstablish(t)
	return c
}

// submit 以修正文本提交用户话语（绕过 ASR 合并缓冲）。
func submitText(t *testing.T, c *wsClient, text string) {
	t.Helper()
	sendWSMsg(t, c.conn, map[string]any{
		"type": "control", "action": "submit", "data": map[string]any{"text": text},
	})
}

// collectUntilComplete 收集下行直至 audio_complete 或超时（超时返回已收消息，
// 由调用方断言给出差异）。
func collectUntilComplete(t *testing.T, c *wsClient, timeout time.Duration) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var out []map[string]any
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return out
		}
		m := c.next(remain)
		if m == nil {
			return out
		}
		out = append(out, m)
		if m["type"] == "control" && m["action"] == "audio_complete" {
			return out
		}
	}
}

// assertSequence 断言消息类型序列（忽略夹带的其他消息类型）。text-partial 帧
// （节流部分帧与流末的优化全文推送）是时钟相关的，其形状由 llm_stream_test
// 覆盖，此处过滤后断言骨架序列。
func assertSequence(t *testing.T, msgs []map[string]any, want []string) {
	t.Helper()
	var got []string
	for _, m := range msgs {
		switch m["type"] {
		case "subtitle":
			if isFinal, _ := m["isFinal"].(bool); isFinal {
				got = append(got, "subtitle-final")
			} else {
				got = append(got, "subtitle-partial")
			}
		case "text":
			if final, _ := m["final"].(bool); final {
				got = append(got, "text-final")
			} else {
				got = append(got, "text-partial")
			}
		case "audio_chunk":
			got = append(got, "audio_chunk")
		case "audio":
			got = append(got, "audio")
		case "control":
			if m["action"] == "audio_complete" {
				got = append(got, "audio_complete")
			}
		}
	}
	filtered := got[:0]
	for _, g := range got {
		if g != "text-partial" {
			filtered = append(filtered, g)
		}
	}
	if len(filtered) != len(want) {
		t.Fatalf("消息序列不符:\n got  %v\n want %v", filtered, want)
	}
	for i := range want {
		if filtered[i] != want[i] {
			t.Fatalf("第 %d 个消息不符:\n got  %v\n want %v", i, filtered, want)
		}
	}
}

// finalText 取 text(final) 消息的 content。
func finalText(t *testing.T, msgs []map[string]any) string {
	t.Helper()
	for _, m := range msgs {
		if m["type"] == "text" {
			if isFinal, _ := m["final"].(bool); isFinal {
				s, _ := m["content"].(string)
				return s
			}
		}
	}
	t.Fatalf("未收到 text(final) 消息: %v", msgs)
	return ""
}

// firstOf 取第一条指定类型的消息。
func firstOf(t *testing.T, msgs []map[string]any, typ string) map[string]any {
	t.Helper()
	for _, m := range msgs {
		if m["type"] == typ {
			return m
		}
	}
	t.Fatalf("未收到 %s 消息: %v", typ, msgs)
	return nil
}

// ===== 正常轮 =====

// 契约（Java triggerLlmResponse L625-748 流式+分块路径）：submit → 流式文本（无
// 节流帧时仅 final）→ subtitle(final，用户话语) → text(final，完整回复) → 逐句
// audio_chunk（按句序）→ audio_complete；落库 DIALOGUE 一行（用户话语 + AI 回复
// + 阶段；序号 = 抑制开场的种子行 1 条 + 本回合 = 2）。
func TestPipeline正常轮完成顺序与落库(t *testing.T) {
	pf := newPipelineFixture(t, nil)
	pf.tts.textScripts = map[string]func(*fakeTtsServer, *websocket.Conn){
		"你好。":          scriptSynthDone([]byte{1, 1}),
		"请介绍一下你的项目经历。": scriptSynthDone([]byte{2, 2}),
	}
	pf.models.queue = []StreamChatModel{
		&scriptedStreamModel{tokens: []string{"你好。", "请介绍一下你的项目经历。"}},
	}
	id := pf.addMidwayTurnSession()
	c := pf.mustWS(t, id)

	submitText(t, c, "我叫张三，做 Java 五年了")
	msgs := collectUntilComplete(t, c, 5*time.Second)
	assertSequence(t, msgs, []string{"subtitle-final", "text-final", "audio_chunk", "audio_chunk", "audio_complete"})

	// subtitle(final) 携带用户话语；text(final) 携带完整回复。
	sub := firstOf(t, msgs, "subtitle")
	if sub["text"] != "我叫张三，做 Java 五年了" {
		t.Fatalf("subtitle(final) 应为用户话语: %v", sub)
	}
	if finalText(t, msgs) != "你好。请介绍一下你的项目经历。" {
		t.Fatalf("text(final) 应为完整回复: %v", firstOf(t, msgs, "text"))
	}
	chunks := []map[string]any{}
	for _, m := range msgs {
		if m["type"] == "audio_chunk" {
			chunks = append(chunks, m)
		}
	}
	if idx, _ := chunks[0]["index"].(float64); int(idx) != 0 {
		t.Fatalf("首块 index 应为 0: %v", chunks[0])
	}
	if idx, _ := chunks[1]["index"].(float64); int(idx) != 1 {
		t.Fatalf("次块 index 应为 1: %v", chunks[1])
	}

	// 落库：种子行之外本回合新增一行 DIALOGUE，用户话语 + AI 回复 + 阶段 TECH
	// + 序号 2（种子 1 条 + 本回合）。
	rows := pf.f.store.dialogues[id]
	if len(rows) != 2 {
		t.Fatalf("应落库种子行+回合行共 2 行: %d", len(rows))
	}
	row := rows[1]
	if row.MessageType != MessageDialogue {
		t.Fatalf("消息类型应为 DIALOGUE: %v", row.MessageType)
	}
	if row.UserRecognizedText == nil || *row.UserRecognizedText != "我叫张三，做 Java 五年了" {
		t.Fatalf("用户话语未落库: %v", row.UserRecognizedText)
	}
	if row.AiGeneratedText == nil || *row.AiGeneratedText != "你好。请介绍一下你的项目经历。" {
		t.Fatalf("AI 回复未落库: %v", row.AiGeneratedText)
	}
	if row.Phase == nil || *row.Phase != PhaseTech {
		t.Fatalf("阶段应取会话当前阶段 TECH: %v", row.Phase)
	}
	if row.SequenceNum == nil || *row.SequenceNum != 2 {
		t.Fatalf("序号应为 2（种子 1 条 + 本回合）: %v", row.SequenceNum)
	}
}

// 完成顺序契约（直连管道 × recordingSpeaker × 与落库共用同一事件记录器）：
// subtitle(final) → text(final) → save-dialogue → audio_chunk → audio_complete。
func TestPipelineTurn顺序契约含落库在音频前(t *testing.T) {
	f := newFixture()
	tts := newFakeTtsServer(t)
	tts.textScripts = map[string]func(*fakeTtsServer, *websocket.Conn){
		"第一句。": scriptSynthDone([]byte{1}),
		"第二句。": scriptSynthDone([]byte{2}),
	}
	models := &stubVoiceResolver{queue: []StreamChatModel{
		&scriptedStreamModel{tokens: []string{"第一句。", "第二句。"}},
	}}
	cfg := Config{
		ChunkedAudioEnabled: true, AIQuestionMaxChars: 120,
		LLMStreamPushInterval: 80 * time.Millisecond, LLMStreamMinCharsDelta: 12,
		MaxConcurrentTTSPerSession: 3, TTSConnectTimeout: 300 * time.Millisecond,
		TTSTimeout: 400 * time.Millisecond, TTSFullTimeout: 800 * time.Millisecond,
	}
	pipe := NewTurnPipeline(TurnPipelineDeps{
		Svc: f.svc, Store: f.store, Models: models,
		Settings: voiceConfigStub{tts: TtsSettings{URL: tts.URL(), Model: "m", APIKey: "k", Mode: "commit"}},
		TTS:      NewTtsClient(cfg, discardLogger()),
		Prompts:  NewPromptService(interview.NewPromptSanitizer(nil), stubSkillPersona{persona: &voicePersona}, discardLogger()),
		Cfg:      cfg, Logger: discardLogger(),
	})
	id := f.store.addSession(voiceTurnSession())

	speaker := &recordingSpeaker{rec: f.rec}
	pipe.Turn(context.Background(), TurnInput{SessionID: id, UserText: "我的回答", Speaker: speaker})

	// 过滤出下行与落库事件（会话/历史读路径的 cache-get/find 不参与顺序契约）。
	var got []string
	for _, e := range f.rec.list() {
		if strings.HasPrefix(e, "send:") || e == "save-dialogue" {
			got = append(got, e)
		}
	}
	// 完成顺序契约（Java L712-715）：subtitle(final) → text(final) → 落库 → 音频；
	// 前置的 text:partial 是流末的优化全文推送（Java onToken(optimized) 逐字）。
	want := []string{
		"send:text:partial", "send:subtitle:final", "send:text:final", "save-dialogue",
		"send:audio_chunk:0", "send:audio_chunk:1", "send:audio_complete",
	}
	if len(got) != len(want) {
		t.Fatalf("事件序列不符:\n got  %v\n want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个事件不符:\n got  %v\n want %v", i, got, want)
		}
	}
}

// ===== 清洗与截断 =====

// 清洗发生在出句前：Markdown/空白不出现在句子与最终文本；超 120 字截断到句界。
func TestPipeline清洗截断到句界(t *testing.T) {
	pf := newPipelineFixture(t, nil)
	// 121 字：句界在第 81 字（≥ 120/2），截断应保留到该句号。
	long := strings.Repeat("甲", 80) + "。" + strings.Repeat("乙", 40)
	pf.models.queue = []StreamChatModel{&scriptedStreamModel{tokens: []string{"**" + long + "**"}}}
	pf.tts.textScripts = map[string]func(*fakeTtsServer, *websocket.Conn){
		long: scriptSynthDone([]byte{1}),
	}
	id := pf.addMidwayTurnSession()
	c := pf.mustWS(t, id)

	submitText(t, c, "请出题")
	msgs := collectUntilComplete(t, c, 5*time.Second)
	assertSequence(t, msgs, []string{"subtitle-final", "text-final", "audio_chunk", "audio_complete"})

	final := finalText(t, msgs)
	runes := []rune(final)
	if len(runes) > 120 {
		t.Fatalf("final 文本应不超 120 字: %d", len(runes))
	}
	if final != strings.Repeat("甲", 80)+"。" {
		t.Fatalf("截断应保留到句界（81 字）: 长度 %d", len(runes))
	}
	if strings.Contains(final, "**") {
		t.Fatalf("final 文本不应含 Markdown 标记: %q", final)
	}
	// 清洗发生在出句前：TTS 收到的是去 Markdown 后的整句（未做 120 截断，Java 逐字）。
	appended := pf.tts.AppendedText()
	if appended != long {
		t.Fatalf("TTS 应收到清洗后的完整句: %q", appended)
	}
}

// ===== 失败路径 =====

// 单句 TTS 失败跳过：第 2 句合成失败 → 仍按句序下发 0/2 两块 + audio_complete。
func TestPipeline单句失败跳句(t *testing.T) {
	pf := newPipelineFixture(t, nil)
	pf.tts.textScripts = map[string]func(*fakeTtsServer, *websocket.Conn){
		"第一句。": scriptSynthDone([]byte{1}),
		"第二句。": scriptErrorEvent("ServerError", "InternalError", "合成失败"),
		"第三句。": scriptSynthDone([]byte{3}),
	}
	pf.models.queue = []StreamChatModel{&scriptedStreamModel{tokens: []string{"第一句。", "第二句。", "第三句。"}}}
	id := pf.addMidwayTurnSession()
	c := pf.mustWS(t, id)

	submitText(t, c, "继续")
	msgs := collectUntilComplete(t, c, 5*time.Second)
	assertSequence(t, msgs, []string{"subtitle-final", "text-final", "audio_chunk", "audio_chunk", "audio_complete"})

	idxs := []int{}
	for _, m := range msgs {
		if m["type"] == "audio_chunk" {
			idx, _ := m["index"].(float64)
			idxs = append(idxs, int(idx))
		}
	}
	if len(idxs) != 2 || idxs[0] != 0 || idxs[1] != 2 {
		t.Fatalf("失败句应跳过，块序应为 0、2: %v", idxs)
	}
}

// 全句失败回退整段：句子 TTS 全部失败 → 无 audio_chunk、无 audio_complete，
// 以单条 audio（整段文本兜底合成）下发（Java L723-734）。
func TestPipeline全句失败整段回退(t *testing.T) {
	pf := newPipelineFixture(t, nil)
	pf.tts.textScripts = map[string]func(*fakeTtsServer, *websocket.Conn){
		"第一句。": scriptErrorEvent("ServerError", "InternalError", "失败一"),
		"第二句。": scriptErrorEvent("ServerError", "InternalError", "失败二"),
	}
	pf.tts.scripts = []func(*fakeTtsServer, *websocket.Conn){
		scriptSynthDone([]byte{9, 9}), // 整段兜底（文本不命中 textScripts 时走连接序）
	}
	pf.models.queue = []StreamChatModel{&scriptedStreamModel{tokens: []string{"第一句。", "第二句。"}}}
	id := pf.addMidwayTurnSession()
	c := pf.mustWS(t, id)

	submitText(t, c, "继续")
	// 兜底走 SynthesizeFull（800ms 超时预算），无 audio_complete 可等：按内容断言。
	deadline := time.Now().Add(5 * time.Second)
	var msgs []map[string]any
	for time.Now().Before(deadline) {
		m := c.next(500 * time.Millisecond)
		if m == nil {
			continue
		}
		msgs = append(msgs, m)
		if m["type"] == "audio" {
			break
		}
	}
	assertSequence(t, msgs, []string{"subtitle-final", "text-final", "audio"})

	audio := firstOf(t, msgs, "audio")
	if audio["text"] != "第一句。第二句。" {
		t.Fatalf("兜底 audio 应携带完整回复文本: %v", audio)
	}
	data, _ := audio["data"].(string)
	wav, _ := base64.StdEncoding.DecodeString(data)
	if len(wav) != 44+2 {
		t.Fatalf("兜底 WAV 应为整段 PCM 加头: %d bytes", len(wav))
	}
	// 落库仍完成（音频失败不影响消息）：种子行 + 兜底回合行共 2 行。
	if rows := pf.f.store.dialogues[id]; len(rows) != 2 {
		t.Fatalf("兜底路径也应落库（种子行+回合行共 2 行）: %d", len(rows))
	}
}

// 空回复（零句子）→ 兜底整段合成「请继续。」单条 audio。
func TestPipeline空回复整段兜底(t *testing.T) {
	pf := newPipelineFixture(t, nil)
	pf.tts.scripts = []func(*fakeTtsServer, *websocket.Conn){scriptSynthDone([]byte{7})}
	pf.models.queue = []StreamChatModel{&scriptedStreamModel{}}
	id := pf.addMidwayTurnSession()
	c := pf.mustWS(t, id)

	submitText(t, c, "（静音）")
	deadline := time.Now().Add(5 * time.Second)
	var msgs []map[string]any
	for time.Now().Before(deadline) {
		m := c.next(500 * time.Millisecond)
		if m == nil {
			continue
		}
		msgs = append(msgs, m)
		if m["type"] == "audio" {
			break
		}
	}
	assertSequence(t, msgs, []string{"subtitle-final", "text-final", "audio"})
	if finalText(t, msgs) != "请继续。" {
		t.Fatalf("空回复 final 文本应为「请继续。」: %v", firstOf(t, msgs, "text"))
	}
}

// 分块开关关闭走整段：不出现 audio_chunk / audio_complete，单条 audio。
func TestPipeline分块关闭走整段(t *testing.T) {
	pf := newPipelineFixture(t, func(cfg *Config) { cfg.ChunkedAudioEnabled = false })
	pf.tts.textScripts = map[string]func(*fakeTtsServer, *websocket.Conn){
		"第一句。": scriptSynthDone([]byte{5}),
		"第二句。": scriptSynthDone([]byte{5}),
	}
	pf.models.queue = []StreamChatModel{&scriptedStreamModel{tokens: []string{"第一句。", "第二句。"}}}
	id := pf.addMidwayTurnSession()
	c := pf.mustWS(t, id)

	submitText(t, c, "继续")
	deadline := time.Now().Add(5 * time.Second)
	var msgs []map[string]any
	for time.Now().Before(deadline) {
		m := c.next(500 * time.Millisecond)
		if m == nil {
			continue
		}
		msgs = append(msgs, m)
		if m["type"] == "audio" {
			break
		}
	}
	assertSequence(t, msgs, []string{"subtitle-final", "text-final", "audio"})
	data, _ := firstOf(t, msgs, "audio")["data"].(string)
	wav, _ := base64.StdEncoding.DecodeString(data)
	if len(wav) != 44+2 || wav[44] != 5 || wav[45] != 5 {
		t.Fatalf("整段 WAV 应为两句 PCM 按序合并: %d bytes", len(wav))
	}
}

// LLM 错误：四类话术 error 下发、无落库、会话保持进行中、处理锁释放（下一轮可用）。
func TestPipelineLLM错误话术与会话不中断(t *testing.T) {
	pf := newPipelineFixture(t, nil)
	pf.models.queue = []StreamChatModel{
		&scriptedStreamModel{err: errString("Error code: 403 - ACCESS_DENIED")},
		&scriptedStreamModel{tokens: []string{"好的。", "继续。"}},
	}
	pf.tts.textScripts = map[string]func(*fakeTtsServer, *websocket.Conn){
		"好的。": scriptSynthDone([]byte{1}),
		"继续。": scriptSynthDone([]byte{2}),
	}
	id := pf.addMidwayTurnSession()
	c := pf.mustWS(t, id)

	submitText(t, c, "第一问的回答")
	c.waitErrorText(t, "AI 服务认证失败，请检查 API Key 配置", 5*time.Second)

	// 无新增落库（仅剩抑制开场的种子行）、会话未结束。
	if rows := pf.f.store.dialogues[id]; len(rows) != 1 {
		t.Fatalf("错误轮不应新增落库（应仅剩种子行）: %v", rows)
	}
	if e := pf.f.store.sessions[id]; e == nil || e.Status == nil || *e.Status != VoiceStatusInProgress {
		t.Fatalf("会话应保持进行中: %+v", pf.f.store.sessions[id])
	}

	// 处理锁已释放：越过回声冷却后下一轮正常。
	time.Sleep(150 * time.Millisecond)
	submitText(t, c, "第二问的回答")
	msgs := collectUntilComplete(t, c, 5*time.Second)
	assertSequence(t, msgs, []string{"subtitle-final", "text-final", "audio_chunk", "audio_chunk", "audio_complete"})
}

// LLM 流中途失败（分块路径、已有一句提交发射器）：error 话术下发后 Turn 提前
// return，发射协程必须随句序通道关闭而退出——否则永久阻塞在 for range e.jobs，
// 每遇一次流失败/断连即泄漏一个 goroutine。
func TestPipeline流中途失败发射协程退出(t *testing.T) {
	f := newFixture()
	tts := newFakeTtsServer(t)
	tts.textScripts = map[string]func(*fakeTtsServer, *websocket.Conn){
		"第一句。": scriptSynthDone([]byte{1}),
	}
	models := &stubVoiceResolver{queue: []StreamChatModel{
		&scriptedStreamModel{tokens: []string{"第一句。"}, errAfter: errString("流中途失败")},
	}}
	cfg := Config{
		ChunkedAudioEnabled: true, AIQuestionMaxChars: 120,
		LLMStreamPushInterval: 80 * time.Millisecond, LLMStreamMinCharsDelta: 12,
		MaxConcurrentTTSPerSession: 3, TTSConnectTimeout: 300 * time.Millisecond,
		TTSTimeout: 400 * time.Millisecond, TTSFullTimeout: 800 * time.Millisecond,
	}
	pipe := NewTurnPipeline(TurnPipelineDeps{
		Svc: f.svc, Store: f.store, Models: models,
		Settings: voiceConfigStub{tts: TtsSettings{URL: tts.URL(), Model: "m", APIKey: "k", Mode: "commit"}},
		TTS:      NewTtsClient(cfg, discardLogger()),
		Prompts:  NewPromptService(interview.NewPromptSanitizer(nil), stubSkillPersona{persona: &voicePersona}, discardLogger()),
		Cfg:      cfg, Logger: discardLogger(),
	})
	id := f.store.addSession(voiceTurnSession())

	before := runtime.NumGoroutine()
	speaker := newRecordingSpeaker()
	pipe.Turn(context.Background(), TurnInput{SessionID: id, UserText: "回答", Speaker: speaker})

	// 契约（票 06）：LLM 异常映射话术下发 error，会话不中断。
	got := speaker.rec.list()
	if len(got) == 0 || !strings.HasPrefix(got[0], "send:error:") {
		t.Fatalf("流失败应下发 error 话术: %v", got)
	}
	// 发射协程已退出（含消费已提交句子的合成协程）。
	waitForCond(t, 3*time.Second, "发射协程已退出", func() bool {
		return runtime.NumGoroutine() <= before
	})
}

// AI 说话期间回声冷却：回合执行中上行音频被丢弃（不转发 ASR），回合结束后恢复。
func TestPipelineAI说话期间回声冷却生效(t *testing.T) {
	pf := newPipelineFixture(t, nil)
	block := make(chan struct{})
	pf.models.queue = []StreamChatModel{&scriptedStreamModel{tokens: []string{"好的。"}, block: block}}
	id := pf.addMidwayTurnSession()
	c := pf.mustWS(t, id)

	submitText(t, c, "回答")
	waitForCond(t, 3*time.Second, "模型回合已开始", func() bool { return pf.models.askCount() == 1 })

	pcm := []byte{1, 2, 3, 4}
	sendWSMsg(t, c.conn, audioFrame(t, pcm))
	time.Sleep(200 * time.Millisecond)
	if got := pf.asr.appendPayloads(); len(got) != 0 {
		t.Fatalf("AI 说话期间音频应被丢弃: %v", got)
	}

	close(block)
	collectUntilComplete(t, c, 5*time.Second)

	// 冷却窗口（60ms）过后音频恢复转发。
	time.Sleep(120 * time.Millisecond)
	sendWSMsg(t, c.conn, audioFrame(t, pcm))
	waitForCond(t, 3*time.Second, "冷却结束后音频恢复转发", func() bool {
		return len(pf.asr.appendPayloads()) == 1
	})
}

// 会话不存在：error 话术逐字（Java triggerLlmResponse L655）。
func TestPipeline会话不存在话术(t *testing.T) {
	f := newFixture()
	cfg := Config{ChunkedAudioEnabled: true, AIQuestionMaxChars: 120,
		LLMStreamPushInterval: 80 * time.Millisecond, LLMStreamMinCharsDelta: 12}
	pipe := NewTurnPipeline(TurnPipelineDeps{
		Svc: f.svc, Store: f.store, Models: &stubVoiceResolver{},
		TTS:     NewTtsClient(cfg, discardLogger()),
		Prompts: NewPromptService(interview.NewPromptSanitizer(nil), nil, discardLogger()),
		Cfg:     cfg, Logger: discardLogger(),
	})
	speaker := newRecordingSpeaker()
	pipe.Turn(context.Background(), TurnInput{SessionID: 99999, UserText: "回答", Speaker: speaker})
	got := speaker.rec.list()
	if len(got) != 1 || got[0] != "send:error:会话不存在，请重新开始面试" {
		t.Fatalf("应仅下发会话不存在话术: %v", got)
	}
}

// ===== prompt 组装贯通 =====

// 历史与简历进 prompt：system 含方向内容与简历（边界标签包裹），user 含
// 【之前的对话】逐行与【当前对话】。
func TestPipeline历史与简历进prompt(t *testing.T) {
	f := newFixture()
	tts := newFakeTtsServer(t)
	tts.textScripts = map[string]func(*fakeTtsServer, *websocket.Conn){
		"好的。": scriptSynthDone([]byte{1}),
	}
	model := &scriptedStreamModel{tokens: []string{"好的。"}}
	models := &stubVoiceResolver{queue: []StreamChatModel{model}}
	cfg := Config{ChunkedAudioEnabled: true, AIQuestionMaxChars: 120,
		LLMStreamPushInterval: 80 * time.Millisecond, LLMStreamMinCharsDelta: 12,
		TTSConnectTimeout: 300 * time.Millisecond, TTSTimeout: 400 * time.Millisecond,
		TTSFullTimeout: 800 * time.Millisecond, MaxConcurrentTTSPerSession: 3}
	pipe := NewTurnPipeline(TurnPipelineDeps{
		Svc: f.svc, Store: f.store, Models: models,
		Settings: voiceConfigStub{tts: TtsSettings{URL: tts.URL(), Model: "m", APIKey: "k", Mode: "commit"}},
		TTS:      NewTtsClient(cfg, discardLogger()),
		Prompts:  NewPromptService(interview.NewPromptSanitizer(nil), stubSkillPersona{persona: &voicePersona}, discardLogger()),
		Resumes:  stubResumeSource{text: "候选人简历：五年 Java 后端经验"},
		Cfg:      cfg, Logger: discardLogger(),
	})

	// 历史两轮：第一问已答，第二问未答。
	ai1, user1, ai2 := "第一问内容", "答一", "第二问内容"
	seq1, seq2 := 1, 2
	entity := voiceTurnSession()
	resumeID := int64(5)
	entity.ResumeID = &resumeID
	id := f.store.addSession(entity)
	f.store.setDialogues(id, []MessageEntity{
		{SessionID: id, MessageType: MessageDialogue, AiGeneratedText: &ai1, UserRecognizedText: &user1, SequenceNum: &seq1},
		{SessionID: id, MessageType: MessageDialogue, AiGeneratedText: &ai2, SequenceNum: &seq2},
	})

	pipe.Turn(context.Background(), TurnInput{SessionID: id, UserText: "答二", Speaker: newRecordingSpeaker()})

	inputs := model.lastInput()
	if len(inputs) != 2 {
		t.Fatalf("应为 system+user 两条: %d", len(inputs))
	}
	sys, user := inputs[0].Content, inputs[1].Content

	if !strings.Contains(sys, voicePersona) {
		t.Errorf("system 应含方向内容")
	}
	if !strings.Contains(sys, "候选人简历：五年 Java 后端经验") || !strings.Contains(sys, "-resume>") {
		t.Errorf("system 应含边界标签包裹的简历")
	}
	for _, want := range []string{"【之前的对话】", "面试官：第一问内容", "候选人：答一", "面试官：第二问内容", "【当前对话】", "用户："} {
		if !strings.Contains(user, want) {
			t.Errorf("user 应含 %q: %s", want, user)
		}
	}
	// 未答问题行（第二问内容）挂起为本轮格式，历史中不出现「候选人：答二」——
	// 答二只以本轮输入（边界标签包裹）出现在【当前对话】。
	if strings.Contains(user, "候选人：答二") {
		t.Errorf("历史不应包含本轮回答: %s", user)
	}
}

// 历史超预算走压缩装配（票 07 接 06 取历史点）：管道注入压缩加载器后，长历史被
// 有界装配为「摘要行 + 最近窗口」，prompt 不再含早期原文；摘要由独立的普通模型
// 生成（非流式 resolver），写回由加载器负责。
func TestPipeline历史超预算走压缩装配(t *testing.T) {
	f := newFixture()
	tts := newFakeTtsServer(t)
	tts.textScripts = map[string]func(*fakeTtsServer, *websocket.Conn){
		"好的。": scriptSynthDone([]byte{1}),
	}
	model := &scriptedStreamModel{tokens: []string{"好的。"}}
	models := &stubVoiceResolver{queue: []StreamChatModel{model}}
	summary := &stubPlainCompleter{content: "这是早期摘要"}
	// 小窗口小批次便于构造触发：window=2、batch=1、预算充足。
	cc := ContextCompressionConfig{Enabled: true, Mode: ModeSummary, WindowSize: 2,
		SummaryBatchSize: 1, MaxHistoryChars: 12000, MaxSummaryChars: 4000}
	cfg := Config{ChunkedAudioEnabled: true, AIQuestionMaxChars: 120,
		LLMStreamPushInterval: 80 * time.Millisecond, LLMStreamMinCharsDelta: 12,
		TTSConnectTimeout: 300 * time.Millisecond, TTSTimeout: 400 * time.Millisecond,
		TTSFullTimeout: 800 * time.Millisecond, MaxConcurrentTTSPerSession: 3}
	pipe := NewTurnPipeline(TurnPipelineDeps{
		Svc: f.svc, Store: f.store, Models: models,
		Settings: voiceConfigStub{tts: TtsSettings{URL: tts.URL(), Model: "m", APIKey: "k", Mode: "commit"}},
		TTS:      NewTtsClient(cfg, discardLogger()),
		Prompts:  NewPromptService(interview.NewPromptSanitizer(nil), stubSkillPersona{persona: &voicePersona}, discardLogger()),
		Cfg:      cfg, Logger: discardLogger(),
		// 压缩加载器复用同一内存 store（fakeCompStore 包装补齐压缩链路四方法）。
		History: NewHistoryLoader(
			&fakeCompStore{fakeStore: f.store, savedSummaries: map[int64]savedSummary{}},
			NewCompressor(cc, summary, discardLogger()), cc, discardLogger()),
	})

	// 预置 5 轮历史（seq 1..5）：最近窗口取 seq4,5，待摘要批取 (0,4) 升序最早的
	// 1 条 = seq1（Java PageRequest.of(0, batchSize) 语义）。
	id := f.store.addSession(voiceTurnSession())
	msgs := make([]MessageEntity, 0, 5)
	for i := 1; i <= 5; i++ {
		q := fmt.Sprintf("问%d内容", i)
		a := fmt.Sprintf("答%d", i)
		msgs = append(msgs, voiceMsg(id, i, &q, &a))
	}
	f.store.setDialogues(id, msgs)

	pipe.Turn(context.Background(), TurnInput{SessionID: id, UserText: "答六", Speaker: newRecordingSpeaker()})

	inputs := model.lastInput()
	if len(inputs) != 2 {
		t.Fatalf("应为 system+user 两条: %d", len(inputs))
	}
	user := inputs[1].Content
	if !strings.Contains(user, "【对话摘要】这是早期摘要") {
		t.Errorf("prompt 应含压缩摘要行: %s", user)
	}
	if !strings.Contains(user, "面试官：问4内容") || !strings.Contains(user, "候选人：答5") {
		t.Errorf("prompt 应含最近窗口原文: %s", user)
	}
	if strings.Contains(user, "问1内容") || strings.Contains(user, "问2内容") || strings.Contains(user, "问3内容") {
		t.Errorf("已摘要与窗口外原文不应进 prompt: %s", user)
	}
	// 摘要由独立普通模型生成，且只喂待摘要批（seq1）。
	prompts := summary.promptList()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "面试官：问1内容") || strings.Contains(prompts[0], "问4") {
		t.Errorf("摘要模型应恰好调用一次且只含待摘要批: %v", prompts)
	}
	// 压缩链路读历史不影响回合完成契约：模型回复照常进入流式收尾（文本 final 已发）。
}

// ===== 落库语义 =====

// 答案回填最近未答问题（Java fillLatestUnansweredQuestion）：既有未答问题行 →
// 回填到旧行、新行 userRecognizedText 为空；无未答问题 → 新行携带用户话语。
func TestSaveTurnMessage答案回填语义(t *testing.T) {
	t.Run("回填最近未答问题", func(t *testing.T) {
		f := newFixture()
		ai1, user1, ai2 := "第一问", "答一", "第二问"
		seq1, seq2 := 1, 2
		id := f.store.addSession(voiceTurnSession())
		f.store.setDialogues(id, []MessageEntity{
			{SessionID: id, MessageType: MessageDialogue, AiGeneratedText: &ai1, UserRecognizedText: &user1, SequenceNum: &seq1},
			{SessionID: id, MessageType: MessageDialogue, AiGeneratedText: &ai2, SequenceNum: &seq2},
		})

		f.svc.SaveTurnMessage(context.Background(), id, "答二内容", "追问内容")

		rows := f.store.dialogues[id]
		if len(rows) != 3 {
			t.Fatalf("应新增一行: %d", len(rows))
		}
		if rows[1].UserRecognizedText == nil || *rows[1].UserRecognizedText != "答二内容" {
			t.Fatalf("第二问行应回填答案: %+v", rows[1])
		}
		if rows[2].UserRecognizedText != nil {
			t.Fatalf("新行答案应随回填置空: %+v", rows[2])
		}
		if rows[2].AiGeneratedText == nil || *rows[2].AiGeneratedText != "追问内容" {
			t.Fatalf("新行应携带 AI 回复: %+v", rows[2])
		}
		if rows[2].SequenceNum == nil || *rows[2].SequenceNum != 3 {
			t.Fatalf("新行序号应为 3: %v", rows[2].SequenceNum)
		}
	})

	t.Run("无未答问题时新行携带用户话语", func(t *testing.T) {
		f := newFixture()
		id := f.store.addSession(voiceTurnSession())
		f.svc.SaveTurnMessage(context.Background(), id, "第一句回答", "好的追问")
		rows := f.store.dialogues[id]
		if len(rows) != 1 {
			t.Fatalf("应落库一行: %d", len(rows))
		}
		if rows[0].UserRecognizedText == nil || *rows[0].UserRecognizedText != "第一句回答" {
			t.Fatalf("新行应携带用户话语: %+v", rows[0])
		}
	})

	t.Run("会话不存在仅告警不落库", func(t *testing.T) {
		f := newFixture()
		f.svc.SaveTurnMessage(context.Background(), 424242, "回答", "回复")
		if rows := f.store.dialogues[424242]; len(rows) != 0 {
			t.Fatalf("会话缺失不应落库: %v", rows)
		}
	})
}
