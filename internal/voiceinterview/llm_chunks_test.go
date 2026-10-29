// 分句音频发射器单测（Java OrderedTtsChunkEmitter L983-1097 语义）：按句序下发
// audio_chunk、单句失败/超时跳过不阻塞、完成且至少成功一句才发 audio_complete、
// 零成功静默（由管道整段兜底）。
package voiceinterview

import (
	"encoding/base64"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// recordingSpeaker TurnSpeaker 替身：全部下行记入事件序列（与落库共用同一
// svcRecorder 以断言「subtitle final → text final → 落库 → 音频」顺序），
// 并保留完整消息 JSON 供形状断言。
type recordingSpeaker struct {
	rec *svcRecorder

	mu   sync.Mutex
	msgs []map[string]any
}

func (s *recordingSpeaker) record(v any, event string) {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.mu.Unlock()
	if s.rec != nil {
		s.rec.add(event)
	}
}

func (s *recordingSpeaker) SendSubtitle(text string, isFinal bool) {
	s.record(buildSubtitle(text, isFinal), "send:subtitle:"+boolFinal(isFinal))
}

func (s *recordingSpeaker) SendText(content string, final bool) {
	s.record(buildTextMessage(content, final), "send:text:"+boolFinal(final))
}

func (s *recordingSpeaker) SendAudio(wav []byte, text string) {
	s.record(buildAudioMessage(base64.StdEncoding.EncodeToString(wav), text), "send:audio")
}

func (s *recordingSpeaker) SendAudioChunk(wav []byte, index int, isLast bool) {
	s.record(buildAudioChunkMessage(base64.StdEncoding.EncodeToString(wav), index, isLast),
		"send:audio_chunk:"+itoaVoice(index))
}

func (s *recordingSpeaker) SendAudioComplete() {
	s.record(buildAudioComplete(), "send:audio_complete")
}

func (s *recordingSpeaker) SendError(message string) {
	s.record(buildErrorMessage(message), "send:error:"+message)
}

func (s *recordingSpeaker) all() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.msgs...)
}

func boolFinal(b bool) string {
	if b {
		return "final"
	}
	return "partial"
}

func itoaVoice(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// newRecordingSpeaker 自带事件记录器的替身。
func newRecordingSpeaker() *recordingSpeaker { return &recordingSpeaker{rec: &svcRecorder{}} }

// newEmitter 发射器夹具：假 TTS 服务端 + 短超时配置。
func newEmitter(t *testing.T, fs *fakeTtsServer, speaker *recordingSpeaker, timeout time.Duration) *ttsChunkEmitter {
	t.Helper()
	cfg := Config{TTSConnectTimeout: 300 * time.Millisecond, TTSTimeout: timeout}
	client := NewTtsClient(cfg, discardLogger())
	settings := TtsSettings{URL: fs.URL(), Model: "qwen3-tts-flash-realtime", APIKey: "sk-test",
		Voice: "Cherry", Format: "pcm", SampleRate: 24000, Mode: "commit"}
	return newTTSChunkEmitter(ttsEmitterParams{
		SessionID: 1, Speaker: speaker, TTS: client, Settings: settings,
		Timeout: timeout, MaxConcurrency: 3, Logger: discardLogger(),
	})
}

func TestChunkEmitter按句序下发与完成(t *testing.T) {
	fs := newFakeTtsServer(t)
	fs.scripts = []func(*fakeTtsServer, *websocket.Conn){
		scriptSynthDone([]byte{1, 1}),
		scriptSynthDone([]byte{2, 2}),
		scriptSynthDone([]byte{3, 3}),
	}
	speaker := newRecordingSpeaker()
	e := newEmitter(t, fs, speaker, 2*time.Second)

	e.Submit("第一句。")
	e.Submit("第二句。")
	e.Submit("第三句。")
	e.Finish()
	if emitted := e.AwaitCompletion(); emitted != 3 {
		t.Fatalf("应发出 3 块: %d", emitted)
	}

	// 顺序：chunk0 → chunk1 → chunk2 → audio_complete；isLast 恒 false
	// （Java sendAudioChunk 调用逐字），完成信号由 audio_complete 承担。
	want := []string{"send:audio_chunk:0", "send:audio_chunk:1", "send:audio_chunk:2", "send:audio_complete"}
	if got := speaker.rec.list(); len(got) != len(want) {
		t.Fatalf("事件序列不符: %v", got)
	}
	for i := range want {
		if speaker.rec.list()[i] != want[i] {
			t.Fatalf("第 %d 个事件不符: got %v, want %v", i, speaker.rec.list(), want)
		}
	}

	chunks := speaker.all()
	for i, m := range chunks[:3] {
		if m["type"] != "audio_chunk" {
			t.Fatalf("第 %d 条应为 audio_chunk: %v", i, m)
		}
		if m["isLast"] != false {
			t.Fatalf("audio_chunk.isLast 应恒为 false（Java 逐字）: %v", m)
		}
	}
	last := chunks[2]
	data, _ := last["data"].(string)
	wav, _ := base64.StdEncoding.DecodeString(data)
	if len(wav) <= 44 {
		t.Fatalf("末块应含第三句 PCM 的 WAV: %d bytes", len(wav))
	}
	if chunks[3]["type"] != "control" || chunks[3]["action"] != "audio_complete" {
		t.Fatalf("应以 audio_complete 收尾: %v", chunks[3])
	}
}

func TestChunkEmitter单句失败跳过(t *testing.T) {
	fs := newFakeTtsServer(t)
	fs.scripts = []func(*fakeTtsServer, *websocket.Conn){
		scriptSynthDone([]byte{1}),
		scriptErrorEvent("ServerError", "InternalError", "合成失败"),
		scriptSynthDone([]byte{3}),
	}
	speaker := newRecordingSpeaker()
	e := newEmitter(t, fs, speaker, 2*time.Second)

	// 假服务端按连接序取脚本，逐句等连接被接受后再提交下一句，保证脚本映射确定。
	e.Submit("一句。")
	waitForCond(t, 2*time.Second, "第一句连接被接受", func() bool { return fs.ConnectionCount() >= 1 })
	e.Submit("失败句。")
	waitForCond(t, 2*time.Second, "失败句连接被接受", func() bool { return fs.ConnectionCount() >= 2 })
	e.Submit("三句。")
	e.Finish()
	if emitted := e.AwaitCompletion(); emitted != 2 {
		t.Fatalf("失败句应跳过，成功 2 块: %d", emitted)
	}

	want := []string{"send:audio_chunk:0", "send:audio_chunk:2", "send:audio_complete"}
	got := speaker.rec.list()
	if len(got) != len(want) {
		t.Fatalf("事件序列不符: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个事件不符: %v", i, got)
		}
	}
}

