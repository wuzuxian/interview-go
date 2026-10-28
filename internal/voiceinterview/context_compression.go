// 上下文压缩（票 07；Java context/VoiceContextCompressor + context/VoiceHistoryLoader
// + VoiceInterviewService.saveSummaryRow 的逐字移植）：
//
//	有界历史加载 = SUMMARY 摘要行 + 摘要边界后最近 windowSize 轮原文 + 至多 1 个
//	待摘要批（summaryBatchSize 条），数据库侧读取量有界，不再全量读取长会话历史。
//	NONE=不摘要不裁窗口但仍有字符硬预算；WINDOW=窗口+字符硬预算；
//	SUMMARY=摘要+窗口+字符硬预算（生产默认）。enabled=false 是唯一关闭方式：
//	返回全量历史，仅用于本地排查。
//	SUMMARY 摘要用与语音面试官独立的普通模型调用（非流式、无工具、无对话记忆注入，
//	对齐 Java LlmProviderRegistry.getPlainChatClient），prompt 模板从 Java 原字节拷贝。
//	新摘要边界在会话行悲观锁（SELECT ... FOR UPDATE）内写回（SUMMARY 消息行 +
//	summary_covered_sequence_num）；摘要失败保留旧摘要或降级 WINDOW，永不回退
//	全量历史、不中断当前回合。
//
// 所有长度/下标一律按字符（rune）计，对齐 Java String 的 char 语义（先例 llm_clean.go）。
package voiceinterview

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"strings"

	"github.com/cloudwego/eino/schema"
)

// 压缩模式（Java VoiceInterviewProperties.Mode 枚举值逐字）。
const (
	ModeNone    = "NONE"
	ModeWindow  = "WINDOW"
	ModeSummary = "SUMMARY"
)

// ContextCompressionConfig 上下文压缩配置（Java application.yml
// app.voice-interview.context-compression.* 段与
// VoiceInterviewProperties.ContextCompressionConfig 代码默认值逐字）。
type ContextCompressionConfig struct {
	// Enabled 是否启用上下文压缩。enabled=false 是唯一关闭方式：返回全量历史
	//（仅本地排查）；enabled=true 时无论何种模式都执行字符硬预算。
	Enabled bool `config:"enabled" default:"true"`
	// Mode 压缩模式：NONE / WINDOW / SUMMARY（生产默认）。
	Mode string `config:"mode" default:"SUMMARY"`
	// WindowSize 保留的最近轮次数量（滑动窗口大小）。SUMMARY 模式在两次摘要批次
	// 之间最多保留 windowSize + summaryBatchSize - 1 轮，轮数有界；Prompt 的最终
	// 硬上限由 MaxHistoryChars 字符预算保证。
	WindowSize int `config:"window_size" default:"20"`
	// SummaryBatchSize 早期轮次每越过该批次数，触发一次增量摘要合并
	//（降低 LLM 摘要调用频率）。
	SummaryBatchSize int `config:"summary_batch_size" default:"10"`
	// MaxHistoryChars 历史总字符预算：摘要字符数 + 近期历史格式化后字符数 <= 预算。
	MaxHistoryChars int `config:"max_history_chars" default:"12000"`
	// MaxSummaryChars 摘要字符上限：超出时从尾部截断。
	MaxSummaryChars int `config:"max_summary_chars" default:"4000"`
}

// ErrSummaryBoundaryRegressed 摘要覆盖边界回退被拒绝（Java saveSummaryRow 锁内
// 校验后 warn+return，非错误；调用方静默继续）。
var ErrSummaryBoundaryRegressed = errors.New("voiceinterview: 摘要覆盖边界回退被拒绝")

// 摘要模板（Java prompts/voice-interview-context-summary.st 原字节拷贝）。
//
//go:embed assets/voice-interview-context-summary.st
var voiceContextSummaryTemplate string

// renderSummaryPrompt 摘要模板渲染（Spring PromptTemplate 的 {占位符} 替换语义，
// 先例同 interview 包 renderPrompt）。
func renderSummaryPrompt(previousSummary, newTurns string) string {
	p := strings.ReplaceAll(voiceContextSummaryTemplate, "{previousSummary}", previousSummary)
	return strings.ReplaceAll(p, "{newTurns}", newTurns)
}

