// WebSocket 网关：/ws/voice-interview/{sessionId}（gorilla/websocket + gin 路由），
// 流程与 Java VoiceInterviewWebSocketHandler（1460 行）逐字对齐：
//
//	建连（校验会话存在且 IN_PROGRESS，升级来源沿 CORS 白名单）→ 建 ASR → welcome →
//	asr_ready → 上行 audio 转发（回声冷却丢弃、ASR 失效自动重启 + 当前块重发 15×80ms）
//	→ STT 部分/最终两级字幕（最终只入话语合并缓冲，不自动触发模型）
//	→ control submit（data.text 可覆盖缓冲 + CAS 处理锁防双答）/ end_interview /
//	start_phase → 断开停 ASR 并自动结束进行中的会话（复用结束+评估投递语义）。
//
// 模型回合以挂钩（函数字段）接入：本票内置空实现（仅日志），票 06 替换为真实
// LLM+TTS 管道。发送保护对齐 Java ConcurrentWebSocketSessionDecorator(10s, 512KB)：
// 出站队列 + 专职写协程 + 逐条写超时，积压超限关闭连接，关闭后发送安全丢弃。
// 上行单条消息上限 256KB（Java setTextMessageSizeLimit）。
package voiceinterview

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// controlSubmitRetryDelay submit 抢处理锁失败后的重试间隔（Java flushMergedUtteranceToLlm
// 的 schedule(400ms) 硬编码）。
const controlSubmitRetryDelay = 400 * time.Millisecond

// wsOutboundQueueDepth 出站消息队列深度（条数）；积压主判据为字节数（512KB 上限），
// 条数上限仅作兜底，两者任一超限即按不可靠连接关闭。
const wsOutboundQueueDepth = 256

// TurnSpeaker 回合挂钩可用的下行发送通道（由网关会话实现；票 06 的真实管道经此
// 下发字幕/文本/音频并保存消息，语义对齐 Java triggerLlmResponse 内的 send* 调用）。
type TurnSpeaker interface {
	// SendSubtitle 字幕（部分级 isFinal=false / 回合提交后 isFinal=true）。
	SendSubtitle(text string, isFinal bool)
	// SendText 流式（final=false）或最终（final=true）文本。
	SendText(content string, final bool)
	// SendAudio 整段 WAV 音频（base64 封装在网关内完成）。
	SendAudio(wav []byte, text string)
	// SendAudioChunk 分句音频块（票 06 分句发射器按句序调用）。
	SendAudioChunk(wav []byte, index int, isLast bool)
	// SendAudioComplete 回合音频全部下发完毕。
	SendAudioComplete()
	// SendError 友话术错误（会话不断）。
	SendError(message string)
}

// TurnInput 回合挂钩入参。
type TurnInput struct {
	SessionID int64       // 语音面试场次 ID
	UserText  string      // 提交的用户话语（合并缓冲原文，Java accumulatedText）
	Speaker   TurnSpeaker // 下行发送通道
}

// TurnHook 模型回合挂钩（spec 实现决策 19 之外的网关接缝，函数字段）：submit 成功
// 抢到处理锁后以合并文本调用，在独立 goroutine 执行；返回后网关释放处理锁并进入
// 回声冷却（对应 Java triggerLlmResponse 的 finally）。nil 时使用内置空实现。
type TurnHook func(ctx context.Context, in TurnInput)

// GatewayDeps 网关依赖聚合。
type GatewayDeps struct {
	Service        *Service          // 会话域（结束/阶段切换/建连校验）
	Settings       VoiceConfigSource // 语音运行时配置（每连接重读，设置页修改热生效）
	Config         Config            // 行为参数（Asr*/WS*/EchoCooldown 等）
	AllowedOrigins []string          // CORS 白名单（升级来源校验，Java setAllowedOrigins）
	Turn           TurnHook          // 模型回合挂钩；nil = 内置空实现（票 06 注入真实管道）
	Logger         *slog.Logger
}

// Gateway 语音面试 WebSocket 网关。
type Gateway struct {
	svc      *Service
	settings VoiceConfigSource
	cfg      Config
	origins  []string
	turn     TurnHook
	logger   *slog.Logger
	tts      *TtsClient // 开场白音频合成（Java Handler 的 ttsService 单例同构）

	openingCache *openingAudioCache // 开场音频缓存（Java openingAudioCache 同构）

	mu   sync.Mutex
	live map[string]*wsSession // 会话键（路径段）→ 当前连接；会话间隔离
}

