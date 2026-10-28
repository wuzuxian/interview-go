// DashScope OmniRealtime 实时语音识别（ASR）协议客户端（ADR 0003：手写协议客户端，
// 不设接口接缝——测试把 URL 指向进程内假服务端，走真实协议代码，spec 实现决策 19）。
//
// 线协议依据（逐字核对来源，勿凭记忆改动）：
//   - Java 参考实现 service/QwenAsrService.java：事件名即线协议事件名——
//     session.created / session.updated / session.finished /
//     conversation.item.input_audio_transcription.completed / .text / .delta / .failed / error。
//   - SDK 封装的线协议（反汇编 dashscope-sdk-java-2.22.7.jar 的
//     com.alibaba.dashscope.audio.omni.OmniRealtimeConversation / OmniRealtimeParam /
//     OmniRealtimeConfig / protocol.DashScopeHeaders 核对）：
//     建连：GET <url>?model=<model>（OmniRealtimeParam.getUrl() 拼接），
//     握手头 Authorization: Bearer <key> 与 X-DashScope-DataInspection: enable；
//     会话更新：{"event_id":"event_N","type":"session.update","session":{…}}，session 内：
//     modalities（Java 传 [TEXT]）、turn_detection{type,threshold,prefix_padding_ms,
//     silence_duration_ms}（仅 enableTurnDetection 时，prefix_padding_ms 为 SDK
//     builder 默认 0）、input_audio_transcription{sample_rate,input_audio_format,
//     language}；SDK 未配置的 null 字段（voice / input_audio_format /
//     output_audio_format）序列化时省略；
//     音频推送：{"event_id":"event_N","type":"input_audio_buffer.append","audio":"<base64>"}；
//     结束：{"event_id":"event_N","type":"session.finish"}。
//   - 会话更新取值对齐 QwenAsrService.startTranscriptionLocked 的 OmniRealtimeConfig：
//     modalities=[TEXT]、server_vad、静音时长来自语音配置（Java yml 默认 2000ms）。
//
// 行为常量对齐 Java：ready 检查 10s、未就绪至多重试 2 次（Handler
// ASR_READY_CHECK_DELAY_SECONDS=10 / MAX_ASR_READY_RETRY=2）；sendAudio 就绪等待 1200ms；
// 重启 stop→200ms→start→1s 内 ready 验证（restartTranscription 10×100ms 轮询）。
// 超时均可经 Config 的 Asr* 字段注入（生产取 app.voice_interview 默认值，测试注入短值）。
//
// 有意偏离：Java SDK endSession 发送 session.finish 后至多等 20s 服务端 session.finished
// 再 close(1000,"bye")；Go 发送 session.finish 后立即关闭——重启路径不能阻塞 20s，
// 且会话状态已先行移除、迟到的最终结果本就会被丢弃，DashScope 对客户端先行关闭容忍
// （与 Java 的「may already be closed」容错一致）。
package voiceinterview

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// 线协议事件名（即 Java QwenAsrService.handleServerEvent 解析的 type 值）。
const (
	asrEvtSessionCreated  = "session.created"
	asrEvtSessionUpdated  = "session.updated"
	asrEvtSessionFinished = "session.finished"
	asrEvtTransCompleted  = "conversation.item.input_audio_transcription.completed"
	asrEvtTransText       = "conversation.item.input_audio_transcription.text"
	asrEvtTransDelta      = "conversation.item.input_audio_transcription.delta"
	asrEvtTransFailed     = "conversation.item.input_audio_transcription.failed"
	asrEvtError           = "error"
)

// 客户端发出的线协议 type 值与 id 前缀（SDK OmniRealtimeConversation 常量）。
const (
	asrCmdSessionUpdate    = "session.update"
	asrCmdInputAudioAppend = "input_audio_buffer.append"
	asrCmdSessionFinish    = "session.finish"
	asrEventIDPrefix       = "event_"
)

// sendAudio 就绪等待（Java QwenAsrService.sendAudio 的 awaitReady(1200) 硬编码）。
const asrSendReadyWait = 1200 * time.Millisecond