// CompressedHistory 压缩结果（Java record CompressedHistory）。
type CompressedHistory struct {
	// Summary 早期轮次的滚动摘要（可为 nil）。
	Summary *string
	// Recent 保留的近期轮次实体（窗口内，预算裁剪后）。
	Recent []MessageEntity
	// CoveredTurns 已被摘要覆盖的轮次数。
	CoveredTurns int
	// Changed 摘要是否较缓存发生变化（需持久化新边界）。
	Changed bool
}

// Compressor 语音面试上下文压缩器（Java VoiceContextCompressor）。
type Compressor struct {
	cfg    ContextCompressionConfig
	models VoicePlainModelResolver
	logger *slog.Logger
}

// NewCompressor 创建压缩器；models 为摘要专用的普通模型解析接缝。
func NewCompressor(cfg ContextCompressionConfig, models VoicePlainModelResolver, logger *slog.Logger) *Compressor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Compressor{cfg: cfg, models: models, logger: logger}
}

// Compress 压缩对话历史（Java VoiceContextCompressor.compress L64-125 逐字）。
// turns 为全部对话轮次（不含 SUMMARY 行），按 sequenceNum 升序；coveredTurns 为
// 已被摘要覆盖的轮次数（增量合并用，避免重复摘要）。
func (c *Compressor) Compress(ctx context.Context, turns []MessageEntity, cachedSummary *string, coveredTurns int, llmProvider string) CompressedHistory {
	cfg := c.cfg
	// enabled=false 是唯一关闭方式：不压缩、不做字符硬预算，返回全量（仅本地排查）。
	if !cfg.Enabled {
		return CompressedHistory{Recent: turns, CoveredTurns: len(turns)}
	}

	// NONE 模式不摘要、不裁窗口，但仍执行字符硬预算。
	if cfg.Mode == ModeNone {
		return CompressedHistory{Recent: c.applyCharBudget(nil, turns)}
	}

	if cfg.Mode == ModeWindow {
		start := max(0, len(turns)-cfg.WindowSize)
		return CompressedHistory{Recent: c.applyCharBudget(nil, turns[start:])}
	}

	// Loader 已排除摘要覆盖过的消息；局部列表在窗口内不代表没有旧摘要。
	if len(turns) <= cfg.WindowSize {
		summary := truncateSummaryText(trimPtrToNull(cachedSummary), cfg.MaxSummaryChars)
		return CompressedHistory{Summary: summary, Recent: c.applyCharBudget(summary, turns)}
	}

	total := len(turns)
	window := cfg.WindowSize
	earlyCount := total - window
	summary := cachedSummary
	changed := false
	summaryFailed := false
	effectiveCoveredTurns := 0
	if trimPtrToNull(cachedSummary) != nil {
		effectiveCoveredTurns = min(max(coveredTurns, 0), earlyCount)
	}

	if earlyCount > effectiveCoveredTurns && earlyCount-effectiveCoveredTurns >= cfg.SummaryBatchSize {
		// 仅对「尚未覆盖的早期轮次」做增量摘要合并，避免每轮都调用 LLM；
		// earlyCount > effectiveCoveredTurns 防御上游脏数据（如被损坏的 SUMMARY 行）。
		earlyTurns := formatTurnLines(turns[effectiveCoveredTurns:earlyCount])
		newSummary := c.summarize(ctx, cachedSummary, earlyTurns, llmProvider)
		if newSummary == nil {
			// 摘要生成失败：有旧摘要保留旧摘要，无旧摘要降级 WINDOW；不允许恢复为全量历史
			summaryFailed = true
		} else {
			// 正文相同也表示本批已处理；changed 同时表示覆盖边界需要持久化。
			summary = newSummary
			effectiveCoveredTurns = earlyCount
			changed = true
		}
	}

	recentStart := earlyCount
	if !summaryFailed {
		recentStart = effectiveCoveredTurns
	}
	budgetedSummary := truncateSummaryText(summary, cfg.MaxSummaryChars)
	recent := c.applyCharBudget(budgetedSummary, turns[recentStart:])
	return CompressedHistory{Summary: budgetedSummary, Recent: recent, CoveredTurns: effectiveCoveredTurns, Changed: changed}
}