// NewGateway 创建网关。 origins 为空或含 "*" 时允许全部来源（对齐 AllowCORS 语义）。
func NewGateway(deps GatewayDeps) *Gateway {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Gateway{
		svc:          deps.Service,
		settings:     deps.Settings,
		cfg:          deps.Config,
		origins:      deps.AllowedOrigins,
		turn:         deps.Turn,
		logger:       deps.Logger,
		tts:          NewTtsClient(deps.Config, deps.Logger),
		openingCache: newOpeningAudioCache(),
		live:         make(map[string]*wsSession),
	}
}

// RegisterRoutes 注册根级 /ws 组下的语音面试路由（main 以 srv.Group("/ws") 传入）。
func (g *Gateway) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/voice-interview/:sessionId", g.handleWS)
}

// originAllowed 升级来源校验（Java setAllowedOrigins(cors.getAllowedOrigins().split(","))）：
// 空 Origin（非浏览器客户端）放行；白名单为空或含 "*" 放行全部（对齐 AllowCORS 语义）；
// 否则须精确命中白名单。
func originAllowed(allowOrigins []string, origin string) bool {
	if origin == "" {
		return true
	}
	if len(allowOrigins) == 0 {
		return true
	}
	for _, o := range allowOrigins {
		if strings.TrimSpace(o) == "*" || strings.TrimSpace(o) == origin {
			return true
		}
	}
	return false
}

// handleWS 升级入口：先按票面要求校验会话存在与状态（Java 原版在校验缺失，
// 本仓库票 04 明确要求），再沿白名单校验来源，最后升级。
func (g *Gateway) handleWS(c *gin.Context) {
	key := c.Param("sessionId")
	sessionID, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "会话ID格式不正确")
		return
	}
	entity, err := g.svc.GetSessionEntity(c.Request.Context(), sessionID)
	if err != nil {
		c.String(http.StatusInternalServerError, "查询语音面试会话失败")
		return
	}
	if entity == nil {
		c.String(http.StatusNotFound, "会话不存在: "+key)
		return
	}
	if statusEnum(entity.Status) != VoiceStatusInProgress {
		c.String(http.StatusConflict, "会话状态为 "+statusValue(entity.Status)+"，无法建立语音连接")
		return
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return originAllowed(g.origins, r.Header.Get("Origin"))
		},
	}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade 失败时已写出 HTTP 错误（含来源不在白名单的 403，对齐 Spring 行为）。
		return
	}
	g.establish(conn, key, sessionID)
}

// establish 建连后初始化（Java afterConnectionEstablished L149-179）：注册会话、
// 建 ASR（读最新语音配置）、下发 welcome，随后进入读循环直至断开。
func (g *Gateway) establish(conn *websocket.Conn, key string, sessionID int64) {
	s := &wsSession{
		gw:    g,
		key:   key,
		id:    sessionID,
		conn:  conn,
		state: newSessionState(g.cfg.EchoCooldown),
		sink: newWSSink(wsSinkParams{
			conn:         conn,
			limit:        g.cfg.WSSendBufferLimit,
			writeTimeout: g.cfg.WSWriteTimeout,
			logger:       g.logger,
		}),
	}
	// 检查与覆盖写在同一锁内完成（register）：并发建连时后注册者必然拿到先注册
	// 者为 prev 并在锁外将其 abandon——不会出现被覆盖出 live 却未作废、可绕过
	// 实例比对与顶替保护与当前连接并行执行回合的孤儿连接。
	if prev := g.register(s); prev != nil && prev != s {
		// 同会话重复建连：新连接顶替注册，旧连接仅作废自身（不触发会话级收尾，
		// 避免旧连接的断开清理误停新连接的 ASR/误结束会话；Java 此处直接覆盖映射）。
		prev.abandon()
	}

	// 上行单条消息上限 256KB（Java setTextMessageSizeLimit/setBinaryMessageSizeLimit）。
	conn.SetReadLimit(g.cfg.WSMessageLimit)
	// 建连即计活动起点（Java afterConnectionEstablished L165 的 lastActivityTime.put）。
	s.touchActivity()
	g.logger.Info("WebSocket 连接已建立", slog.Int64("sessionId", sessionID))

	// ASR（Java startDashScopeStt）：每连接读最新语音配置（设置页修改后热生效）。
	asrSettings, err := g.settings.VoiceAsrSettings(context.Background())
	if err == nil {
		s.asr, err = NewAsrClient(g.cfg, asrSettings, g.logger)
	}
	if err == nil {
		err = s.asr.Start(s.key, s.asrCallbacks())
	}
	if err != nil {
		// Java：建 ASR 异常 → sendError("初始化语音识别失败: " + e.getMessage())，
		// welcome 跳过，连接保留。
		g.logger.Error("初始化语音识别失败", slog.Int64("sessionId", sessionID), slog.String("error", err.Error()))
		s.SendError(msgErrInit + err.Error())
	}
	if s.asr != nil {
		// welcome 在 ASR 启动之后下发（Java 同序）；asr_ready 由就绪回调异步下发。
		s.sendJSON(buildWelcome())
		// 自动开场（Java triggerOpeningQuestionIfNeeded L174：welcome 后异步触发，
		// 仅无历史消息的会话生效）。文本先于音频由同一协程顺序保证。
		go s.triggerOpeningQuestion()
	}

	g.readPump(s)
}

