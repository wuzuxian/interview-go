// 语音面试全链路冒烟（票 10）：-mode voice 的实现。链路对齐票面：
//
//	前置检查 voice_config（真实 ASR/TTS Key 缺失即清晰报错）→ 健康检查 →
//	真实创建语音场次 → WebSocket 连接 → 收开场白（welcome / asr_ready /
//	开场白文本 / 真实 TTS 开场音频）→ 开场音频（服务端真实 TTS 合成产物，作为
//	模拟"回答"）线性插值重采样 16kHz → 200ms 帧逐帧喂 ASR → control submit
//	（携带 ASR 字幕作为修正文本，保证回合确定性触发）→ 断言 subtitle(final) /
//	text(final) / audio_chunk / audio_complete → end_interview → 轮询评估至
//	COMPLETED → 报告结构校验 → DELETE 清理（-keep 跳过）。
//
// 步骤化输出与失败即停非零退出沿既有 smoke 风格（票 09）。真实冒烟运行需要
// 真实 DashScope Key，不在门禁要求内；单测覆盖校验逻辑与重采样纯函数。
package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// voice 链路节奏常量。
const (
	// asrSampleRate ASR 上行采样率（Java 前端 AudioContext 16kHz）。
	asrSampleRate = 16000
	// endStatusPollInterval / endStatusPollWait 结束后状态确认节奏。
	endStatusPollInterval = 500 * time.Millisecond
	endStatusPollWait     = 10 * time.Second
)

// voice 链路节奏参数。默认值对齐生产语义；声明为 var 仅为让流程级测试注入
// 极短节奏（不必按真实时长跑 200ms 帧与静默收尾），生产路径不受影响。
var (
	// feedFrameInterval 喂帧间隔 200ms（pcm-processor.js 按 200ms 块实时出帧，
	// 服务端 ASR server_vad 的静默判定依赖真实节奏）。
	feedFrameInterval = 200 * time.Millisecond
	// feedTrailingSilence 音频尾部静音时长 2.5s（触发 ASR 静默收尾，服务端
	// 默认静默判定 2000ms，须留出裕量）。
	feedTrailingSilence = 2500 * time.Millisecond
	// feedFinalizeGrace 喂完后等 ASR 最终结果回传的宽限（静默 2s + 识别延迟）。
	feedFinalizeGrace = 4 * time.Second
)

// wsDownMessage 下行消息的客户端扁平视图：六类消息（control/subtitle/text/
// audio/audio_chunk/error）字段名逐字对齐 internal/voiceinterview/ws_messages.go
//（即 Java VoiceInterviewWebSocketHandler 的 Map.of/Lombok 序列化）。control.message
// 与 error.message 同名，共用一个字段。
type wsDownMessage struct {
	Type    string `json:"type"`
	Action  string `json:"action"`  // control
	Message string `json:"message"` // control / error
	// subtitle
	Text    string `json:"text"`
	IsFinal bool   `json:"isFinal"`
	// text
	Content string `json:"content"`
	Final   bool   `json:"final"`
	// audio / audio_chunk
	Data   string `json:"data"`
	Index  int    `json:"index"`
	IsLast bool   `json:"isLast"`
}

