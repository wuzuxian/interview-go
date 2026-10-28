// LLM 回合管道（Java VoiceInterviewWebSocketHandler.triggerLlmResponse L625-848
// 流式路径 + OrderedTtsChunkEmitter 编排的 Go 实现），经票 04 的回合挂钩接入：
// 会话实体校验 → 历史装载 → prompt 组装 → eino 流式出句（逐句并发 TTS）→
// subtitle(final) → text(final) → 消息落库 → 分块/整段音频下发。
//
// 与 Java 的差异（票面裁决）：LLM 失败不把友好话术当作回复朗读/落库，而是下发
// error 消息后会话保持进行中（处理锁由网关 finally 释放、回声冷却照常生效）。
package voiceinterview

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/wuzuxian/interview/internal/resume"
)

// voiceHistoryWindowSize 本票历史窗口（Java context-compression.window-size=20 代码
// 默认值）；票 07 换有界压缩加载后由压缩配置接管。
const voiceHistoryWindowSize = 20

// msgErrSessionMissing 会话缺失话术（Java triggerLlmResponse L655 逐字）。
const msgErrSessionMissing = "会话不存在，请重新开始面试"

// ResumeTextSource 简历文本读取（Java ResumeRepository.findById → getResumeText；
// 生产实现 *resume.Repo，测试注入 stub）。
type ResumeTextSource interface {
	FindByID(ctx context.Context, id int64) (*resume.ResumeEntity, error)
}

// TurnPipelineDeps 回合管道依赖聚合。
type TurnPipelineDeps struct {
	Svc      *Service           // 会话域（实体读取 + 消息落库）
	Store    SessionStore       // 最近窗口历史读取
	Models   VoiceModelResolver // 流式模型解析接缝
	Settings VoiceConfigSource  // TTS 运行时配置（每回合重读，热生效）
	TTS      *TtsClient         // TTS 协议客户端（无接缝：URL 指向假服务端即打真协议）
	Prompts  *PromptService     // prompt 组装
	Resumes  ResumeTextSource   // 简历文本；可 nil（跳过简历注入）
	// History 上下文压缩加载器（票 07；生产必注入，nil 时回退票 06 固定窗口读取，
	// 仅供测试零值场景）。
	History *HistoryLoader
	Cfg     Config
	Logger  *slog.Logger
}

// TurnPipeline 模型回合管道（TurnHook 的生产实现）。
type TurnPipeline struct {
	svc      *Service
	store    SessionStore
	models   VoiceModelResolver
	settings VoiceConfigSource
	tts      *TtsClient
	prompts  *PromptService
	resumes  ResumeTextSource
	history  *HistoryLoader
	cfg      Config
	logger   *slog.Logger
}

// NewTurnPipeline 创建回合管道。
func NewTurnPipeline(deps TurnPipelineDeps) *TurnPipeline {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &TurnPipeline{
		svc: deps.Svc, store: deps.Store, models: deps.Models, settings: deps.Settings,
		tts: deps.TTS, prompts: deps.Prompts, resumes: deps.Resumes, history: deps.History,
		cfg: deps.Cfg, logger: deps.Logger,
	}
}

// aliveSpeaker 可选活性检查（Java session.isOpen()；*wsSession 实现 alive()，
// 测试替身未实现时视为存活）。
type aliveSpeaker interface{ alive() bool }

func speakerAlive(sp TurnSpeaker) bool {
	if a, ok := sp.(aliveSpeaker); ok {
		return a.alive()
	}
	return true
}