// register 原子注册连接为会话当前连接：读取旧连接与覆盖写在同一锁内完成，返回
// 被顶替的旧连接（无则 nil），由调用方在锁外 abandon。
func (g *Gateway) register(s *wsSession) *wsSession {
	g.mu.Lock()
	prev := g.live[s.key]
	g.live[s.key] = s
	g.mu.Unlock()
	return prev
}

// triggerOpeningQuestion 建连自动开场（Java triggerOpeningQuestionIfNeeded
// L218-261 逐字，运行在独立协程）：无历史消息的会话按方向取开场文案 → 先落库
// （DIALOGUE、当前阶段、序号）→ 推 text(final=true) → TTS 合成音频下发（缓存命中
// 不重复合成）。任一前置不满足（连接已关/会话缺失/已有历史/文案为空）静默返回；
// 异常仅记日志不中断连接（Java catch log.error）。
func (s *wsSession) triggerOpeningQuestion() {
	defer func() {
		if r := recover(); r != nil {
			s.gw.logger.Error("发送开场白异常",
				slog.Int64("sessionId", s.id), slog.Any("panic", r))
		}
	}()

	if !s.alive() {
		return
	}
	ctx := context.Background()
	entity, err := s.gw.svc.GetSessionEntity(ctx, s.id)
	if err != nil {
		s.gw.logger.Warn("开场白查询会话实体失败",
			slog.Int64("sessionId", s.id), slog.String("error", err.Error()))
		return
	}
	if entity == nil {
		s.gw.logger.Warn("发送开场白时未找到会话实体，跳过", slog.Int64("sessionId", s.id))
		return
	}
	if s.gw.svc.hasConversationHistory(ctx, s.id) {
		// 已有历史对话（如重连/恢复），不重复开场（Java L232-235）。
		return
	}

	aiReply := buildOpeningQuestion(skillValue(entity.SkillID), embeddedOpeningConfig())
	if isBlank(aiReply) {
		return
	}
	if !s.alive() {
		return
	}

	// 先落库再推前端，确保用户提交时 DB 中已有该条消息（Java L246-248 注释原意）。
	if err := s.gw.svc.SaveOpeningMessage(ctx, s.id, aiReply); err != nil {
		s.gw.logger.Error("发送开场白失败",
			slog.Int64("sessionId", s.id), slog.String("error", err.Error()))
		return
	}
	s.SendText(aiReply, true)

	// 语音随后下发（Java L250-254）：合成失败或空结果跳过，文本不回撤。
	if wav := s.gw.getOpeningWavAudio(ctx, aiReply); len(wav) > 0 && s.alive() {
		s.SendAudio(wav, aiReply)
	}
	s.gw.logger.Info("开场白已发送", slog.Int64("sessionId", s.id))
}

// getOpeningWavAudio 开场音频（Java getOpeningWavAudio L263-281 + synthesizeToWav
// L275-281）：缓存命中直接返回；未命中按最新语音配置合成 PCM → 包 WAV 头 → 回填
// 缓存。合成失败或空结果返回 nil（不缓存，下次建连重试）。
func (g *Gateway) getOpeningWavAudio(ctx context.Context, text string) []byte {
	if cached := g.openingCache.get(text); len(cached) > 0 {
		return cached
	}
	settings, err := g.settings.VoiceTtsSettings(ctx)
	if err != nil {
		g.logger.Warn("读取语音合成配置失败，跳过开场音频",
			slog.String("error", err.Error()))
		return nil
	}
	pcm, err := g.tts.Synthesize(ctx, settings, text)
	if err != nil {
		g.logger.Warn("开场白语音合成失败，跳过音频下发",
			slog.String("error", err.Error()))
		return nil
	}
	if len(pcm) == 0 {
		return nil
	}
	wav := PcmToWav(pcm)
	g.openingCache.put(text, wav)
	return wav
}