// 上行消息（对齐前端 WebSocket 发送形状；服务端只解 type/action/data）。
type wsUpAudio struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type wsUpControl struct {
	Type   string         `json:"type"`
	Action string         `json:"action"`
	Phase  string         `json:"phase,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
}

// voiceSessionDTO GET/POST /api/voice-interview/sessions 响应中冒烟关心的字段
//（Java dto/SessionResponseDTO 子集；webSocketUrl 为相对路径）。
type voiceSessionDTO struct {
	SessionID    int64  `json:"sessionId"`
	RoleType     string `json:"roleType"`
	CurrentPhase string `json:"currentPhase"`
	Status       string `json:"status"`
	WebSocketURL string `json:"webSocketUrl"`
}

// voiceEvalStatusDTO GET /sessions/{id}/evaluation 响应（Java
// dto/VoiceEvaluationStatusDTO；COMPLETED 时 evaluation 内嵌完整报告，
// 冒烟以原始 JSON 保留做键级校验）。
type voiceEvalStatusDTO struct {
	EvaluateStatus          *string         `json:"evaluateStatus"`
	EvaluateError           *string         `json:"evaluateError"`
	EvaluateStatusUpdatedAt *time.Time      `json:"evaluateStatusUpdatedAt"`
	Evaluation              json.RawMessage `json:"evaluation"`
}

// ===== WS 消息收集器：等待与断言统计的数据面 =====

// wsCollector 汇集服务端下行消息，供各步骤「等待特定消息 + 事后断言统计」。
// Record 由读协程同步调用；waitFor 先扫历史再挂条件变量等待，超时自醒。
type wsCollector struct {
	mu      sync.Mutex
	cond    *sync.Cond
	seen    []wsDownMessage
	readErr error
}

func newWSCollector() *wsCollector {
	c := &wsCollector{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Record 记录一条下行消息并唤醒等待者（读协程调用）。
func (c *wsCollector) Record(msg wsDownMessage) {
	c.mu.Lock()
	c.seen = append(c.seen, msg)
	c.mu.Unlock()
	c.cond.Broadcast()
}

// RecordError 记录连接终态错误并唤醒等待者（读协程调用，仅首次生效）。
func (c *wsCollector) RecordError(err error) {
	c.mu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.mu.Unlock()
	c.cond.Broadcast()
}

// waitFor 返回首条命中谓词的消息；先扫已收历史，未命中则阻塞等待直至
// 命中 / 超时 / 连接断开。
func (c *wsCollector) waitFor(pred func(wsDownMessage) bool, timeout time.Duration) (wsDownMessage, error) {
	deadline := time.Now().Add(timeout)
	timer := time.AfterFunc(timeout, c.cond.Broadcast)
	defer timer.Stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		for _, m := range c.seen {
			if pred(m) {
				return m, nil
			}
		}
		if c.readErr != nil {
			return wsDownMessage{}, fmt.Errorf("WebSocket 连接已断开: %v", c.readErr)
		}
		if !time.Now().Before(deadline) {
			return wsDownMessage{}, fmt.Errorf("等待超时（%s 内未出现目标消息）", timeout)
		}
		c.cond.Wait()
	}
}

func (c *wsCollector) terminalError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

func (c *wsCollector) hasNonEmptySubtitle() bool {
	return c.lastSubtitleText() != ""
}

// lastSubtitleText 最后一条非空字幕（ASR 部分级/合并级共用 isFinal=false，
// 无法区分；冒烟取最后可见文本作为提交修正文本）。
func (c *wsCollector) lastSubtitleText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.seen) - 1; i >= 0; i-- {
		if c.seen[i].Type == "subtitle" && strings.TrimSpace(c.seen[i].Text) != "" {
			return c.seen[i].Text
		}
	}
	return ""
}

// subtitleFinalText 模型回合提交后下发的最终字幕（Java triggerLlmResponse
// sendSubtitle(userText, true)；isFinal=true 仅出现在回合提交后）。
func (c *wsCollector) subtitleFinalText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.seen) - 1; i >= 0; i-- {
		if c.seen[i].Type == "subtitle" && c.seen[i].IsFinal {
			return c.seen[i].Text
		}
	}
	return ""
}

// textFinalContent 最终文本（面试官回复收尾一次）。
func (c *wsCollector) textFinalContent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.seen) - 1; i >= 0; i-- {
		if c.seen[i].Type == "text" && c.seen[i].Final {
			return c.seen[i].Content
		}
	}
	return ""
}

func (c *wsCollector) audioChunkCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.seen {
		if m.Type == "audio_chunk" {
			n++
		}
	}
	return n
}

func (c *wsCollector) hasAudioComplete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.seen {
		if m.Type == "control" && m.Action == "audio_complete" {
			return true
		}
	}
	return false
}

// openingAudioBase64 首条非空整段音频（开场白音频，服务端真实 TTS 合成产物）。
func (c *wsCollector) openingAudioBase64() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.seen {
		if m.Type == "audio" && m.Data != "" {
			return m.Data
		}
	}
	return ""
}

// firstErrorMessage 首条服务端 error 话术（任一步骤发现即判失败）。
func (c *wsCollector) firstErrorMessage() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.seen {
		if m.Type == "error" {
			return m.Message
		}
	}
	return ""
}

// ===== WS 地址换算 =====

// wsURLFromBase 把服务地址与相对 webSocketUrl 拼成拨号地址（http→ws、https→wss）。
func wsURLFromBase(base, path string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("webSocketUrl 应为相对路径，实际 %q", path)
	}
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", fmt.Errorf("解析服务地址失败: %v", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("服务地址协议须为 http/https，实际 %q", u.Scheme)
	}
	u.Path = path
	return u.String(), nil
}

// ===== voice 冒烟流程 =====

// voiceSmoke voice 模式流程状态；HTTP 封装复用 text 模式的 smoke。
type voiceSmoke struct {
	sm   *smoke
	opts *options

	sessionID  int64
	wsPath     string
	conn       *websocket.Conn
	writeMu    sync.Mutex
	collector  *wsCollector
	opening    string // 开场白文本（回合断言日志用）
	evaluation []byte // COMPLETED 时的评估报告原始 JSON
}

// runVoice voice 模式主流程：与 run() 同构的步骤化执行框架。
func runVoice(opts *options, out io.Writer) error {
	base := strings.TrimRight(opts.baseURL, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return fmt.Errorf("参数 -base-url 非法：%q（须包含 http:// 或 https://）", opts.baseURL)
	}
	client := &http.Client{Timeout: opts.timeout}
	sm := &smoke{client: client, base: base, out: out, opts: opts, stepElapsed: map[string]time.Duration{}}
	v := &voiceSmoke{sm: sm, opts: opts}

	fmt.Fprintf(out, "== 语音面试全链路冒烟（票 10）==\n服务: %s  模式: voice  方向: %s  难度: %s  超时: %s  评估轮询上限: %s\n\n",
		base, opts.skillID, opts.difficulty, opts.timeout, opts.evalWait)

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"前置检查语音配置", v.stepVoicePrecheck},
		{"健康检查", sm.stepHealth},
		{"创建语音场次", v.stepVoiceCreate},
		{"WS 连接与开场白", v.stepVoiceConnectOpening},
		{"TTS→重采样→ASR 回环", v.stepVoiceFeedASR},
		{"提交回答", v.stepVoiceSubmit},
		{"模型回合断言", v.stepVoiceTurnAssertions},
		{"结束面试", v.stepVoiceEnd},
		{"轮询评估状态", v.stepVoicePollEvaluation},
		{"报告结构校验", v.stepVoiceReport},
		{"清理场次", v.stepVoiceCleanup},
	}
	return executeSteps(context.Background(), out, sm, steps)
}

// await 等待目标消息；等待期间收到服务端 error 话术即失败即停（不再空等超时），
// 超时失败时若服务端已下发 error 也一并透出（便于定位 ASR/TTS 配置类故障）。
func (v *voiceSmoke) await(what string, pred func(wsDownMessage) bool, timeout time.Duration) (wsDownMessage, error) {
	m, err := v.collector.waitFor(func(m wsDownMessage) bool {
		return pred(m) || m.Type == "error"
	}, timeout)
	if err != nil {
		if msg := v.collector.firstErrorMessage(); msg != "" {
			return m, fmt.Errorf("未等到%s（服务端已下发错误: %s）", what, msg)
		}
		return m, fmt.Errorf("未等到%s: %v", what, err)
	}
	if m.Type == "error" {
		return m, fmt.Errorf("等待%s期间服务端下发错误: %s", what, m.Message)
	}
	return m, nil
}

// checkServerError 步骤内时点检查：服务端 error 话术即失败（失败即停）。
func (v *voiceSmoke) checkServerError() error {
	if msg := v.collector.firstErrorMessage(); msg != "" {
		return fmt.Errorf("服务端下发错误: %s", msg)
	}
	return nil
}

// sendJSON 上行一条文本帧（gorilla 不允许并发写，写锁串行化）。
func (v *voiceSmoke) sendJSON(payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化上行消息失败: %v", err)
	}
	v.writeMu.Lock()
	defer v.writeMu.Unlock()
	_ = v.conn.SetWriteDeadline(time.Now().Add(v.opts.timeout))
	return v.conn.WriteMessage(websocket.TextMessage, raw)
}

// ===== 各步骤 =====

// stepVoicePrecheck 前置检查（票面：voice_config 真实 ASR/TTS Key 缺失时清晰报错）：
// 读设置页两个 GET 接口；服务端对 Key 掩码，掩码为 "***"（Key 缺失或过短）即判定
// 未配置真实 Key。
func (v *voiceSmoke) stepVoicePrecheck(ctx context.Context) error {
	var asr, tts *voiceConfigView
	asrErr := v.sm.doJSON(ctx, http.MethodGet, "/api/llm-provider/voice/asr", nil, &asr)
	ttsErr := v.sm.doJSON(ctx, http.MethodGet, "/api/llm-provider/voice/tts", nil, &tts)
	if err := validateVoiceConfigReady(asr, asrErr, tts, ttsErr); err != nil {
		return err
	}
	v.sm.logf("voice_config 已配置：ASR 掩码 Key=%s，TTS 掩码 Key=%s", asr.MaskedAPIKey, tts.MaskedAPIKey)
	return nil
}

// stepVoiceCreate 真实创建语音场次（skillId/difficulty/llmProvider 透传既有 flags）。
func (v *voiceSmoke) stepVoiceCreate(ctx context.Context) error {
	body := map[string]any{
		"skillId":    v.opts.skillID,
		"difficulty": v.opts.difficulty,
	}
	if v.opts.provider != "" {
		body["llmProvider"] = v.opts.provider
	}
	var sess voiceSessionDTO
	if err := v.sm.doJSON(ctx, http.MethodPost, "/api/voice-interview/sessions", body, &sess); err != nil {
		return err
	}
	if sess.SessionID <= 0 {
		return fmt.Errorf("sessionId=%d 非法", sess.SessionID)
	}
	if strings.TrimSpace(sess.WebSocketURL) == "" {
		return fmt.Errorf("webSocketUrl 为空（服务端应返回相对 WS 地址）")
	}
	if sess.Status != "IN_PROGRESS" {
		return fmt.Errorf("创建后 status=%q，应为 IN_PROGRESS", sess.Status)
	}
	v.sessionID = sess.SessionID
	v.wsPath = sess.WebSocketURL
	v.sm.logf("sessionId=%d phase=%s status=%s webSocketUrl=%s",
		sess.SessionID, sess.CurrentPhase, sess.Status, sess.WebSocketURL)
	return nil
}

// stepVoiceConnectOpening WS 连接并收开场白（票面断言：welcome、开场白文本、
// 开场音频非空、asr_ready）。空 Origin（非浏览器）在服务端白名单校验中放行。
func (v *voiceSmoke) stepVoiceConnectOpening(ctx context.Context) error {
	dialURL, err := wsURLFromBase(v.sm.base, v.wsPath)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: v.opts.timeout}
	conn, resp, err := dialer.Dial(dialURL, nil)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("WS 连接失败（HTTP %d %s）: %v", resp.StatusCode, resp.Status, err)
		}
		return fmt.Errorf("WS 连接失败（%s）: %v", dialURL, err)
	}
	v.conn = conn
	v.collector = newWSCollector()
	go v.readPump()

	v.sm.logf("已连接 %s", dialURL)
	if _, err := v.await("welcome", func(m wsDownMessage) bool {
		return m.Type == "control" && m.Action == "welcome"
	}, v.opts.timeout); err != nil {
		return err
	}
	if _, err := v.await("asr_ready", func(m wsDownMessage) bool {
		return m.Type == "control" && m.Action == "asr_ready"
	}, v.opts.timeout); err != nil {
		return err
	}
	opening, err := v.await("开场白文本 text(final=true)", func(m wsDownMessage) bool {
		return m.Type == "text" && m.Final && strings.TrimSpace(m.Content) != ""
	}, v.opts.timeout)
	if err != nil {
		return err
	}
	v.opening = opening.Content
	audio, err := v.await("开场音频 audio(非空)", func(m wsDownMessage) bool {
		return m.Type == "audio" && m.Data != ""
	}, v.opts.timeout)
	if err != nil {
		return err
	}
	raw, _ := base64.StdEncoding.DecodeString(audio.Data)
	v.sm.logf("welcome/asr_ready 已收到；开场白 %d 字，开场音频 %dB WAV（真实 TTS 合成）",
		len([]rune(v.opening)), len(raw))
	return nil
}

// readPump 下行读协程：文本帧解扁平视图入收集器；连接断开记终态错误。
func (v *voiceSmoke) readPump() {
	for {
		_, raw, err := v.conn.ReadMessage()
		if err != nil {
			v.collector.RecordError(err)
			return
		}
		var msg wsDownMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			v.collector.RecordError(fmt.Errorf("下行消息不是合法 JSON: %v（原文: %s）", err, excerpt(raw, 120)))
			return
		}
		v.collector.Record(msg)
	}
}

// stepVoiceFeedASR TTS→重采样→ASR 回环（票面核心）：开场音频（服务端真实 TTS
// 合成产物，作为模拟"回答"）解 WAV → 线性插值重采样 16kHz → 追加静音尾触发
// VAD 收尾 → 200ms 帧按实时节奏逐帧喂 ASR → 宽限等待并断言字幕非空。
func (v *voiceSmoke) stepVoiceFeedASR(ctx context.Context) error {
	wav, err := base64.StdEncoding.DecodeString(v.collector.openingAudioBase64())
	if err != nil {
		return fmt.Errorf("开场音频 base64 解码失败: %v", err)
	}
	pcmSrc, rate, err := ParseWavPCM(wav)
	if err != nil {
		return fmt.Errorf("开场音频 WAV 解析失败: %v", err)
	}
	pcm := ResampleLinear(pcmSrc, rate, asrSampleRate)
	silence := make([]int16, int(asrSampleRate*feedTrailingSilence/time.Second))
	pcm = append(pcm, silence...)
	frames := FramePCM16(pcm, pcmSamplesPerFrame)
	if len(frames) == 0 {
		return fmt.Errorf("开场音频重采样后不足一帧（%.1fs@%dHz）", float64(len(pcmSrc))/float64(rate), rate)
	}
	total := time.Duration(len(frames)) * feedFrameInterval
	v.sm.logf("%d 样本@%dHz → 重采样 16kHz → %d 帧 × 200ms（含 %.1fs 静音尾），喂入预计 %s",
		len(pcmSrc), rate, len(frames),
		feedTrailingSilence.Seconds(), total.Round(time.Millisecond))

	start := time.Now()
	for i, frame := range frames {
		if err := v.sendJSON(wsUpAudio{Type: "audio", Data: encodePCMFrame(frame)}); err != nil {
			return fmt.Errorf("发送第 %d/%d 帧失败: %v", i+1, len(frames), err)
		}
		if err := v.checkServerError(); err != nil {
			return err
		}
		if (i+1)%25 == 0 {
			v.sm.logf("已喂 %d/%d 帧（用时 %s）", i+1, len(frames), time.Since(start).Round(time.Second))
		}
		if i < len(frames)-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(feedFrameInterval):
			}
		}
	}
	v.sm.logf("喂入完成（用时 %s），等待 ASR 收尾（静音 %.1fs + 宽限 %.1fs）",
		time.Since(start).Round(time.Millisecond), feedTrailingSilence.Seconds(), feedFinalizeGrace.Seconds())
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(feedFinalizeGrace):
	}
	if err := v.checkServerError(); err != nil {
		return err
	}
	if !v.collector.hasNonEmptySubtitle() {
		return fmt.Errorf("逐帧喂入后未收到任何非空字幕（ASR 未产出识别结果，请检查 ASR Key 与配额）")
	}
	v.sm.logf("ASR 已产出字幕，合并文本 %d 字: %q",
		len([]rune(v.collector.lastSubtitleText())), v.collector.lastSubtitleText())
	return nil
}

// encodePCMFrame 帧样本 → 小端字节 → base64（上行 base64 装帧约定）。
func encodePCMFrame(frame []int16) string {
	raw := make([]byte, 0, len(frame)*2)
	for _, s := range frame {
		raw = binary.LittleEndian.AppendUint16(raw, uint16(s))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// stepVoiceSubmit control submit（票面：可带修正文本）——携带 ASR 字幕文本提交，
// 保证回合确定性触发（字幕非空已在上一步断言；文本与合并缓冲一致，覆盖仅为兜底
// 部分级未沉淀为最终级的场景）。
func (v *voiceSmoke) stepVoiceSubmit(ctx context.Context) error {
	text := v.collector.lastSubtitleText()
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("无可用字幕文本，无法提交")
	}
	if err := v.sendJSON(wsUpControl{
		Type:   "control",
		Action: "submit",
		Data:   map[string]any{"text": text},
	}); err != nil {
		return fmt.Errorf("发送 submit 失败: %v", err)
	}
	v.sm.logf("已提交（修正文本 %d 字）: %q", len([]rune(text)), text)
	return nil
}

// stepVoiceTurnAssertions 模型回合断言（票面：subtitle(isFinal=true) 非空、
// text(final=true) 非空、至少一个 audio_chunk、audio_complete）。subtitle(final=true)
// 由模型回合提交后下发（Java triggerLlmResponse L713），故在 submit 之后断言。
func (v *voiceSmoke) stepVoiceTurnAssertions(ctx context.Context) error {
	sub, err := v.await("subtitle(isFinal=true)", func(m wsDownMessage) bool {
		return m.Type == "subtitle" && m.IsFinal
	}, v.opts.timeout)
	if err != nil {
		return err
	}
	if strings.TrimSpace(sub.Text) == "" {
		return fmt.Errorf("subtitle(isFinal=true) 文本为空")
	}
	if _, err := v.await("text(final=true)", func(m wsDownMessage) bool {
		return m.Type == "text" && m.Final && strings.TrimSpace(m.Content) != ""
	}, v.opts.timeout); err != nil {
		return err
	}
	if _, err := v.await("audio_chunk", func(m wsDownMessage) bool {
		return m.Type == "audio_chunk"
	}, v.opts.timeout); err != nil {
		return err
	}
	if _, err := v.await("audio_complete", func(m wsDownMessage) bool {
		return m.Type == "control" && m.Action == "audio_complete"
	}, v.opts.timeout); err != nil {
		return fmt.Errorf("%v（若服务端关闭了分块音频开关 chunked_audio_enabled，整段路径不发 audio_chunk/audio_complete）", err)
	}
	if err := v.checkServerError(); err != nil {
		return err
	}
	v.sm.logf("回合完整：subtitle(final)=%q text(final)=%d 字 audio_chunk=%d 条 audio_complete 已收到",
		sub.Text, len([]rune(v.collector.textFinalContent())), v.collector.audioChunkCount())
	return nil
}

// stepVoiceEnd control end_interview 结束面试，随后 REST 确认状态 COMPLETED，
// 最后礼貌关闭 WS（会话已 COMPLETED，服务端断开清理的自动结束为 no-op）。
func (v *voiceSmoke) stepVoiceEnd(ctx context.Context) error {
	if err := v.sendJSON(wsUpControl{Type: "control", Action: "end_interview"}); err != nil {
		return fmt.Errorf("发送 end_interview 失败: %v", err)
	}
	deadline := time.Now().Add(endStatusPollWait)
	for {
		var sess voiceSessionDTO
		if err := v.sm.doJSON(ctx, http.MethodGet,
			"/api/voice-interview/sessions/"+fmt.Sprint(v.sessionID), nil, &sess); err != nil {
			return err
		}
		if sess.Status == "COMPLETED" {
			v.sm.logf("end_interview 已生效，status=COMPLETED")
			break
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("end_interview 后 %s 内状态仍未达 COMPLETED（当前 %q）", endStatusPollWait, sess.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(endStatusPollInterval):
		}
	}
	v.writeMu.Lock()
	_ = v.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "smoke done"),
		time.Now().Add(time.Second))
	v.writeMu.Unlock()
	_ = v.conn.Close()
	return nil
}

// stepVoicePollEvaluation 轮询评估状态至 COMPLETED（超时 -eval-timeout，
// 进度每 15s 打印；FAILED 透出评估错误）。COMPLETED 时保留报告原始 JSON。
func (v *voiceSmoke) stepVoicePollEvaluation(ctx context.Context) error {
	path := "/api/voice-interview/sessions/" + fmt.Sprint(v.sessionID) + "/evaluation"
	deadline := time.Now().Add(v.opts.evalWait)
	lastProgress := time.Time{}
	for {
		var status voiceEvalStatusDTO
		if err := v.sm.doJSON(ctx, http.MethodGet, path, nil, &status); err != nil {
			return err
		}
		cur := ""
		if status.EvaluateStatus != nil {
			cur = *status.EvaluateStatus
		}
		switch cur {
		case "COMPLETED":
			v.evaluation = status.Evaluation
			v.sm.logf("evaluateStatus=COMPLETED（评估完成）")
			return nil
		case "FAILED":
			errMsg := ""
			if status.EvaluateError != nil {
				errMsg = *status.EvaluateError
			}
			return fmt.Errorf("评估失败 evaluateStatus=FAILED, evaluateError=%q", errMsg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("评估超时（%s 内未达 COMPLETED，最后状态=%q）", v.opts.evalWait, orNone(cur))
		}
		if lastProgress.IsZero() || time.Since(lastProgress) >= progressInterval {
			v.sm.logf("evaluateStatus=%s …（剩余 %s）", orNone(cur), time.Until(deadline).Round(time.Second))
			lastProgress = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// stepVoiceReport 报告结构校验（票面断言项：报告必备字段齐全）。
func (v *voiceSmoke) stepVoiceReport(ctx context.Context) error {
	if len(v.evaluation) == 0 || string(v.evaluation) == "null" {
		return fmt.Errorf("评估已 COMPLETED 但 evaluation 报告为空")
	}
	if err := validateVoiceReport(v.evaluation, v.sessionID); err != nil {
		return err
	}
	for _, w := range voiceReportWarnings(v.evaluation) {
		v.sm.logf("告警: %s", w)
	}
	var summary struct {
		TotalQuestions int  `json:"totalQuestions"`
		OverallScore   *int `json:"overallScore"`
	}
	_ = json.Unmarshal(v.evaluation, &summary)
	v.sm.logf("报告必备字段齐全：totalQuestions=%d overallScore=%s 逐题 question/feedback 非空",
		summary.TotalQuestions, orNilInt(summary.OverallScore))
	return nil
}

// stepVoiceCleanup 清理场次（-keep 跳过）；删除失败不重试、判定失败（对齐 text 模式）。
func (v *voiceSmoke) stepVoiceCleanup(ctx context.Context) error {
	if v.opts.keep {
		v.sm.logf("-keep 指定，保留场次 %d", v.sessionID)
		return nil
	}
	if err := v.sm.doJSON(ctx, http.MethodDelete,
		"/api/voice-interview/sessions/"+fmt.Sprint(v.sessionID), nil, nil); err != nil {
		return err
	}
	v.sm.logf("场次 %d 已删除", v.sessionID)
	return nil
}

// orNilInt 可空整数显示（进度打印用）。
func orNilInt(v *int) string {
	if v == nil {
		return "(空)"
	}
	return fmt.Sprint(*v)
}