// asrStillNotReadyPrefix 就绪监督重试耗尽的错误前缀（网关据此映射 Java
// checkAsrReadyOrRetry L460 的「语音识别连接准备超时」话术）。
const asrStillNotReadyPrefix = "ASR still not ready"

// SendAudio 错误语义（Java 以消息子串分类，Go 以哨兵 errors.Is 分类；消息文本保留
// Java 原文以便对照）：未就绪静默丢块，无会话/追加失败重启后重发（Handler
// isAsrNotReady / shouldRecoverAsrConnection）。
var (
	ErrAsrNoActiveSession = errors.New("No active session found")
	ErrAsrSessionNotReady = errors.New("ASR session not ready")
	ErrAsrAppendFailed    = errors.New("ASR append failed")
)

// AsrCallbacks ASR 事件回调（均可选；可能从客户端内部 goroutine 并发触发，
// 回调内不得阻塞或再调用同一会话的客户端方法）。回调均带会话归属。
type AsrCallbacks struct {
	// OnReady 连接建立且会话更新已发送（Java onReady，markReady 后触发）。
	OnReady func(sessionID string)
	// OnFinal 最终识别（completed 事件的 transcript）。
	OnFinal func(sessionID string, transcript string)
	// OnPartial 部分识别（.text/.delta 事件的文本，拼接规则见 extractAsrPartialText）。
	OnPartial func(sessionID string, text string)
	// OnError 错误（error 事件已脱敏为固定标记，不透传服务端原文；建连/写失败为原始错误）。
	OnError func(sessionID string, err error)
	// OnReconnecting ready 超时即将重启（Java Handler 据此发 asr_reconnecting 提示）。
	OnReconnecting func(sessionID string)
	// OnClose 连接关闭（code/reason 为 WebSocket 关闭帧值；非正常关闭 code 为 0）。
	OnClose func(sessionID string, code int, reason string)
}

// AsrClient DashScope ASR 协议客户端，管理多个并发识别会话（Java QwenAsrService 的
// 同构实现：sessions map + 每会话锁 + 连接实例比对）。具体类型，无接口接缝。
type AsrClient struct {
	cfg      Config      // 行为参数（Asr* 字段，调用方注入）
	settings AsrSettings // URL/APIKey/业务参数（来自 VoiceConfigSource，调用方注入）
	logger   *slog.Logger

	mu       sync.Mutex
	sessions map[string]*asrSession
	locks    map[string]*sync.Mutex   // 防同一会话并发 stop/start（Java sessionLocks）
	stopSigs map[string]chan struct{} // 会话级停止信号：Stop 关闭，用于退出 ready 监督
	cbsOf    map[string]AsrCallbacks  // 会话回调记录：重启时复用（Java Handler 重传同一组 lambda）

	eventSeq atomic.Int64 // event_id 序号（SDK generateEventId："event_" + 序号）
}

// asrSession 单个识别会话；conn 是连接实例（实例比对语义的载体，原子发布）。
type asrSession struct {
	id       string
	conn     atomic.Pointer[asrConn]
	cbs      AsrCallbacks
	ready    chan struct{} // 关闭即就绪（连接已建立且 session.update 已发送）
	readyOne sync.Once
	closeMu  sync.Mutex
	closed   bool
}

// asrConn 一条 WebSocket 连接 + 写锁（gorilla 不允许并发写）。
type asrConn struct {
	raw *websocket.Conn
	wmu sync.Mutex
}

func (c *asrConn) writeJSON(v any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.raw.WriteJSON(v)
}

func (c *asrConn) close(code int, reason string) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.raw.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	_ = c.raw.Close()
}