// WarmupOpeningAudio 开场音频缓存预热（Java @PostConstruct warmupOpeningAudioCache
// L111-137）：开关关闭（默认）仅记日志不产生云端调用；开启时对全部去重后的开场
// 文案逐条合成回填缓存。Java 在独立线程池异步执行，Go 由调用方决定是否入协程。
func (g *Gateway) WarmupOpeningAudio(ctx context.Context) {
	if !g.cfg.OpeningAudioWarmupEnabled {
		g.logger.Info("开场音频缓存预热已关闭")
		return
	}
	opening := embeddedOpeningConfig()
	seen := make(map[string]struct{})
	texts := make([]string, 0, len(opening.SkillQuestions)+2)
	for _, text := range opening.SkillQuestions {
		if _, ok := seen[text]; !ok {
			seen[text] = struct{}{}
			texts = append(texts, text)
		}
	}
	for _, text := range []string{opening.AlgorithmQuestion, opening.BackendQuestion} {
		if _, ok := seen[text]; !ok {
			seen[text] = struct{}{}
			texts = append(texts, text)
		}
	}
	for _, text := range texts {
		if isBlank(text) {
			continue
		}
		if wav := g.getOpeningWavAudio(ctx, text); len(wav) > 0 {
			g.openingCache.put(text, wav)
		}
	}
	g.logger.Info("开场音频缓存预热完成", slog.Int("entries", g.openingCache.size()))
}

// readPump 上行读循环（Java 容器回调 handleTextMessage 的驱动源）。
func (g *Gateway) readPump(s *wsSession) {
	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			code, reason := asrCloseInfo(err)
			g.logger.Info("WebSocket 连接已关闭",
				slog.Int64("sessionId", s.id), slog.Int("code", code), slog.String("reason", reason))
			g.cleanup(s)
			return
		}
		s.touchActivity()
		s.handleMessage(data)
	}
}

// handleMessage 上行文本帧分发（Java handleTextMessage L309-358）：audio / control，
// 未知类型告警忽略；处理异常以下行 error 回达（「消息处理失败： …」）。
func (s *wsSession) handleMessage(data []byte) {
	defer func() {
		if r := recover(); r != nil {
			s.gw.logger.Error("处理上行消息异常",
				slog.Int64("sessionId", s.id), slog.Any("panic", r))
			s.SendError(msgErrHandle + "internal error")
		}
	}()

	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		s.SendError(msgErrHandle + err.Error())
		return
	}
	switch envelope.Type {
	case upstreamTypeAudio:
		var m struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			s.SendError(msgErrHandle + err.Error())
			return
		}
		if m.Data == "" {
			s.gw.logger.Warn("收到缺少 data 的音频消息", slog.Int64("sessionId", s.id))
			return
		}
		s.handleUserAudio(m.Data)
	case upstreamTypeControl:
		var cm wsControlMessage
		if err := json.Unmarshal(data, &cm); err != nil {
			s.SendError(msgErrControl + err.Error())
			return
		}
		s.handleControl(cm)
	default:
		s.gw.logger.Warn("未知的消息类型",
			slog.Int64("sessionId", s.id), slog.String("type", envelope.Type))
	}
}

// handleUserAudio 上行音频（Java handleUserAudio L487-544）：AI 说话或回声冷却期间
// 丢弃；解码转发 ASR；未就绪静默丢帧；无会话/写入失败自动重启并重发当前块。
func (s *wsSession) handleUserAudio(b64Audio string) {
	if s.state.isAiSpeakingOrCooldown() {
		return
	}
	pcm, err := base64.StdEncoding.DecodeString(b64Audio)
	if err != nil {
		s.SendError(friendlyAudioError(err))
		return
	}
	if s.asr == nil {
		// 语音识别初始化失败后的音频帧：无处转发，丢弃（Java 侧等价于对已死的
		// 识别器持续走重启恢复路径，行为同样是丢帧+错误提示）。
		s.gw.logger.Warn("语音识别未初始化，丢弃音频帧", slog.Int64("sessionId", s.id))
		return
	}
	if err := s.asr.SendAudio(s.key, pcm); err != nil {
		switch {
		case errors.Is(err, ErrAsrSessionNotReady):
			// ASR 未就绪：静默丢块（Java isAsrNotReady 分支）。
			s.gw.logger.Debug("ASR 就绪前丢弃音频块", slog.Int64("sessionId", s.id))
		case errors.Is(err, ErrAsrNoActiveSession), errors.Is(err, ErrAsrAppendFailed):
			s.recoverAsrAndResend(pcm)
		default:
			s.SendError(friendlyAudioError(err))
		}
	}
}