// truncateSummaryText 摘要超过 maxSummaryChars 时从尾部截断并附加标记
//（Java truncateSummary L130-141 逐字）。
func truncateSummaryText(summary *string, maxChars int) *string {
	if summary == nil {
		return nil
	}
	if maxChars <= 0 || runeLen(*summary) <= maxChars {
		return summary
	}
	marker := "(摘要已截断)"
	keep := max(0, maxChars-runeLen(marker))
	t := string([]rune(*summary)[:keep]) + marker
	return &t
}

// applyCharBudget 字符硬预算：摘要字符数 + 近期历史格式化后字符数 <= maxHistoryChars。
// 超出时从最早的近期消息开始删除，始终保留最近消息（Java applyCharBudget L147-178 逐字）。
func (c *Compressor) applyCharBudget(summary *string, recent []MessageEntity) []MessageEntity {
	maxChars := c.cfg.MaxHistoryChars
	if maxChars <= 0 || len(recent) == 0 {
		return recent
	}
	used := 0
	if summary != nil {
		used = runeLen(*summary)
	}
	start := len(recent)
	for start > 0 {
		cost := formattedCharCost(recent[start-1])
		if used+cost > maxChars {
			break
		}
		used += cost
		start--
	}
	if start == 0 {
		return recent
	}
	summaryChars := 0
	if summary != nil {
		summaryChars = runeLen(*summary)
	}
	if start == len(recent) {
		// 连最近一条都放不下：保留最近消息的身份，按剩余预算截断其文本，硬上限不被绕过
		truncated := truncateMessageTail(recent[len(recent)-1], maxChars-summaryChars)
		c.logger.Info("最近消息超出字符预算已截断",
			slog.Int("summaryChars", summaryChars), slog.Int("budget", maxChars),
			slog.Int("inputTurns", len(recent)), slog.Int("outputTurns", 1))
		return []MessageEntity{truncated}
	}
	c.logger.Info("上下文超出字符预算，丢弃最早近期消息",
		slog.Int("inputTurns", len(recent)), slog.Int("outputTurns", len(recent)-start),
		slog.Int("summaryChars", summaryChars), slog.Int("budget", maxChars))
	return recent[start:]
}

// truncateMessageTail 截断单条消息文本以适配剩余预算，保留尾部语义（候选人最后的
// 表述更接近当前语境）。返回副本，不修改原实体（Java truncateMessageTail L184-208 逐字）。
func truncateMessageTail(m MessageEntity, remaining int) MessageEntity {
	aiText := trimPtrToNull(m.AiGeneratedText)
	userText := trimPtrToNull(m.UserRecognizedText)
	aiLen, userLen := 0, 0
	if aiText != nil {
		aiLen = runeLen("面试官：") + runeLen(*aiText)
	}
	if userText != nil {
		userLen = runeLen("候选人：") + runeLen(*userText)
	}
	budget := max(remaining, 0)

	// 优先保留候选人回答（含前缀），再给面试官问题分配剩余空间，两者都从尾部保留
	userKeep := min(userLen, budget)
	aiKeep := min(aiLen, budget-userKeep)

	cp := MessageEntity{
		SequenceNum: m.SequenceNum,
		MessageType: m.MessageType,
	}
	if aiText != nil {
		t := keepTail(*aiText, max(aiKeep-runeLen("面试官："), 0))
		cp.AiGeneratedText = &t
	}
	if userText != nil {
		t := keepTail(*userText, max(userKeep-runeLen("候选人："), 0))
		cp.UserRecognizedText = &t
	}
	return cp
}

// keepTail 从尾部保留 keepChars 个字符（Java keepTail L210-215 逐字）。
func keepTail(text string, keepChars int) string {
	runes := []rune(text)
	if keepChars >= len(runes) {
		return text
	}
	if keepChars <= 0 {
		return ""
	}
	return string(runes[len(runes)-keepChars:])
}