// NewAsrClient 构造客户端（目标 URL 与业务参数来自语音配置的 AsrSettings，
// 行为参数来自 Config 的 Asr* 字段）。APIKey 缺失即报错（Java @PostConstruct
// 「API key must be configured」语义）；未注入的 Asr* 行为参数回退 Java 默认值；
// logger 为 nil 时用 slog.Default()。
func NewAsrClient(cfg Config, settings AsrSettings, logger *slog.Logger) (*AsrClient, error) {
	if strings.TrimSpace(settings.APIKey) == "" {
		return nil, errors.New("API key must be configured before initializing QwenAsrService")
	}
	if logger == nil {
		logger = slog.Default()
	}
	// Java 常量兜底：10s / 2 次 / 200ms / 1s（Handler L93-94 与 restartTranscription）。
	if cfg.AsrReadyTimeout <= 0 {
		cfg.AsrReadyTimeout = 10 * time.Second
	}
	if cfg.AsrReadyRetries <= 0 {
		cfg.AsrReadyRetries = 2
	}
	if cfg.AsrRestartPause <= 0 {
		cfg.AsrRestartPause = 200 * time.Millisecond
	}
	if cfg.AsrRestartReadyTimeout <= 0 {
		cfg.AsrRestartReadyTimeout = 1 * time.Second
	}
	return &AsrClient{
		cfg:      cfg,
		settings: settings,
		logger:   logger,
		sessions: make(map[string]*asrSession),
		locks:    make(map[string]*sync.Mutex),
		stopSigs: make(map[string]chan struct{}),
		cbsOf:    make(map[string]AsrCallbacks),
	}, nil
}

func (c *AsrClient) lockFor(sessionID string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.locks[sessionID]; ok {
		return l
	}
	l := &sync.Mutex{}
	c.locks[sessionID] = l
	return l
}

// Start 新建识别会话：建连 → session.update（modalities=TEXT、server_vad、静音时长）→
// ready。连接与就绪是异步的，失败经 OnError 回调上报；会话已存在时报错
// （Java「Session already exists」）。Start 同时启动 ready 监督：
// 超时未就绪则 OnReconnecting + Restart，至多重试 cfg.AsrReadyRetries 次。
func (c *AsrClient) Start(sessionID string, cbs AsrCallbacks) error {
	cbs = filledAsrCallbacks(cbs)
	lock := c.lockFor(sessionID)
	lock.Lock()
	defer lock.Unlock()

	c.mu.Lock()
	if _, exists := c.sessions[sessionID]; exists {
		c.mu.Unlock()
		return fmt.Errorf("Session already exists: %s", sessionID)
	}
	sig := make(chan struct{})
	c.stopSigs[sessionID] = sig
	c.cbsOf[sessionID] = cbs
	c.mu.Unlock()

	if err := c.startLocked(sessionID, cbs); err != nil {
		c.mu.Lock()
		delete(c.stopSigs, sessionID)
		delete(c.cbsOf, sessionID)
		c.mu.Unlock()
		return err
	}
	go c.superviseReady(sessionID, sig, cbs)
	return nil
}

// startLocked 登记会话并异步建连（调用方持有会话锁）。
// 先入 map 再建连，保证 HasActiveSession 立即可见（Java 同序并注明原因）。
func (c *AsrClient) startLocked(sessionID string, cbs AsrCallbacks) error {
	c.mu.Lock()
	if _, exists := c.sessions[sessionID]; exists {
		c.mu.Unlock()
		return fmt.Errorf("Session already exists: %s", sessionID)
	}
	s := &asrSession{
		id:    sessionID,
		cbs:   cbs,
		ready: make(chan struct{}),
	}
	c.sessions[sessionID] = s
	c.mu.Unlock()

	go c.connect(s)
	return nil
}