// Turn 执行一整轮模型回复（TurnHook 签名；进入前网关已置 AI 说话标志，返回后
// 网关释放处理锁并进入回声冷却）。非分块路径不发 audio_complete（Java 逐字）。
func (p *TurnPipeline) Turn(ctx context.Context, in TurnInput) {
	if !speakerAlive(in.Speaker) {
		p.logger.Warn("连接已关闭，跳过模型回合", slog.Int64("sessionId", in.SessionID))
		return
	}
	if strings.TrimSpace(in.UserText) == "" {
		p.logger.Warn("用户话语为空，跳过模型回合", slog.Int64("sessionId", in.SessionID))
		return
	}
	p.logger.Info("开始生成模型回复",
		slog.Int64("sessionId", in.SessionID), slog.Int("textLength", len(in.UserText)))

	// 会话实体（Java getSessionEntity L1250-1260：缺失 → 话术 error）。
	entity, err := p.svc.GetSessionEntity(ctx, in.SessionID)
	if err != nil {
		p.logger.Error("查询会话实体失败",
			slog.Int64("sessionId", in.SessionID), slog.String("error", err.Error()))
		in.Speaker.SendError(msgErrTurn + err.Error())
		return
	}
	if entity == nil {
		p.logger.Error("会话实体不存在，无法生成回复", slog.Int64("sessionId", in.SessionID))
		in.Speaker.SendError(msgErrSessionMissing)
		return
	}

	// TTS 运行时配置（每回合重读，设置页修改热生效）。
	ttsSettings, err := p.settings.VoiceTtsSettings(ctx)
	if err != nil {
		p.logger.Error("读取语音合成配置失败",
			slog.Int64("sessionId", in.SessionID), slog.String("error", err.Error()))
		in.Speaker.SendError(mapLLMErrorToUserMessage(err))
		return
	}

	history := p.loadHistory(ctx, in.SessionID, entity)
	systemPrompt := p.prompts.SystemPrompt(valueOfVoice(entity.SkillID), p.resumeText(ctx, entity))
	userPrompt := p.prompts.UserPrompt(in.UserText, history)

	model, err := p.models.VoiceStreamModelFor(ctx, valueOfVoice(entity.LLMProvider))
	if err != nil {
		p.logger.Error("解析流式模型失败",
			slog.Int64("sessionId", in.SessionID),
			slog.String("provider", valueOfVoice(entity.LLMProvider)), slog.String("error", err.Error()))
		in.Speaker.SendError(mapLLMErrorToUserMessage(err))
		return
	}

	// 逐句 TTS 编排（Java L663-672）：分块开关开 → 有序发射器；关 → 并发收集后合并。
	ttsTimeout := p.cfg.TTSTimeout
	if ttsTimeout < 5*time.Second {
		ttsTimeout = 5 * time.Second // Java Math.max(5, ttsTimeoutSeconds)
	}
	maxConcurrency := p.cfg.MaxConcurrentTTSPerSession
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	sem := make(chan struct{}, maxConcurrency)
	var futures []chan ttsOutcome
	var emitter *ttsChunkEmitter
	if p.cfg.ChunkedAudioEnabled {
		emitter = newTTSChunkEmitter(ttsEmitterParams{
			SessionID: in.SessionID, Ctx: ctx, Speaker: in.Speaker, TTS: p.tts, Settings: ttsSettings,
			Timeout: ttsTimeout, MaxConcurrency: maxConcurrency, Logger: p.logger,
		})
	}

	onSentence := func(sentence string) {
		if sentence == "" {
			return
		}
		if emitter != nil {
			emitter.Submit(sentence)
			return
		}
		sem <- struct{}{}
		fut := make(chan ttsOutcome, 1)
		futures = append(futures, fut)
		go func() {
			defer func() { <-sem }()
			pcm, err := p.tts.Synthesize(ctx, ttsSettings, sentence)
			fut <- ttsOutcome{pcm: pcm, err: err}
		}()
	}
	onToken := func(partial string) {
		if partial == "" || !speakerAlive(in.Speaker) {
			return
		}
		in.Speaker.SendText(partial, false)
	}

	aiReply, err := streamSentences(ctx, model, streamParams{
		SystemPrompt: systemPrompt, UserPrompt: userPrompt,
		PushInterval: p.cfg.LLMStreamPushInterval, MinCharsDelta: p.cfg.LLMStreamMinCharsDelta,
		MaxChars: p.cfg.AIQuestionMaxChars,
		OnToken:  onToken, OnSentence: onSentence,
	})
	if err != nil {
		// 票面契约：LLM 异常映射友好话术、下发 error、会话不中断
		//（Java 把话术当回复继续朗读/落库，见文件头差异说明）。
		p.logger.Error("模型流式调用失败",
			slog.Int64("sessionId", in.SessionID), slog.String("error", err.Error()))
		if speakerAlive(in.Speaker) {
			in.Speaker.SendError(mapLLMErrorToUserMessage(err))
		}
		return
	}
	p.logger.Info("模型回复已生成",
		slog.Int64("sessionId", in.SessionID), slog.Int("replyLength", len([]rune(aiReply))))

	// 流式期间断连：丢弃回复（Java L696-699）。
	if !speakerAlive(in.Speaker) {
		p.logger.Warn("WebSocket 已关闭，丢弃模型回复", slog.Int64("sessionId", in.SessionID))
		return
	}

	// 完成顺序契约：subtitle(final) → text(final) → 消息落库 → 音频（Java L712-715）。
	in.Speaker.SendSubtitle(in.UserText, true)
	in.Speaker.SendText(aiReply, true)
	p.svc.SaveTurnMessage(ctx, in.SessionID, in.UserText, aiReply)

	if emitter != nil {
		p.deliverChunked(in.Speaker, emitter, ttsSettings, aiReply)
		return
	}
	p.deliverMerged(ctx, in.SessionID, in.Speaker, futures, ttsSettings, aiReply)
}