// recoverAsrAndResend ASR 失效恢复（Java L510-533）：重启 ASR 后重发当前块，
// 至多 cfg.AsrResendMaxAttempts 次 × cfg.AsrResendInterval（Java 15×80ms）；
// 仍失败下发 error 且会话不断。
func (s *wsSession) recoverAsrAndResend(pcm []byte) {
	s.gw.logger.Warn("ASR 推流失败，重启 DashScope 并重发当前块",
		slog.Int64("sessionId", s.id),
		slog.Int("max_attempts", s.gw.cfg.AsrResendMaxAttempts),
		slog.Duration("interval", s.gw.cfg.AsrResendInterval))
	s.asr.Restart(s.key)
	for i := 0; i < s.gw.cfg.AsrResendMaxAttempts; i++ {
		time.Sleep(s.gw.cfg.AsrResendInterval)
		err := s.asr.SendAudio(s.key, pcm)
		if err == nil {
			return
		}
		if errors.Is(err, ErrAsrSessionNotReady) {
			continue
		}
		if errors.Is(err, ErrAsrNoActiveSession) || errors.Is(err, ErrAsrAppendFailed) {
			continue
		}
		// 不可恢复错误：对齐 Java 抛回外层 catch → getErrorMessage → sendError。
		s.SendError(friendlyAudioError(err))
		return
	}
	s.gw.logger.Error("ASR 重启重发后仍失败", slog.Int64("sessionId", s.id))
	s.SendError(msgErrAsrDown)
}

// handleSttResult ASR 识别回调（Java handleSttResult L549-577）：处理中或 AI 说话/
// 冷却期间丢弃迟到结果；部分级只更新实时字幕（isFinal=false）；最终级拼入话语合并
// 缓冲后更新字幕（isFinal 同为 false——isFinal=true 由模型回合提交后下发，见
// Java L713），不自动触发模型。
func (s *wsSession) handleSttResult(text string, isFinal bool) {
	if s.state.isProcessing() || s.state.isAiSpeakingOrCooldown() {
		s.gw.logger.Debug("丢弃迟到的识别结果",
			slog.Int64("sessionId", s.id), slog.Bool("isFinal", isFinal), slog.Int("len", len(text)))
		return
	}
	if !isFinal {
		s.state.markSttActivity()
		s.SendSubtitle(s.state.mergeBufferPreviewWithPartial(text), false)
		return
	}
	s.state.appendFinalSttSegment(text)
	s.SendSubtitle(s.state.mergeBufferPreview(), false)
}

// handleControl 控制消息（Java handleControl L876-899）：submit / end_interview /
// start_phase；处理异常以下行 error 回达（「控制消息处理失败： …」）。
func (s *wsSession) handleControl(cm wsControlMessage) {
	s.gw.logger.Info("收到控制消息",
		slog.Int64("sessionId", s.id),
		slog.String("action", cm.Action), slog.String("phase", cm.Phase))
	switch cm.Action {
	case controlActionSubmit:
		if cm.Data != nil {
			if text, ok := cm.Data["text"].(string); ok && strings.TrimSpace(text) != "" {
				s.state.setMergeBufferDirectly(text)
			}
		}
		s.flushMergedUtteranceToLlm()
	case controlActionEndInterview:
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.gw.svc.EndSession(ctx, s.id); err != nil {
			s.SendError(msgErrControl + err.Error())
			return
		}
	case controlActionStartPhase:
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.gw.svc.StartPhase(ctx, s.id, cm.Phase); err != nil {
			s.SendError(msgErrControl + err.Error())
			return
		}
	default:
		// Java switch 无 default：未知动作静默忽略。
	}
}

// flushMergedUtteranceToLlm 手动提交（Java flushMergedUtteranceToLlm L582-616）：
// CAS 抢处理锁，失败 400ms 后重试（防双答）；取走合并缓冲，空则释放返回；
// 非空则在独立 goroutine 执行模型回合挂钩。
func (s *wsSession) flushMergedUtteranceToLlm() {
	if !s.alive() {
		return
	}
	if !s.state.tryAcquireProcessing() {
		time.AfterFunc(controlSubmitRetryDelay, s.flushMergedUtteranceToLlm)
		return
	}
	userText := s.state.takeMergeBufferAndClear()
	if strings.TrimSpace(userText) == "" {
		s.state.releaseProcessing()
		return
	}
	s.state.setAccumulatedText(userText)
	s.gw.logger.Info("用户话语已合并，触发模型回合",
		slog.Int64("sessionId", s.id), slog.Int("length", len(userText)))

	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.state.setTurnCancel(cancel)
		defer func() {
			// Java finally（L611-614 + L844-847）：释放处理锁、清除线程引用；
			// aiSpeaking=false 并进入回声冷却。
			s.state.releaseProcessing()
			s.state.setTurnCancel(nil)
			cancel()
		}()
		s.runTurn(ctx, userText)
	}()
}

// runTurn 回合挂钩包装（Java triggerLlmResponse L625-627 + finally L844-847）：
// 进入时置 AI 说话（期间麦克风输入与迟到识别结果被丢弃），返回（含 panic）后
// 清除并进入回声冷却。
func (s *wsSession) runTurn(ctx context.Context, userText string) {
	s.state.setAiSpeaking(true)
	defer func() {
		if r := recover(); r != nil {
			s.gw.logger.Error("模型回合执行异常",
				slog.Int64("sessionId", s.id), slog.Any("panic", r))
			s.SendError(msgErrTurn + "internal error")
		}
		s.state.setAiSpeaking(false)
		s.state.endAiSpeaking()
	}()
	hook := s.gw.turn
	if hook == nil {
		hook = defaultTurnHook(s.gw.logger)
	}
	hook(ctx, TurnInput{SessionID: s.id, UserText: userText, Speaker: s})
}