// connect 建连并完成会话更新（Java 连接线程的异步路径；失败移除会话并回调 OnError）。
func (c *AsrClient) connect(s *asrSession) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.settings.APIKey)
	header.Set("X-DashScope-DataInspection", "enable")

	// OmniRealtimeParam.getUrl()：<url>?model=<model>。
	url := c.settings.URL + "?model=" + c.settings.Model
	dialer := &websocket.Dialer{HandshakeTimeout: c.handshakeTimeout()}
	raw, _, err := dialer.Dial(url, header)
	if err != nil {
		c.connectFailed(s, err)
		return
	}
	s.conn.Store(&asrConn{raw: raw})

	// session.update（OmniRealtimeConfig.getConfig 形状，见文件头注释；null 字段省略）。
	sessionBody := map[string]any{
		"modalities": []string{"TEXT"},
	}
	if c.settings.EnableTurnDetection {
		sessionBody["turn_detection"] = map[string]any{
			"type":                c.settings.TurnDetectionType,
			"threshold":           c.settings.TurnDetectionThreshold,
			"prefix_padding_ms":   0, // SDK builder 默认值（Java 未配置该字段）
			"silence_duration_ms": c.settings.TurnDetectionSilenceDurationMs,
		}
	}
	transcription := map[string]any{}
	if c.settings.SampleRate > 0 {
		transcription["sample_rate"] = c.settings.SampleRate
	}
	if c.settings.Format != "" {
		transcription["input_audio_format"] = c.settings.Format
	}
	if c.settings.Language != "" {
		transcription["language"] = c.settings.Language
	}
	sessionBody["input_audio_transcription"] = transcription

	update := map[string]any{
		"event_id": c.nextEventID(),
		"type":     asrCmdSessionUpdate,
		"session":  sessionBody,
	}
	if err := s.conn.Load().writeJSON(update); err != nil {
		c.connectFailed(s, err)
		return
	}

	// 就绪 = 连接建立 + session.update 已发送（Java markReady 时机，不等服务端 ack）。
	// 过期连接不发 ready（Java sessions.get(sessionId) != asrSession 的实例比对），
	// 并关闭被废弃的连接避免泄漏。
	if !c.isCurrentSession(s.id, s) {
		c.logger.Debug("忽略过期 ASR 连接的就绪回调", slog.String("session_id", s.id))
		_ = raw.Close()
		return
	}
	s.readyOne.Do(func() { close(s.ready) })
	s.cbs.OnReady(s.id)
	c.logger.Info("语音识别会话已启动",
		slog.String("session_id", s.id), slog.String("model", c.settings.Model))

	go c.readPump(s)
}

func (c *AsrClient) handshakeTimeout() time.Duration {
	// 握手超时与 ready 检查同界：就绪监督是最终兜底（Java okhttp 默认连接超时同级）。
	return c.cfg.AsrReadyTimeout
}

func (c *AsrClient) connectFailed(s *asrSession, err error) {
	c.logger.Error("ASR 建连失败", slog.String("session_id", s.id), slog.String("error", err.Error()))
	c.removeIfCurrent(s.id, s)
	s.cbs.OnError(s.id, err)
}

// superviseReady ready 监督：每 cfg.AsrReadyTimeout 检查一次；未就绪则提示重启，
// 至多 cfg.AsrReadyRetries 次，耗尽后 OnError（Java checkAsrReadyOrRetry 流程）。
// 仅 Stop（stopSigs 关闭）终止监督——连接被服务端关闭时 Java 也会继续重试到耗尽。
func (c *AsrClient) superviseReady(sessionID string, stop <-chan struct{}, cbs AsrCallbacks) {
	for attempt := 0; ; attempt++ {
		select {
		case <-stop:
			return
		case <-time.After(c.cfg.AsrReadyTimeout):
		}
		if c.IsReady(sessionID) {
			return
		}
		// Stop 可能恰在超时窗口内发生：先复查停止信号，避免对已停止会话误报。
		select {
		case <-stop:
			return
		default:
		}
		if attempt >= c.cfg.AsrReadyRetries {
			c.logger.Warn("ASR 重试后仍未就绪",
				slog.String("session_id", sessionID), slog.Int("retries", attempt))
			cbs.OnError(sessionID, fmt.Errorf("%s after %d retries", asrStillNotReadyPrefix, attempt))
			return
		}
		c.logger.Warn("ASR 未就绪，自动重连",
			slog.String("session_id", sessionID),
			slog.Int("retry", attempt+1), slog.Int("max_retries", c.cfg.AsrReadyRetries))
		cbs.OnReconnecting(sessionID)
		c.Restart(sessionID)
	}
}