// deliverChunked 分块路径（Java L717-737）：finish 声明句子提交完毕（totalChunks
// 定界）→ 等发射器全部处理完；零句成功 → 整段兜底合成单条 audio（失败仅告警）。
func (p *TurnPipeline) deliverChunked(speaker TurnSpeaker, emitter *ttsChunkEmitter, settings TtsSettings, aiReply string) {
	emitter.Finish()
	emitted := emitter.AwaitCompletion()
	p.logger.Info("分句音频已按序下发",
		slog.Int("emitted", emitted))
	if emitted > 0 || !speakerAlive(speaker) {
		return
	}
	p.logger.Info("流式分句音频全部失败，回退整段合成")
	p.fallbackFullAudio(context.Background(), speaker, settings, aiReply)
}

// deliverMerged 合并路径（分块开关关闭，Java L738-800）：按句序收集结果（单句
// 超时计失败），全部成功 PCM 合并为单条 audio；全失败且确有失败 → 整段兜底；
// 全空 → 仅记日志。不发 audio_complete（Java 逐字）。
func (p *TurnPipeline) deliverMerged(ctx context.Context, sessionID int64, speaker TurnSpeaker, futures []chan ttsOutcome, settings TtsSettings, aiReply string) {
	timeout := p.cfg.TTSTimeout
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	var pcms [][]byte
	totalSize := 0
	failedCount := 0
	for _, fut := range futures {
		select {
		case out := <-fut:
			if out.err == nil && len(out.pcm) > 0 {
				pcms = append(pcms, out.pcm)
				totalSize += len(out.pcm)
			} else {
				failedCount++
			}
		case <-time.After(timeout):
			failedCount++
		}
	}
	if !speakerAlive(speaker) {
		return
	}

	if totalSize == 0 && failedCount > 0 {
		p.logger.Info("全部句级 TTS 失败，回退整段合成", slog.Int("failed", failedCount))
		p.fallbackFullAudio(ctx, speaker, settings, aiReply)
		return
	}
	if totalSize > 0 {
		merged := make([]byte, 0, totalSize)
		for _, pcm := range pcms {
			merged = append(merged, pcm...)
		}
		p.logger.Info("整段音频已合并下发",
			slog.Int("sentences", len(pcms)), slog.Int("pcmBytes", totalSize))
		speaker.SendAudio(PcmToWav(merged), aiReply)
		return
	}
	p.logger.Error("全部 TTS 调用返回空音频", slog.Int64("sessionId", sessionID))
}

// fallbackFullAudio 整段兜底合成（Java L724-734：失败仅 warn，不阻塞收尾）。
func (p *TurnPipeline) fallbackFullAudio(ctx context.Context, speaker TurnSpeaker, settings TtsSettings, text string) {
	pcm, err := p.tts.SynthesizeFull(ctx, settings, text)
	if err != nil {
		p.logger.Warn("整段兜底合成失败", slog.String("error", err.Error()))
		return
	}
	if len(pcm) > 0 {
		speaker.SendAudio(PcmToWav(pcm), text)
	}
}

// resumeText 简历文本（Java buildPromptContext L161-167：resumeId 非空 → 查简历，
// 记录缺失视为无简历；查询失败上抛走 LLM 错误路径）。
func (p *TurnPipeline) resumeText(ctx context.Context, entity *SessionEntity) string {
	if entity.ResumeID == nil || p.resumes == nil {
		return ""
	}
	r, err := p.resumes.FindByID(ctx, *entity.ResumeID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "" // Java orElse(null)：简历不存在按无简历处理
		}
		p.logger.Error("读取候选人简历失败",
			slog.Int64("sessionId", entity.ID),
			slog.Int64("resumeId", *entity.ResumeID), slog.String("error", err.Error()))
		return ""
	}
	if r == nil || r.ResumeText == nil {
		return ""
	}
	return *r.ResumeText
}

// loadHistory 历史装载（Java Handler getHistory L1231-1243：经 VoiceHistoryLoader
// 有界压缩装配，loader 内部已含降级链——有界失败→最近窗口→空列表，回合不中断）。
// 无压缩加载器时回退票 06 的固定窗口读取。
func (p *TurnPipeline) loadHistory(ctx context.Context, sessionID int64, entity *SessionEntity) []string {
	if p.history != nil {
		return p.history.LoadHistory(ctx, sessionID, valueOfVoice(entity.LLMProvider))
	}
	msgs, err := p.store.FindRecentDialogueMessages(ctx, sessionID, voiceHistoryWindowSize)
	if err != nil {
		p.logger.Error("加载语音面试历史失败",
			slog.Int64("sessionId", sessionID), slog.String("error", err.Error()))
		return nil
	}
	return formatTurnLines(msgs)
}