// defaultTurnHook 内置空实现（本票范围）：仅记日志，不发文本/音频（票 06 替换为
// 真实 LLM+TTS 管道）。
func defaultTurnHook(logger *slog.Logger) TurnHook {
	return func(ctx context.Context, in TurnInput) {
		logger.Info("模型回合挂钩未接入（内置空实现），跳过",
			slog.Int64("sessionId", in.SessionID), slog.Int("length", len(in.UserText)))
	}
}

// asrCallbacks ASR 事件接线（Java startDashScopeStt L421-434 的回调组）。
func (s *wsSession) asrCallbacks() AsrCallbacks {
	return AsrCallbacks{
		OnReady: func(string) {
			s.sendJSON(buildAsrReady())
		},
		OnPartial: func(_ string, text string) {
			s.handleSttResult(text, false)
		},
		OnFinal: func(_ string, transcript string) {
			s.handleSttResult(transcript, true)
		},
		OnError: func(_ string, err error) {
			s.gw.logger.Error("STT 错误", slog.Int64("sessionId", s.id), slog.String("error", err.Error()))
			// 就绪监督耗尽（Java checkAsrReadyOrRetry L459-460）与一般识别错误
			// （L427-430）话术不同，按错误形状区分。
			if strings.HasPrefix(err.Error(), asrStillNotReadyPrefix) {
				s.SendError(msgErrAsrReadyTimout)
				return
			}
			s.SendError(msgErrSTT + err.Error())
		},
		OnReconnecting: func(string) {
			s.sendJSON(buildAsrReconnecting())
		},
	}
}

// cleanup 断开收尾（Java afterConnectionClosed L373-400）：注销会话、取消进行中的
// 模型回合（对应 processingThread.interrupt）、关发送通道、停 ASR，并自动结束
// 进行中的会话（EndSessionIfInProgress：COMPLETED + 评估 PENDING + 投递）。
// 被新连接顶替的旧连接不做会话级收尾。
func (g *Gateway) cleanup(s *wsSession) {
	s.closeOnce.Do(func() {
		g.mu.Lock()
		isCurrent := g.live[s.key] == s
		if isCurrent {
			delete(g.live, s.key)
		}
		g.mu.Unlock()

		s.state.cancelTurn()
		s.sink.close()
		if s.asr != nil {
			s.asr.Stop(s.key)
		}
		g.logger.Info("连接清理完成", slog.Int64("sessionId", s.id))
		if !isCurrent {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := g.svc.EndSessionIfInProgress(ctx, s.id); err != nil {
			g.logger.Warn("断开后自动结束会话失败",
				slog.Int64("sessionId", s.id), slog.String("error", err.Error()))
		}
	})
}

// liveSnapshot 活动连接快照（巡检与优雅关闭遍历用，不在锁内回调会话方法）。
func (g *Gateway) liveSnapshot() []*wsSession {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*wsSession, 0, len(g.live))
	for _, s := range g.live {
		out = append(out, s)
	}
	return out
}

// unregister 从活动表移除连接（仅当仍是当前注册连接；暂停超时路径同步移除，
// 对齐 Java handlePauseTimeout L1208-1210 的 sessions/lastActivityTime.remove）。
func (g *Gateway) unregister(s *wsSession) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.live[s.key] == s {
		delete(g.live, s.key)
	}
}

// Close 关闭全部活动连接（优雅关闭，spec 实现决策 20）：仅断开底层连接，会话级
// 收尾（取消回合、停 ASR、自动结束进行中的会话并触发评估）由每条连接的断开路径
// （cleanup）幂等完成，行为等价于逐个断开。
func (g *Gateway) Close() {
	for _, s := range g.liveSnapshot() {
		_ = s.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutdown"),
			time.Now().Add(time.Second))
		_ = s.conn.Close()
	}
}

// ===== wsSession：单连接会话 =====

// wsSession 一条 WebSocket 连接的会话状态与发送通道（实现 TurnSpeaker）。
type wsSession struct {
	gw    *Gateway
	key   string // ASR 会话键 = URI 末段（Java extractSessionId L1223-1226）
	id    int64  // 数字会话 ID（会话域操作用）
	conn  *websocket.Conn
	sink  *wsSink
	state *sessionState
	asr   *AsrClient

	closeOnce   sync.Once
	lastAct     atomic.Int64 // 最近活动 UnixMilli（Java lastActivityTime，票 09 巡检消费）
	warnedPause atomic.Bool  // pause_timeout_warning 已推（同一不活动窗口只推一次，票 09）
}