// formattedCharCost 单条轮次格式化后的字符成本，与 formatTurnLines 的输出一致：
// 每个非空文本都会以「面试官：/候选人：」前缀各占一行（Java L221-232 逐字）。
func formattedCharCost(m MessageEntity) int {
	cost := 0
	if aiText := trimPtrToNull(m.AiGeneratedText); aiText != nil {
		cost += runeLen("面试官：") + runeLen(*aiText)
	}
	if userText := trimPtrToNull(m.UserRecognizedText); userText != nil {
		cost += runeLen("候选人：") + runeLen(*userText)
	}
	return cost
}

// formatRecentWithinBudget 先应用字符硬预算，再格式化最近窗口；用于加载失败时的
// 有界降级路径（Java formatRecentWithinBudget L272-274）。
func (c *Compressor) formatRecentWithinBudget(turns []MessageEntity) []string {
	return formatTurnLines(c.applyCharBudget(nil, turns))
}

// summarize 将早期轮次增量合并进已有摘要（Java summarize L279-300 逐字）：
// 无工具、无对话记忆注入的普通模型调用；生成失败返回 nil，由调用方降级为最近窗口，
// 不阻塞主链路。
func (c *Compressor) summarize(ctx context.Context, prevSummary *string, earlyTurns []string, llmProvider string) *string {
	if len(earlyTurns) == 0 {
		return prevSummary
	}
	// 前情摘要为空时模板占位取 "(空)"（Java 逐字）。
	prev := "(空)"
	if prevSummary != nil {
		prev = *prevSummary
	}
	prompt := renderSummaryPrompt(prev, strings.Join(earlyTurns, "\n"))
	cm, err := c.models.VoicePlainCompleterFor(ctx, llmProvider)
	if err != nil {
		c.logger.Warn("上下文摘要生成失败", slog.String("error", err.Error()))
		return nil
	}
	msg, err := cm.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)})
	if err != nil {
		c.logger.Warn("上下文摘要生成失败", slog.String("error", err.Error()))
		return nil
	}
	content := ""
	if msg != nil {
		content = msg.Content
	}
	if strings.TrimSpace(content) == "" {
		return nil
	}
	t := strings.TrimSpace(content)
	return &t
}

// ===== 有界历史加载器（Java context/VoiceHistoryLoader L29-175）=====

// CompressionStore 上下文压缩的数据面接缝（生产实现 *Repo，测试注入内存替身；
// 先例同评估链路 EvalStore）。
type CompressionStore interface {
	// FindSummaryRow 会话第一条（sequence_num 升序）SUMMARY 行；无行返回 (nil, nil)。
	FindSummaryRow(ctx context.Context, sessionID int64) (*MessageEntity, error)
	// FindRecentDialogueMessages 最近 limit 条非 SUMMARY 消息（升序）。
	FindRecentDialogueMessages(ctx context.Context, sessionID int64, limit int) ([]MessageEntity, error)
	// FindPendingSummaryBatch 摘要边界后、最近窗口前的待摘要批（升序，
	// sequence_num ∈ (after, before)，至多 limit 条）。
	FindPendingSummaryBatch(ctx context.Context, sessionID int64, after, before, limit int) ([]MessageEntity, error)
	// FindDialogueMessageAt 升序第 offset 条（0 起）非 SUMMARY 消息（旧摘要边界
	// 迁移专用，OFFSET/LIMIT 1 定位）；无行返回 (nil, nil)。
	FindDialogueMessageAt(ctx context.Context, sessionID int64, offset int) (*MessageEntity, error)
	// FindDialogueMessages 全量非 SUMMARY 消息（升序；enabled=false 本地排查分支）。
	FindDialogueMessages(ctx context.Context, sessionID int64) ([]MessageEntity, error)
	// SaveSummaryRow 摘要行锁内写回；边界回退返回 ErrSummaryBoundaryRegressed，
	// 会话不存在返回 ErrSessionNotFound。
	SaveSummaryRow(ctx context.Context, sessionID int64, summary *string, coveredSequenceNum int) error
}

// HistoryLoader 语音面试上下文有界加载器（Java VoiceHistoryLoader）：
// 单次最多读取「最近窗口 + 一个摘要批次」，保证数据库侧读取量有界。
type HistoryLoader struct {
	store      CompressionStore
	compressor *Compressor
	cfg        ContextCompressionConfig
	logger     *slog.Logger
}