// SendAudio 推送一帧 PCM（自动 base64，input_audio_buffer.append）。
// 错误语义：ErrAsrNoActiveSession（无会话/已停止，可重发）/ ErrAsrSessionNotReady
// （等待 1200ms 仍未就绪，调用方应静默丢块）/ ErrAsrAppendFailed（写失败，调用方应重启重发）。
func (c *AsrClient) SendAudio(sessionID string, pcm []byte) error {
	s := c.getSession(sessionID)
	if s == nil {
		return fmt.Errorf("%w: %s", ErrAsrNoActiveSession, sessionID)
	}
	select {
	case <-s.ready:
	case <-time.After(asrSendReadyWait):
		return fmt.Errorf("%w: %s", ErrAsrSessionNotReady, sessionID)
	}
	payload := map[string]any{
		"event_id": c.nextEventID(),
		"type":     asrCmdInputAudioAppend,
		"audio":    base64.StdEncoding.EncodeToString(pcm),
	}
	if err := s.conn.Load().writeJSON(payload); err != nil {
		c.logger.Error("ASR 音频写入失败（上游或将重连）",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return fmt.Errorf("%w: %s", ErrAsrAppendFailed, sessionID)
	}
	return nil
}

// Restart 失效重启：stop → cfg.AsrRestartPause 短歇 → start（复用原回调）→
// cfg.AsrRestartReadyTimeout 内 ready 验证（每 100ms 轮询，Java restartTranscription）。
// 重启不触碰 stopSigs：ready 监督跨重启延续。
func (c *AsrClient) Restart(sessionID string) {
	lock := c.lockFor(sessionID)
	lock.Lock()
	defer lock.Unlock()

	// 会话已被显式停止（Stop 在同一把会话锁内删除停止信号）时不再重启，
	// 防止监督协程最后一次竞态触发把已停止的会话复活成僵尸。
	c.mu.Lock()
	_, supervised := c.stopSigs[sessionID]
	c.mu.Unlock()
	if !supervised {
		return
	}

	c.logger.Info("重启 DashScope ASR（stop + start）", slog.String("session_id", sessionID))

	// 重启用原回调：优先取存活会话上的记录，会话已被失败清理时回退客户端级记录
	// （Java Handler 以同一组 lambda 重新调用）。
	c.mu.Lock()
	prev := c.sessions[sessionID]
	var cbs AsrCallbacks
	if prev != nil {
		cbs = prev.cbs
	} else if recorded, ok := c.cbsOf[sessionID]; ok {
		cbs = recorded
	} else {
		cbs = filledAsrCallbacks(AsrCallbacks{})
	}
	delete(c.sessions, sessionID)
	c.mu.Unlock()

	if prev != nil {
		c.closeSession(prev)
	} else {
		c.logger.Warn("尝试停止不存在的 ASR 会话", slog.String("session_id", sessionID))
	}
	time.Sleep(c.cfg.AsrRestartPause)
	if err := c.startLocked(sessionID, cbs); err != nil {
		c.logger.Warn("ASR 重启建会话失败",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return
	}

	deadline := time.Now().Add(c.cfg.AsrRestartReadyTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if c.IsReady(sessionID) {
			c.logger.Info("ASR 重连验证通过", slog.String("session_id", sessionID))
			return
		}
	}
	c.logger.Warn("ASR 重启后可能未完全就绪",
		slog.String("session_id", sessionID),
		slog.Duration("verify_timeout", c.cfg.AsrRestartReadyTimeout))
}

// Stop 停止会话：发 session.finish 后关闭连接（Java stopTranscription；
// 不等 session.finished 的偏离见文件头注释），并终止该会话的 ready 监督。
// 会话不存在仅告警。
func (c *AsrClient) Stop(sessionID string) {
	lock := c.lockFor(sessionID)
	lock.Lock()
	defer lock.Unlock()

	c.mu.Lock()
	s := c.sessions[sessionID]
	delete(c.sessions, sessionID)
	sig := c.stopSigs[sessionID]
	delete(c.stopSigs, sessionID)
	delete(c.cbsOf, sessionID)
	delete(c.locks, sessionID)
	c.mu.Unlock()

	if s == nil {
		c.logger.Warn("尝试停止不存在的 ASR 会话", slog.String("session_id", sessionID))
		return
	}
	c.closeSession(s)
	if sig != nil {
		close(sig)
	}
}

// Close 停止全部会话（进程退出清理，Java destroy 语义）。
func (c *AsrClient) Close() {
	c.mu.Lock()
	ids := make([]string, 0, len(c.sessions))
	for id := range c.sessions {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.Stop(id)
	}
}

// HasActiveSession 会话是否存在（Java hasActiveSession）。
func (c *AsrClient) HasActiveSession(sessionID string) bool {
	return c.getSession(sessionID) != nil
}

// IsReady 会话是否已就绪（Java isReady）。
func (c *AsrClient) IsReady(sessionID string) bool {
	s := c.getSession(sessionID)
	if s == nil {
		return false
	}
	select {
	case <-s.ready:
		return true
	default:
		return false
	}
}

// ===== 内部：连接读泵与事件分发 =====

// readPump 持续读取服务端事件并分发；连接断开时按实例比对清理（防旧 onClose 误删）。
func (c *AsrClient) readPump(s *asrSession) {
	conn := s.conn.Load()
	for {
		_, data, err := conn.raw.ReadMessage()
		if err != nil {
			code, reason := asrCloseInfo(err)
			c.logger.Warn("DashScope ASR 连接已关闭",
				slog.String("session_id", s.id),
				slog.Int("code", code), slog.String("reason", reason))
			c.removeIfCurrent(s.id, s)
			s.cbs.OnClose(s.id, code, reason)
			return
		}
		c.handleServerEvent(s, data)
	}
}

// asrCloseInfo 从读错误中提取 WebSocket 关闭码与原因（正常关闭帧）；
// 非关闭帧错误 code 取 0、原错误文本作 reason。
func asrCloseInfo(err error) (int, string) {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Code, closeErr.Text
	}
	return 0, err.Error()
}

// handleServerEvent 事件分发，逐字对齐 Java handleServerEvent（含 extractTranscriptPayload）。
func (c *AsrClient) handleServerEvent(s *asrSession, data []byte) {
	var message map[string]any
	if err := json.Unmarshal(data, &message); err != nil {
		c.logger.Error("ASR 服务端事件解析失败",
			slog.String("session_id", s.id), slog.String("error", err.Error()))
		s.cbs.OnError(s.id, err)
		return
	}
	eventType, _ := message["type"].(string)

	switch eventType {
	case asrEvtSessionCreated:
		c.logger.Debug("服务端会话已创建", slog.String("session_id", s.id))
	case asrEvtSessionUpdated:
		c.logger.Debug("服务端会话配置已更新", slog.String("session_id", s.id))
	case asrEvtTransCompleted:
		transcript, _ := message["transcript"].(string)
		language, ok := message["language"].(string)
		if !ok {
			language = "unknown"
		}
		emotion, ok := message["emotion"].(string)
		if !ok {
			emotion = "neutral"
		}
		c.logger.Debug("转写完成",
			slog.String("session_id", s.id),
			slog.String("language", language), slog.String("emotion", emotion),
			slog.Int("text_len", len(transcript)))
		s.cbs.OnFinal(s.id, transcript)
	case asrEvtTransText, asrEvtTransDelta:
		c.dispatchPartial(s, message)
	case asrEvtError:
		errObj, _ := message["error"].(map[string]any)
		errType := "unknown"
		errCode := "unknown"
		errMessage := "Unknown error"
		if errObj != nil {
			if v, ok := errObj["type"].(string); ok {
				errType = v
			}
			if v, ok := errObj["code"].(string); ok {
				errCode = v
			}
			if v, ok := errObj["message"].(string); ok && v != "" {
				errMessage = v
			}
		}
		// 脱敏对齐 Java ErrorLogSanitizer.summarize(String)：外部自由文本归一为固定标记。
		full := fmt.Sprintf("ASR Error [%s/%s]: %s", errType, errCode, sanitizeExternalMessage(errMessage))
		c.logger.Error("ASR 服务端错误", slog.String("session_id", s.id), slog.String("detail", full))
		s.cbs.OnError(s.id, errors.New(full))
	case asrEvtSessionFinished:
		c.logger.Debug("服务端会话已结束", slog.String("session_id", s.id))
	case asrEvtTransFailed:
		c.logger.Error("单句转写失败", slog.String("session_id", s.id))
	default:
		if strings.Contains(eventType, "transcription") {
			c.logger.Debug("未处理的转写相关事件",
				slog.String("session_id", s.id), slog.String("type", eventType))
		} else {
			c.logger.Debug("未处理事件类型",
				slog.String("session_id", s.id), slog.String("type", eventType))
		}
	}
}

// dispatchPartial 部分识别：文本提取后回调，空白不回调（Java dispatchPartialTranscript）。
func (c *AsrClient) dispatchPartial(s *asrSession, message map[string]any) {
	text := extractAsrPartialText(message)
	if strings.TrimSpace(text) != "" {
		s.cbs.OnPartial(s.id, text)
		return
	}
	c.logger.Debug("部分识别事件无可提取文本", slog.String("session_id", s.id))
}

// extractAsrPartialText 提取可展示文本，逐字对齐 Java extractTranscriptPayload：
// transcript 直取 → text+stash 拼接 → delta（字符串/对象 text/对象 transcript）→ item.transcript。
func extractAsrPartialText(message map[string]any) string {
	if v, ok := message["transcript"].(string); ok {
		return v
	}
	_, hasText := message["text"]
	_, hasStash := message["stash"]
	if hasText || hasStash {
		prefix, _ := message["text"].(string)
		suffix, _ := message["stash"].(string)
		return prefix + suffix
	}
	if delta, ok := message["delta"]; ok {
		switch d := delta.(type) {
		case string:
			return d
		case map[string]any:
			if v, ok := d["text"].(string); ok {
				return v
			}
			if v, ok := d["transcript"].(string); ok {
				return v
			}
		}
	}
	if item, ok := message["item"].(map[string]any); ok {
		if v, ok := item["transcript"].(string); ok {
			return v
		}
	}
	return ""
}

// sanitizeExternalMessage 对齐 Java ErrorLogSanitizer.summarize(String)：
// 空白返回空串，其余返回固定标记，避免服务端自由文本被回显。
func sanitizeExternalMessage(message string) string {
	if strings.TrimSpace(message) == "" {
		return ""
	}
	return "external-error"
}

// ===== 内部：会话状态 =====

func (c *AsrClient) getSession(sessionID string) *asrSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[sessionID]
}