// abandon 被新连接顶替时作废旧连接：关闭收发（读循环退出后 cleanup 走非当前
// 连接分支，不做会话级收尾）。
func (s *wsSession) abandon() {
	s.closeOnce.Do(func() {
		_ = s.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "superseded"),
			time.Now().Add(time.Second))
		_ = s.conn.Close()
	})
}

// alive 连接是否仍在（Java session.isOpen()）。
func (s *wsSession) alive() bool {
	select {
	case <-s.sink.done:
		return false
	default:
		return true
	}
}

// touchActivity 更新最近活动时间（Java handleTextMessage L331：任何上行消息刷新，
// 票 09 暂停超时消费）；活动刷新同时解除预警武装（同一不活动窗口只推一次预警，
// 下一次不活动窗口重新计）。
func (s *wsSession) touchActivity() {
	s.lastAct.Store(time.Now().UnixMilli())
	s.warnedPause.Store(false)
}

// lastActivityAt 最近活动时刻。
func (s *wsSession) lastActivityAt() time.Time {
	return time.UnixMilli(s.lastAct.Load())
}

// markPauseWarned 预警武装（CAS 保证同一不活动窗口内 30s 巡检只推一次
// pause_timeout_warning；票面对 Java 每 30s 重复推的一次性收敛）。
func (s *wsSession) markPauseWarned() bool {
	return s.warnedPause.CompareAndSwap(false, true)
}

// sendJSON 序列化并下发；序列化失败对齐 Java toJson 返回 "{}" 并继续发送。
func (s *wsSession) sendJSON(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		s.gw.logger.Error("下行消息序列化失败",
			slog.Int64("sessionId", s.id), slog.String("error", err.Error()))
		raw = []byte("{}")
	}
	s.sink.send(raw)
}

// sendJSONSync 序列化并同步下发（返回前已写出）：暂停超时等必须先送达再断连的
// 管理消息使用；序列化失败同样按 "{}" 发送。
func (s *wsSession) sendJSONSync(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		s.gw.logger.Error("下行消息序列化失败",
			slog.Int64("sessionId", s.id), slog.String("error", err.Error()))
		raw = []byte("{}")
	}
	_ = s.sink.sendSync(raw)
}

// ===== TurnSpeaker 实现（下行六类） =====

// SendSubtitle 下发字幕。
func (s *wsSession) SendSubtitle(text string, isFinal bool) { s.sendJSON(buildSubtitle(text, isFinal)) }

// SendText 下发流式/最终文本。
func (s *wsSession) SendText(content string, final bool) {
	s.sendJSON(buildTextMessage(content, final))
}

// SendAudio 下发整段 WAV（base64 封装，Java sendAudio L911-923）。
func (s *wsSession) SendAudio(wav []byte, text string) {
	if !s.alive() {
		return
	}
	s.sendJSON(buildAudioMessage(base64.StdEncoding.EncodeToString(wav), text))
}

// SendAudioChunk 下发分句音频块（Java sendAudioChunk L957-969）。
func (s *wsSession) SendAudioChunk(wav []byte, index int, isLast bool) {
	if !s.alive() {
		return
	}
	s.sendJSON(buildAudioChunkMessage(base64.StdEncoding.EncodeToString(wav), index, isLast))
}

// SendAudioComplete 回合音频下发完毕（Java sendAudioComplete L971-981）。
func (s *wsSession) SendAudioComplete() { s.sendJSON(buildAudioComplete()) }

// SendError 下发错误（Java sendError L937-939）。
func (s *wsSession) SendError(message string) { s.sendJSON(buildErrorMessage(message)) }

// sendControlStatus 下发 control 状态消息（Java sendAsrStatus L945-955；暂停超时
// 两个提示由票 09 的巡检调用）。
func (s *wsSession) sendControlStatus(action, message string) {
	s.sendJSON(buildControlStatus(action, message))
}

// SendPauseTimeoutWarning 不活动预警（票 09 消费）。
func (s *wsSession) SendPauseTimeoutWarning() { s.sendJSON(buildPauseTimeoutWarning()) }

// SendPauseTimeout 不活动暂停提示（票 09 消费）。
func (s *wsSession) SendPauseTimeout() { s.sendJSON(buildPauseTimeout()) }

// ===== 发送保护（Java ConcurrentWebSocketSessionDecorator(10_000ms, 512KB) 同构） =====

// wsSinkParams 发送保护参数。
type wsSinkParams struct {
	conn         *websocket.Conn
	limit        int64         // 积压字节上限（Java WS_SEND_BUFFER_LIMIT_BYTES = 512KB）
	writeTimeout time.Duration // 单条写超时（Java WS_SEND_TIME_LIMIT_MS = 10s）
	logger       *slog.Logger
}