func TestChunkEmitter单句超时跳过(t *testing.T) {
	fs := newFakeTtsServer(t)
	fs.scripts = []func(*fakeTtsServer, *websocket.Conn){
		scriptSynthDone([]byte{1}),
		scriptStall([]byte{2}), // 停滞后不回 response.done：超时跳句
		scriptSynthDone([]byte{3}),
	}
	speaker := newRecordingSpeaker()
	e := newEmitter(t, fs, speaker, 300*time.Millisecond)

	start := time.Now()
	e.Submit("一句。")
	waitForCond(t, 2*time.Second, "第一句连接被接受", func() bool { return fs.ConnectionCount() >= 1 })
	e.Submit("卡住句。")
	waitForCond(t, 2*time.Second, "卡住句连接被接受", func() bool { return fs.ConnectionCount() >= 2 })
	e.Submit("三句。")
	e.Finish()
	if emitted := e.AwaitCompletion(); emitted != 2 {
		t.Fatalf("超时句应跳过: %d", emitted)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("单句超时不应阻塞后续句")
	}
	want := []string{"send:audio_chunk:0", "send:audio_chunk:2", "send:audio_complete"}
	got := speaker.rec.list()
	if len(got) != len(want) {
		t.Fatalf("事件序列不符: %v", got)
	}
}

func TestChunkEmitter全部失败零下发(t *testing.T) {
	fs := newFakeTtsServer(t)
	fs.scripts = []func(*fakeTtsServer, *websocket.Conn){
		scriptErrorEvent("ServerError", "InternalError", "失败一"),
		scriptErrorEvent("ServerError", "InternalError", "失败二"),
	}
	speaker := newRecordingSpeaker()
	e := newEmitter(t, fs, speaker, 500*time.Millisecond)

	e.Submit("一句。")
	e.Submit("二句。")
	e.Finish()
	if emitted := e.AwaitCompletion(); emitted != 0 {
		t.Fatalf("全失败应零下发: %d", emitted)
	}
	// 零成功时不发 audio_complete（Java drainChunks 逐字：emitted > 0 才发），
	// 由管道整段兜底补一条完整音频。
	if got := speaker.rec.list(); len(got) != 0 {
		t.Fatalf("零成功不应有任何下发: %v", got)
	}
}

func TestChunkEmitter无句子直接完成零下发(t *testing.T) {
	fs := newFakeTtsServer(t)
	speaker := newRecordingSpeaker()
	e := newEmitter(t, fs, speaker, 500*time.Millisecond)

	e.Finish()
	if emitted := e.AwaitCompletion(); emitted != 0 {
		t.Fatalf("无句子应零下发: %d", emitted)
	}
	if got := speaker.rec.list(); len(got) != 0 {
		t.Fatalf("无句子不应有任何下发: %v", got)
	}
}