// NewHistoryLoader 创建有界历史加载器。
func NewHistoryLoader(store CompressionStore, compressor *Compressor, cfg ContextCompressionConfig, logger *slog.Logger) *HistoryLoader {
	if logger == nil {
		logger = slog.Default()
	}
	return &HistoryLoader{store: store, compressor: compressor, cfg: cfg, logger: logger}
}

// LoadHistory 加载并压缩会话历史，返回格式化后的文本行（含可选的摘要行）。
// 与压缩器的契约：传入的 turns 是「待摘要批 + 最近窗口」，coveredTurns 恒为 0
//（待摘要批已在覆盖边界之后）；压缩成功后的新覆盖边界 =
// turns[coveredTurns-1].sequenceNum，锁内校验不回退。
func (l *HistoryLoader) LoadHistory(ctx context.Context, sessionID int64, llmProvider string) []string {
	if !l.cfg.Enabled {
		// 唯一的全量分支：enabled=false 仅用于本地排查，生产默认不得走进
		l.logger.Warn("上下文压缩已关闭，语音历史走全量读取（仅限本地排查）",
			slog.Int64("sessionId", sessionID))
		return l.loadUncompressed(ctx, sessionID)
	}
	history, err := l.loadBounded(ctx, sessionID, llmProvider)
	if err != nil {
		l.logger.Warn("语音历史有界加载失败，降级为最近窗口",
			slog.Int64("sessionId", sessionID), slog.String("error", err.Error()))
		return l.loadWindowOnly(ctx, sessionID)
	}
	return history
}