// wsSink 出站发送保护：消息入队后由专职写协程按序写出，逐条写超时；积压超限或
// 写失败视为连接不可靠并关闭；关闭后的发送安全丢弃。gorilla 不允许并发写，
// 单写协程天然满足。
type wsSink struct {
	conn         *websocket.Conn
	ch           chan []byte
	done         chan struct{}
	once         sync.Once
	writeMu      sync.Mutex // 串行化 writeLoop 与 sendSync 的底层写（gorilla 不允许并发写）
	pending      atomic.Int64
	limit        int64
	writeTimeout time.Duration
	logger       *slog.Logger
}

func newWSSink(p wsSinkParams) *wsSink {
	s := &wsSink{
		conn:         p.conn,
		ch:           make(chan []byte, wsOutboundQueueDepth),
		done:         make(chan struct{}),
		limit:        p.limit,
		writeTimeout: p.writeTimeout,
		logger:       p.logger,
	}
	if s.limit <= 0 {
		s.limit = 512 * 1024
	}
	if s.writeTimeout <= 0 {
		s.writeTimeout = 10 * time.Second
	}
	go s.writeLoop()
	return s
}

// send 入队一条下行消息；已关闭安全丢弃，积压超限按不可靠连接关闭（Java
// ConcurrentWebSocketSessionDecorator 的 bufferLimit 语义：超限关闭会话）。
func (s *wsSink) send(msg []byte) {
	select {
	case <-s.done:
		return // 会话关闭后安全丢弃
	default:
	}
	if s.pending.Add(int64(len(msg))) > s.limit {
		s.pending.Add(-int64(len(msg)))
		s.logger.Warn("下行积压超限，关闭连接", slog.Int64("limit", s.limit))
		s.close()
		return
	}
	select {
	case s.ch <- msg:
	default:
		s.pending.Add(-int64(len(msg)))
		s.logger.Warn("下行队列已满，关闭连接", slog.Int("depth", wsOutboundQueueDepth))
		s.close()
	}
}

// writeLoop 专职写协程：逐条写超时，失败关闭连接（读循环随即退出并走断开清理）。
func (s *wsSink) writeLoop() {
	for {
		select {
		case msg := <-s.ch:
			s.pending.Add(-int64(len(msg)))
			s.writeMu.Lock()
			_ = s.conn.SetWriteDeadline(time.Now().Add(s.writeTimeout))
			err := s.conn.WriteMessage(websocket.TextMessage, msg)
			s.writeMu.Unlock()
			if err != nil {
				s.logger.Warn("下行消息发送失败，关闭连接", slog.String("error", err.Error()))
				s.close()
				return
			}
		case <-s.done:
			return
		}
	}
}

// sendSync 同步直写一条管理消息（绕过出站队列，返回前已写出）：暂停超时提示等
// 「必须先送达再断连」的管理消息使用，对齐 Java sendMessage 的同步发送语义
// （ConcurrentWebSocketSessionDecorator）。已关闭安全拒绝；写超时沿配置。
func (s *wsSink) sendSync(msg []byte) error {
	select {
	case <-s.done:
		return errSinkClosed
	default:
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.writeTimeout))
	return s.conn.WriteMessage(websocket.TextMessage, msg)
}

// errSinkClosed 发送通道已关闭（sendSync 拒绝写入）。
var errSinkClosed = errors.New("voiceinterview: 发送通道已关闭")

// close 关闭发送通道与底层连接（幂等）。
func (s *wsSink) close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.conn.Close() // 解除写阻塞；读循环退出后由网关做断开清理
	})
}

// isClosed 是否已关闭（测试与 alive 判定用）。
func (s *wsSink) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// ===== 错误话术映射 =====

// friendlyAudioError 音频处理异常 → 友话术（Java getErrorMessage L853-871）：
// 沿错误链匹配 403/ACCESS_DENIED 与 timeout/channel inactive，默认「语音处理失败：」前缀。
func friendlyAudioError(err error) string {
	chain := ""
	for e := err; e != nil; e = errors.Unwrap(e) {
		chain += e.Error() + "\n"
	}
	if strings.Contains(chain, "403") || strings.Contains(chain, "ACCESS_DENIED") {
		return "阿里云语音服务认证失败：AccessKey 无效或已过期。请在 .env 文件中配置正确的 ALIYUN_ACCESS_KEY"
	}
	if strings.Contains(chain, "timeout") || strings.Contains(chain, "channel inactive") {
		return "阿里云语音服务连接超时。请检查网络连接或稍后重试"
	}
	return msgErrAudioPrefix + err.Error()
}