// isCurrentSession 实例比对：s 是否仍是 sessionID 的当前连接
// （Java「仅移除与本次连接对应的会话，避免重连后旧 onClose 误删新连接」）。
func (c *AsrClient) isCurrentSession(sessionID string, s *asrSession) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[sessionID] == s
}

func (c *AsrClient) removeIfCurrent(sessionID string, s *asrSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessions[sessionID] == s {
		delete(c.sessions, sessionID)
	}
}

// closeSession 发 session.finish 并关闭连接（幂等）。
func (c *AsrClient) closeSession(s *asrSession) {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if conn := s.conn.Load(); conn != nil {
		// 发送失败仅告警（Java：「Error while ending session (may already be closed)」）。
		if err := conn.writeJSON(map[string]any{
			"event_id": c.nextEventID(),
			"type":     asrCmdSessionFinish,
		}); err != nil {
			c.logger.Warn("ASR session.finish 发送失败（连接可能已关闭）",
				slog.String("session_id", s.id), slog.String("error", err.Error()))
		}
		conn.close(1000, "bye")
	}
	// conn 为 nil 时建连尚未完成：连接 goroutine 会因实例比对失败自行关闭废弃连接。
	c.logger.Info("语音识别会话已停止", slog.String("session_id", s.id))
}

func (c *AsrClient) nextEventID() string {
	return fmt.Sprintf("%s%d", asrEventIDPrefix, c.eventSeq.Add(1))
}

func filledAsrCallbacks(cbs AsrCallbacks) AsrCallbacks {
	if cbs.OnReady == nil {
		cbs.OnReady = func(string) {}
	}
	if cbs.OnFinal == nil {
		cbs.OnFinal = func(string, string) {}
	}
	if cbs.OnPartial == nil {
		cbs.OnPartial = func(string, string) {}
	}
	if cbs.OnError == nil {
		cbs.OnError = func(string, error) {}
	}
	if cbs.OnReconnecting == nil {
		cbs.OnReconnecting = func(string) {}
	}
	if cbs.OnClose == nil {
		cbs.OnClose = func(string, int, string) {}
	}
	return cbs
}