// loadBounded 有界装配（Java loadBounded L65-127 逐步对齐）。
func (l *HistoryLoader) loadBounded(ctx context.Context, sessionID int64, llmProvider string) ([]string, error) {
	windowSize := l.cfg.WindowSize
	batchSize := l.cfg.SummaryBatchSize

	// 1. 读取 SUMMARY 行（单行查询）。
	summaryRow, err := l.store.FindSummaryRow(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var cachedSummary *string
	var coveredSequenceNum *int
	if summaryRow != nil {
		cachedSummary = trimPtrToNull(summaryRow.AiGeneratedText)
		coveredSequenceNum = summaryRow.SummaryCoveredSequenceNum
	}
	if summaryRow != nil && coveredSequenceNum == nil {
		// 2. 旧数据迁移：只用 OFFSET/LIMIT 1 定位第 N 条，不全量读取、不重新摘要。
		migrated, mErr := l.migrateLegacyBoundary(ctx, sessionID, summaryRow.SequenceNum)
		if mErr != nil {
			return nil, mErr
		}
		coveredSequenceNum = &migrated
		if err := l.store.SaveSummaryRow(ctx, sessionID, cachedSummary, migrated); err != nil &&
			!errors.Is(err, ErrSummaryBoundaryRegressed) {
			return nil, err
		}
	}

	// 3. 倒序取最近窗口，内存恢复升序。
	recent, err := l.store.FindRecentDialogueMessages(ctx, sessionID, windowSize)
	if err != nil {
		return nil, err
	}

	// 4. 边界之后、最近窗口之前的待摘要段（升序，最多一个批次）。
	//    无 SUMMARY 行时 after 取 0；每轮只推进一个批次，大量历史积压不能靠一次加载补齐。
	var pending []MessageEntity
	after := 0
	if coveredSequenceNum != nil {
		after = *coveredSequenceNum
	}
	// 窗口未填满时必然没有窗口外的未摘要消息，直接跳过 pending 查询。
	if len(recent) > 0 && len(recent) >= windowSize && msgSeq(recent[0]) > after {
		pending, err = l.store.FindPendingSummaryBatch(ctx, sessionID, after, msgSeq(recent[0]), batchSize)
		if err != nil {
			return nil, err
		}
	}

	// 5. 压缩：turns = pending + 窗口，coveredTurns 恒为 0（换算在 Loader 完成）。
	turns := make([]MessageEntity, 0, len(pending)+len(recent))
	turns = append(turns, pending...)
	turns = append(turns, recent...)
	compressed := l.compressor.Compress(ctx, turns, cachedSummary, 0, llmProvider)

	// 6. 持久化推进后的边界（LLM 已在 Compress 内完成，事务在 SaveSummaryRow 内）。
	if compressed.Changed && compressed.Summary != nil &&
		strings.TrimSpace(*compressed.Summary) != "" && compressed.CoveredTurns > 0 {
		newBoundary := msgSeq(turns[compressed.CoveredTurns-1])
		if err := l.store.SaveSummaryRow(ctx, sessionID, compressed.Summary, newBoundary); err != nil {
			if errors.Is(err, ErrSummaryBoundaryRegressed) {
				l.logger.Warn("摘要覆盖边界回退被拒绝", slog.Int64("sessionId", sessionID))
			} else {
				l.logger.Warn("持久化上下文摘要失败（不影响本次应答）",
					slog.Int64("sessionId", sessionID), slog.String("error", err.Error()))
			}
		}
	}

	history := make([]string, 0, len(compressed.Recent)+1)
	if compressed.Summary != nil && strings.TrimSpace(*compressed.Summary) != "" {
		history = append(history, "【对话摘要】"+*compressed.Summary)
	}
	history = append(history, formatTurnLines(compressed.Recent)...)
	return history, nil
}

// migrateLegacyBoundary 旧 SUMMARY 行迁移：coveredTurns = -旧sequenceNum - 1，
// 用 OFFSET/LIMIT 1 定位第 N 条原始消息的 sequenceNum 作为边界并写回
//（Java migrateLegacyBoundary L133-151 逐字）。
func (l *HistoryLoader) migrateLegacyBoundary(ctx context.Context, sessionID int64, legacySequenceNum *int) (int, error) {
	coveredTurns := 0
	if legacySequenceNum != nil {
		coveredTurns = max(0, -*legacySequenceNum-1)
	}
	if coveredTurns <= 0 {
		return 0, nil
	}
	nth, err := l.store.FindDialogueMessageAt(ctx, sessionID, coveredTurns-1)
	if err != nil {
		return 0, err
	}
	if nth == nil || nth.SequenceNum == nil {
		l.logger.Warn("旧摘要边界迁移定位失败，回退边界 0", slog.Int64("sessionId", sessionID))
		return 0, nil
	}
	return *nth.SequenceNum, nil
}

// loadWindowOnly 摘要失败/加载异常时的降级：只取最近窗口（查询量仍有界），不恢复全量
//（Java loadWindowOnly L156-166）。
func (l *HistoryLoader) loadWindowOnly(ctx context.Context, sessionID int64) []string {
	recent, err := l.store.FindRecentDialogueMessages(ctx, sessionID, l.cfg.WindowSize)
	if err != nil {
		l.logger.Error("加载语音面试历史失败",
			slog.Int64("sessionId", sessionID), slog.String("error", err.Error()))
		return []string{}
	}
	return l.compressor.formatRecentWithinBudget(recent)
}

// loadUncompressed enabled=false 的全量分支（仅本地排查）：行为与旧 getHistory
// 全量路径一致（Java loadUncompressed L171-174）。
func (l *HistoryLoader) loadUncompressed(ctx context.Context, sessionID int64) []string {
	turns, err := l.store.FindDialogueMessages(ctx, sessionID)
	if err != nil {
		l.logger.Error("加载语音面试历史失败",
			slog.Int64("sessionId", sessionID), slog.String("error", err.Error()))
		return []string{}
	}
	return formatTurnLines(turns)
}

// ===== 小工具 =====

// trimPtrToNull 指针版 trimToNull：nil 原样，空白 → nil，其余去首尾空白。
func trimPtrToNull(s *string) *string {
	if s == nil {
		return nil
	}
	return trimVoiceToNull(*s)
}

func runeLen(s string) int { return len([]rune(s)) }

// msgSeq 消息序号（nil 按 0，防御上游脏数据）。
func msgSeq(m MessageEntity) int {
	if m.SequenceNum == nil {
		return 0
	}
	return *m.SequenceNum
}
