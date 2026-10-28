// 批次评估引擎（票 05）：纯逻辑，不接队列、不出 HTTP。
// 对齐 Java common/evaluation/UnifiedEvaluationService 与
// modules/interview/service/AnswerEvaluationService 的编排（笔记 06 逐字）：
// 问答成组 → 按组装批 → 逐批结构化调用 → 批失败按组二分恢复（共享预算）→
// 按 questionIndex 合并 → 二次汇总 → 构建报告。
// LLM 经注入的 ChatCompleter + StructuredInvoker 调用（spec 决策 3 的既有接缝）。
package interview

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/wuzuxian/interview/internal/ai"
	"github.com/wuzuxian/interview/internal/apperr"
	"github.com/wuzuxian/interview/internal/strutil"
)

// ===== 评估 prompt 资源（Java 原始字节拷贝，{xxx} 为占位符）=====

//go:embed prompts/interview-evaluation-system.st
var evalSystemPrompt string

//go:embed prompts/interview-evaluation-user.st
var evalUserPrompt string

//go:embed prompts/interview-evaluation-summary-system.st
var evalSummarySystemPrompt string

//go:embed prompts/interview-evaluation-summary-user.st
var evalSummaryUserPrompt string

// 批次/汇总输出 JSON 形状说明，等价 Java BeanOutputConverter#getFormat()
// 为 BatchReportDTO / SummaryDTO 生成的结构约束（由 ai.InvokeStructured 追加）。
const (
	batchReportSchema = "{overallScore:int, overallFeedback:string, strengths:[string], " +
		"improvements:[string], questionEvaluations:[{questionIndex:int, score:int, " +
		"feedback:string, referenceAnswer:string, keyPoints:[string]}]}"
	summarySchema = "{overallFeedback:string, strengths:[string], improvements:[string]}"
)

// 截断提示文案（Java evaluate 内联字符串，逐字）。
const (
	resumeTruncatedSuffix    = "\n...(简历内容过长，已截断)"
	referenceTruncatedSuffix = "\n...(参考基线过长，已截断)"
)

// 两种兜底文案逐字（裁决 9：场景不同不得混用）：
// 合并缺失/非法位置用前者，批恢复显式降级用后者。
const (
	mergeMissingFeedback = "该题未成功生成评估结果，系统按 0 分处理。"
	degradedFeedback     = "模型评估失败后的系统降级，该分数不代表真实表现。"
	degradedFeedbackMsg  = "模型评估失败后的系统降级。"
	noFeedbackMsg        = "该题未成功生成评估反馈。"
	noValidFeedbackMsg   = "本次面试已完成分批评估，但未生成有效综合评语。"
)

// strengths/improvements 汇总上限（裁决：sanitizeItems limit(8)，照抄）。
const summaryItemLimit = 8

// questionHighlights 行数上限与字段截断长度（Java buildQuestionHighlights）。
const (
	highlightMaxLines    = 20
	highlightQuestionMax = 50
	highlightFeedbackMax = 80
)

// ===== 报告 DTO（JSON 字段名与 Java record 序列化逐字一致）=====

// ReportDTO 对应 Java InterviewReportDTO record。
type ReportDTO struct {
	SessionID        string                  `json:"sessionId"`
	TotalQuestions   int                     `json:"totalQuestions"`
	OverallScore     int                     `json:"overallScore"`
	CategoryScores   []CategoryScoreDTO      `json:"categoryScores"`
	QuestionDetails  []QuestionEvaluationDTO `json:"questionDetails"`
	OverallFeedback  string                  `json:"overallFeedback"`
	Strengths        []string                `json:"strengths"`
	Improvements     []string                `json:"improvements"`
	ReferenceAnswers []ReferenceAnswerDTO    `json:"referenceAnswers"`
}

// CategoryScoreDTO 对应 Java InterviewReportDTO.CategoryScore。
type CategoryScoreDTO struct {
	Category      string `json:"category"`
	Score         int    `json:"score"`
	QuestionCount int    `json:"questionCount"`
}

// QuestionEvaluationDTO 对应 Java InterviewReportDTO.QuestionEvaluation。
// userAnswer 原样保留 null（Java 语义）。
type QuestionEvaluationDTO struct {
	QuestionIndex int     `json:"questionIndex"`
	Question      string  `json:"question"`
	Category      string  `json:"category"`
	UserAnswer    *string `json:"userAnswer"`
	Score         int     `json:"score"`
	Feedback      string  `json:"feedback"`
}

// ReferenceAnswerDTO 对应 Java InterviewReportDTO.ReferenceAnswer。
type ReferenceAnswerDTO struct {
	QuestionIndex   int      `json:"questionIndex"`
	Question        string   `json:"question"`
	ReferenceAnswer string   `json:"referenceAnswer"`
	KeyPoints       []string `json:"keyPoints"`
}

// EvaluateInput 评估输入：会话数据快照（票 06 从 DB 重放后组装）。
type EvaluateInput struct {
	SessionID  string
	ResumeText string
	Questions  []QuestionDTO
	// SkillReference 方向参考段兜底（票 06 注入，可空）：
	// 题目自带参考基线（参考答案/评分要点/评分规则）拼接为空时使用。
	SkillReference string
}

// ===== 引擎 =====

// EvaluationConfig 评估引擎配置，键对齐 Java UnifiedEvaluationService 构造参数。
type EvaluationConfig struct {
	BatchSize             int  // 每批题目条数（主问题+追问合计），Java 默认 8
	FallbackMaxExtraCalls int  // 二分恢复共享额外调用预算，Java 默认 6
	FallbackMinGroups     int  // 可再二分的最小组数，Java 默认 2
	FallbackSplitEnabled  bool // 批失败是否启用二分恢复（Java fallbackSplitEnabled）
	ResumeMaxChars        int  // 简历摘要截断长度，Java 3000
	MaxReferenceChars     int  // 参考基线截断长度，Java MAX_REFERENCE_CONTEXT_CHARS=6000
}

// DefaultEvaluationConfig 默认值（对齐 Java application.yml 与构造钳制前配置）。
func DefaultEvaluationConfig() EvaluationConfig {
	return EvaluationConfig{
		BatchSize:             8,
		FallbackMaxExtraCalls: 6,
		FallbackMinGroups:     2,
		FallbackSplitEnabled:  true,
		ResumeMaxChars:        3000,
		MaxReferenceChars:     6000,
	}
}

// EvaluationEngine 批次评估引擎。
type EvaluationEngine struct {
	cfg    EvaluationConfig
	logger *slog.Logger
}

// NewEvaluationEngine 创建引擎，构造钳制对齐 Java：
// batchSize=max(1,·)、fallbackMaxExtraCalls=max(0,·)、fallbackMinGroups=max(2,·)。
func NewEvaluationEngine(cfg EvaluationConfig, logger *slog.Logger) *EvaluationEngine {
	if cfg.BatchSize < 1 {
		cfg.BatchSize = 1
	}
	if cfg.FallbackMaxExtraCalls < 0 {
		cfg.FallbackMaxExtraCalls = 0
	}
	if cfg.FallbackMinGroups < 2 {
		cfg.FallbackMinGroups = 2
	}
	if cfg.ResumeMaxChars < 1 {
		cfg.ResumeMaxChars = 3000
	}
	if cfg.MaxReferenceChars < 1 {
		cfg.MaxReferenceChars = 6000
	}
	return &EvaluationEngine{cfg: cfg, logger: logger}
}

// EvaluateSession 评估整场面试（引擎唯一入口）：输入会话数据（简历文本、题目列表含作答），
// 输出完整报告。批失败、汇总失败均显式降级而非报错（对齐 Java）；唯一上抛口径（主控仲裁，
// 第三处有意偏离 Java）：成功的批数 == 0 且总题数 > 0——即全部批次都没有产出任何可用
// 逐题评估（连显式降级 0 分产物也没有）——返回 error 交消费者重试→FAILED，避免落全零
// 假报告。显式降级 0 分算该批产物（Java 语义保留），部分成功/全降级仍正常出报告。
func (e *EvaluationEngine) EvaluateSession(ctx context.Context, completer ai.ChatCompleter,
	invoker *ai.StructuredInvoker, input EvaluateInput) (*ReportDTO, error) {
	if completer == nil {
		return nil, apperr.New(apperr.CodeInterviewEvaluationFailed, "面试评估失败")
	}

	// 问答记录与参考基线（对齐 AnswerEvaluationService.evaluateInterview）。
	qaRecords := toQaRecords(input.Questions)
	referenceContext := buildQuestionReferenceContext(input.Questions)
	if strings.TrimSpace(referenceContext) == "" {
		referenceContext = input.SkillReference
	}

	// 简历摘要与参考基线截断（对齐 UnifiedEvaluationService.evaluate；
	// 截断按 rune 计，对齐 Java String.length() 的中文 BMP 语义）。
	resumeContext := input.ResumeText
	if runeLen(resumeContext) > e.cfg.ResumeMaxChars {
		resumeContext = strutil.TruncateRunes(resumeContext, e.cfg.ResumeMaxChars) + resumeTruncatedSuffix
	}
	referenceBaseline := strings.TrimSpace(referenceContext)
	if runeLen(referenceBaseline) > e.cfg.MaxReferenceChars {
		referenceBaseline = strutil.TruncateRunes(referenceBaseline, e.cfg.MaxReferenceChars) + referenceTruncatedSuffix
	}

	// 分批评估 → 合并 → 二次汇总 → 构建报告。
	batchResults := e.evaluateInBatches(ctx, completer, invoker, input.SessionID,
		resumeContext, referenceBaseline, qaRecords)
	// 全批失败上抛（主控仲裁，有意偏离 Java）：无任何一批产出可用逐题评估时不再拼报告。
	if countProducingBatches(batchResults) == 0 && len(qaRecords) > 0 {
		return nil, apperr.New(apperr.CodeInterviewEvaluationFailed, fmt.Sprintf(
			"面试评估失败：全部 %d 个评估批次均未产出逐题评估结果（sessionId=%s，题数=%d）",
			len(batchResults), input.SessionID, len(qaRecords)))
	}
	merged := mergeQuestionEvaluations(batchResults)
	fallbackFeedback := mergeOverallFeedback(batchResults)
	fallbackStrengths := mergeListItems(batchResults, true)
	fallbackImprovements := mergeListItems(batchResults, false)

	summary := e.summarizeBatchResults(ctx, completer, invoker, input.SessionID,
		resumeContext, referenceBaseline, qaRecords, merged,
		fallbackFeedback, fallbackStrengths, fallbackImprovements)

	report := buildReport(input.SessionID, qaRecords, merged,
		summary.OverallFeedback, summary.Strengths, summary.Improvements)
	// 题目自带参考答案优先覆盖报告中的参考答案（对齐 withQuestionReferences）。
	withQuestionReferences(report, input.Questions)
	return report, nil
}

// ===== 内部数据形状（对齐 Java common/evaluation 包内部 record）=====

// qaRecord 通用问答记录；userAnswer nil 表示未回答。
type qaRecord struct {
	questionIndex       int
	question            string
	category            string
	userAnswer          *string
	followUp            bool
	parentQuestionIndex *int
}

func (q qaRecord) answered() bool {
	return q.userAnswer != nil && strings.TrimSpace(*q.userAnswer) != ""
}

// questionEvalDTO 批次结果中的逐题评估；Feedback/ReferenceAnswer 用指针以区分
// JSON null（Java record 组件可为 null，buildReport 据此回退默认文案）。
type questionEvalDTO struct {
	QuestionIndex   int      `json:"questionIndex"`
	Score           int      `json:"score"`
	Feedback        *string  `json:"feedback"`
	ReferenceAnswer *string  `json:"referenceAnswer"`
	KeyPoints       []string `json:"keyPoints"`
}

// batchReportDTO 批次评估结果（LLM 输出目标结构）。
type batchReportDTO struct {
	OverallScore        int                `json:"overallScore"`
	OverallFeedback     string             `json:"overallFeedback"`
	Strengths           []string           `json:"strengths"`
	Improvements        []string           `json:"improvements"`
	QuestionEvaluations []*questionEvalDTO `json:"questionEvaluations"`
}

// summaryDTO 二次汇总结果（LLM 输出目标结构，同时作内部降级兜底的载体）。
type summaryDTO struct {
	OverallFeedback string   `json:"overallFeedback"`
	Strengths       []string `json:"strengths"`
	Improvements    []string `json:"improvements"`
}

// batchResult 一批的期望题号与报告（report 为 nil 表示该批整体失败）。
type batchResult struct {
	questionIndexes []int
	report          *batchReportDTO
}

// qaGroup 问答组：主问题与其追问构成的不可拆分单元。
type qaGroup struct {
	records []qaRecord
}

// ===== 记录与参考基线构建（对齐 AnswerEvaluationService）=====

// toQaRecords 题目 DTO → 通用问答记录。
func toQaRecords(questions []QuestionDTO) []qaRecord {
	records := make([]qaRecord, 0, len(questions))
	for _, q := range questions {
		category := ""
		if q.Category != nil {
			category = *q.Category
		}
		records = append(records, qaRecord{
			questionIndex:       q.QuestionIndex,
			question:            q.Question,
			category:            category,
			userAnswer:          q.UserAnswer,
			followUp:            q.IsFollowUp,
			parentQuestionIndex: q.ParentQuestionIndex,
		})
	}
	return records
}

// buildQuestionReferenceContext 题目自带参考基线拼接（逐字格式；展示序号 questionIndex+1）。
func buildQuestionReferenceContext(questions []QuestionDTO) string {
	var sb strings.Builder
	for _, q := range questions {
		if !hasQuestionReference(q) {
			continue
		}
		sb.WriteString(fmt.Sprintf("问题%d: %s\n", q.QuestionIndex+1, q.Question))
		appendIfPresent(&sb, "参考答案", q.ReferenceAnswer)
		if len(q.KeyPoints) > 0 {
			sb.WriteString("评分要点: " + strings.Join(q.KeyPoints, "；") + "\n")
		}
		appendIfPresent(&sb, "评分规则", q.ScoringRubric)
		sb.WriteString("\n")
	}
	return sb.String()
}

func hasQuestionReference(q QuestionDTO) bool {
	return hasText(q.ReferenceAnswer) || hasText(q.ScoringRubric) || len(q.KeyPoints) > 0
}

func appendIfPresent(sb *strings.Builder, label string, value *string) {
	if hasText(value) {
		sb.WriteString(label + ": " + strings.TrimSpace(*value) + "\n")
	}
}

func hasText(value *string) bool {
	return value != nil && strings.TrimSpace(*value) != ""
}

// ===== 分批（buildGroups / packBatches，逐字对齐）=====

// buildGroups 主问题与其追问成组；追问父索引异常（父索引不存在或不小于自身）降级为独立组。
func buildGroups(records []qaRecord, logger *slog.Logger) []qaGroup {
	known := make(map[int]bool, len(records))
	for _, q := range records {
		known[q.questionIndex] = true
	}
	groupByIndex := make(map[int]*qaGroup, len(records))
	var ordered []*qaGroup
	for _, q := range records {
		var target *qaGroup
		if q.followUp && q.parentQuestionIndex != nil {
			parent := *q.parentQuestionIndex
			if known[parent] && parent < q.questionIndex {
				target = groupByIndex[parent]
			} else if logger != nil {
				logger.Warn("追问父索引异常，降级为独立组",
					slog.Int("questionIndex", q.questionIndex),
					slog.Int("parentQuestionIndex", parent))
			}
		}
		if target == nil {
			target = &qaGroup{}
			ordered = append(ordered, target)
		}
		target.records = append(target.records, q)
		groupByIndex[q.questionIndex] = target
	}
	groups := make([]qaGroup, 0, len(ordered))
	for _, g := range ordered {
		groups = append(groups, *g)
	}
	return groups
}

// packBatches 按组装批：组不可拆散，当前批放入下一组会超过 batchSize 时切批。
func packBatches(groups []qaGroup, batchSize int) [][]qaGroup {
	var batches [][]qaGroup
	current := []qaGroup{}
	currentSize := 0
	for _, group := range groups {
		if currentSize > 0 && currentSize+len(group.records) > batchSize {
			batches = append(batches, current)
			current = []qaGroup{}
			currentSize = 0
		}
		current = append(current, group)
		currentSize += len(group.records)
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

func groupRecords(groups []qaGroup) []qaRecord {
	var records []qaRecord
	for _, g := range groups {
		records = append(records, g.records...)
	}
	return records
}

func recordIndexes(records []qaRecord) []int {
	indexes := make([]int, 0, len(records))
	for _, q := range records {
		indexes = append(indexes, q.questionIndex)
	}
	return indexes
}

// ===== 批评估与二分恢复 =====

// evaluateInBatches 逐批评估；批失败且启用恢复时按组二分（共享额外调用预算）。
func (e *EvaluationEngine) evaluateInBatches(ctx context.Context, completer ai.ChatCompleter,
	invoker *ai.StructuredInvoker, sessionID, resumeContext, referenceContext string,
	qaRecords []qaRecord) []batchResult {
	results := []batchResult{}
	budget := e.cfg.FallbackMaxExtraCalls
	for _, batchGroups := range packBatches(buildGroups(qaRecords, e.logger), e.cfg.BatchSize) {
		flattened := groupRecords(batchGroups)
		report := e.evaluateBatch(ctx, completer, invoker, sessionID, resumeContext, referenceContext, flattened)
		if report == nil && e.cfg.FallbackSplitEnabled {
			report = e.recoverBatch(ctx, completer, invoker, sessionID,
				resumeContext, referenceContext, batchGroups, &budget, 0)
		}
		results = append(results, batchResult{questionIndexes: recordIndexes(flattened), report: report})
	}
	return results
}

// countProducingBatches 产出可用逐题评估的批数（仲裁口径的“成功的批数”）：
// 显式降级报告也算该批产物（Java 语义保留，降级 0 分仍落报告）；
// 报告为 nil 或不含任何逐题评估条目的批次不算。
func countProducingBatches(results []batchResult) int {
	n := 0
	for _, r := range results {
		if r.report != nil && len(r.report.QuestionEvaluations) > 0 {
			n++
		}
	}
	return n
}

// evaluateBatch 单批结构化调用；任何失败返回 nil（让合并逻辑用零分兜底）。
func (e *EvaluationEngine) evaluateBatch(ctx context.Context, completer ai.ChatCompleter,
	invoker *ai.StructuredInvoker, sessionID, resumeContext, referenceContext string,
	batch []qaRecord) *batchReportDTO {
	// system：模板 + 防注入段，格式说明由 ai.InvokeStructured 追加（票 02 决定的等价顺序）。
	system := evalSystemPrompt + antiInjectionInstruction
	reference := referenceContext
	if strings.TrimSpace(reference) == "" {
		reference = "无"
	}
	user := renderPrompt(evalUserPrompt, map[string]string{
		"resumeText":       resumeContext,
		"qaRecords":        buildQARecordsText(batch),
		"referenceContext": reference,
	})

	report, err := ai.InvokeStructured[batchReportDTO](ctx, invoker, completer,
		system, user, batchReportSchema, "批次评估", apperr.CodeInterviewEvaluationFailed)
	if err != nil {
		if e.logger != nil {
			e.logger.Error("批次评估失败",
				slog.String("sessionId", sessionID),
				slog.Int("batchSize", len(batch)),
				slog.String("error", err.Error()))
		}
		return nil
	}
	return report
}

// recoverBatch 批失败按组二分恢复：共享额外调用预算；组数达最小组数时二分重试
// （先评价兄弟半批，健康侧先拿到预算，再对失败侧递归下钻）；
// 不可再拆时整段加试一次（耗预算），仍失败则该段降级。
func (e *EvaluationEngine) recoverBatch(ctx context.Context, completer ai.ChatCompleter,
	invoker *ai.StructuredInvoker, sessionID, resumeContext, referenceContext string,
	groups []qaGroup, budget *int, depth int) *batchReportDTO {
	indexes := recordIndexes(groupRecords(groups))
	if *budget <= 0 {
		e.warnDegraded("评估批次恢复预算耗尽，降级", sessionID, groups, indexes, depth)
		return degradedReport(indexes)
	}
	if len(groups) >= e.cfg.FallbackMinGroups {
		mid := len(groups) / 2
		halves := [][]qaGroup{groups[:mid], groups[mid:]}
		halfReports := make([]*batchReportDTO, len(halves))
		halfFailed := make([]bool, len(halves))
		for i := range halves {
			flattened := groupRecords(halves[i])
			if *budget <= 0 {
				halfReports[i] = degradedReport(recordIndexes(flattened))
				continue
			}
			*budget--
			halfReports[i] = e.evaluateBatch(ctx, completer, invoker, sessionID,
				resumeContext, referenceContext, flattened)
			halfFailed[i] = halfReports[i] == nil
		}
		merged := []*questionEvalDTO{}
		for i := range halves {
			flattened := groupRecords(halves[i])
			if halfFailed[i] {
				halfReports[i] = e.recoverBatch(ctx, completer, invoker, sessionID,
					resumeContext, referenceContext, halves[i], budget, depth+1)
			}
			if halfReports[i] != nil && halfReports[i].QuestionEvaluations != nil {
				merged = append(merged, halfReports[i].QuestionEvaluations...)
			} else {
				merged = append(merged, degradedReport(recordIndexes(flattened)).QuestionEvaluations...)
			}
		}
		return &batchReportDTO{Strengths: []string{}, Improvements: []string{}, QuestionEvaluations: merged}
	}
	// 不可再拆：整段加试一次（耗预算），仍失败则该段降级。
	flattened := groupRecords(groups)
	*budget--
	retried := e.evaluateBatch(ctx, completer, invoker, sessionID, resumeContext, referenceContext, flattened)
	if retried == nil {
		e.warnDegraded("评估批次恢复最终失败，降级", sessionID, groups, indexes, depth)
		return degradedReport(indexes)
	}
	return retried
}

func (e *EvaluationEngine) warnDegraded(reason, sessionID string, groups []qaGroup, indexes []int, depth int) {
	if e.logger != nil {
		e.logger.Warn(reason,
			slog.String("sessionId", sessionID),
			slog.Int("groups", len(groups)),
			slog.Int("size", len(indexes)),
			slog.Int("depth", depth),
			slog.String("failedIndexes", fmt.Sprintf("%v", indexes)))
	}
}

// degradedReport 显式降级报告：全部 0 分 + 系统降级文案（逐字）。
func degradedReport(indexes []int) *batchReportDTO {
	evaluations := make([]*questionEvalDTO, 0, len(indexes))
	for _, index := range indexes {
		evaluations = append(evaluations, &questionEvalDTO{
			QuestionIndex:   index,
			Score:           0,
			Feedback:        stringPtr(degradedFeedback),
			ReferenceAnswer: stringPtr(""),
			KeyPoints:       []string{},
		})
	}
	return &batchReportDTO{
		OverallFeedback:     degradedFeedbackMsg,
		Strengths:           []string{},
		Improvements:        []string{},
		QuestionEvaluations: evaluations,
	}
}

// ===== 合并（mergeQuestionEvaluations，逐字对齐）=====

// mergeQuestionEvaluations 按 questionIndex 映射回原题；三条兜底路径：
// 整批一致 1-based 偏移修正、全非法按位置对齐、缺失位置补默认文案。
func mergeQuestionEvaluations(batchResults []batchResult) []*questionEvalDTO {
	merged := []*questionEvalDTO{}
	for _, result := range batchResults {
		expectedIndexes := result.questionIndexes
		var current []*questionEvalDTO
		if result.report != nil && result.report.QuestionEvaluations != nil {
			current = result.report.QuestionEvaluations
		}

		byIndex := map[int]*questionEvalDTO{}
		returnedIndexes := map[int]bool{}
		for _, dto := range current {
			if dto == nil {
				continue
			}
			returnedIndexes[dto.QuestionIndex] = true
			if containsInt(expectedIndexes, dto.QuestionIndex) {
				if _, ok := byIndex[dto.QuestionIndex]; !ok {
					byIndex[dto.QuestionIndex] = dto
				}
			}
		}
		countsMatch := len(current) == len(expectedIndexes)
		allMapped := len(byIndex) == len(expectedIndexes)
		oneBasedShift := !allMapped && countsMatch && isConsistentOneBasedShift(returnedIndexes, expectedIndexes)
		noUsableIndex := len(byIndex) == 0 && countsMatch
		positionalFallback := oneBasedShift || noUsableIndex

		for i, originalIndex := range expectedIndexes {
			var dto *questionEvalDTO
			if positionalFallback {
				dto = current[i]
			} else {
				dto = byIndex[originalIndex]
			}
			if dto != nil && dto.QuestionIndex != originalIndex {
				corrected := *dto
				corrected.QuestionIndex = originalIndex
				dto = &corrected
			}
			if dto == nil {
				dto = &questionEvalDTO{
					QuestionIndex:   originalIndex,
					Score:           0,
					Feedback:        stringPtr(mergeMissingFeedback),
					ReferenceAnswer: stringPtr(""),
					KeyPoints:       []string{},
				}
			}
			merged = append(merged, dto)
		}
	}
	return merged
}

// isConsistentOneBasedShift 返回题号集合是否恰为期望题号整体 +1。
func isConsistentOneBasedShift(returnedIndexes map[int]bool, expectedIndexes []int) bool {
	shifted := make(map[int]bool, len(expectedIndexes))
	for _, i := range expectedIndexes {
		shifted[i+1] = true
	}
	if len(returnedIndexes) != len(shifted) {
		return false
	}
	for index := range returnedIndexes {
		if !shifted[index] {
			return false
		}
	}
	return true
}

func containsInt(list []int, target int) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

// mergeOverallFeedback 批次评语按序拼接；全空时给默认文案。
func mergeOverallFeedback(batchResults []batchResult) string {
	var parts []string
	for _, result := range batchResults {
		if result.report == nil || !hasText(&result.report.OverallFeedback) {
			continue
		}
		parts = append(parts, result.report.OverallFeedback)
	}
	feedback := strings.Join(parts, "\n\n")
	if strings.TrimSpace(feedback) == "" {
		return noValidFeedbackMsg
	}
	return feedback
}

// mergeListItems 批次优势/改进去重合并（保持出现序），上限 8 条。
func mergeListItems(batchResults []batchResult, strengthsMode bool) []string {
	merged := []string{}
	seen := map[string]bool{}
	for _, result := range batchResults {
		if result.report == nil {
			continue
		}
		items := result.report.Improvements
		if strengthsMode {
			items = result.report.Strengths
		}
		for _, item := range items {
			trimmed := strings.TrimSpace(item)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			merged = append(merged, trimmed)
		}
	}
	if len(merged) > summaryItemLimit {
		merged = merged[:summaryItemLimit]
	}
	return merged
}

// ===== 二次汇总 =====

// summarizeBatchResults 一次汇总调用；失败降级为批次聚合结果。
func (e *EvaluationEngine) summarizeBatchResults(ctx context.Context, completer ai.ChatCompleter,
	invoker *ai.StructuredInvoker, sessionID, resumeContext, referenceContext string,
	qaRecords []qaRecord, evaluations []*questionEvalDTO,
	fallbackFeedback string, fallbackStrengths, fallbackImprovements []string) summaryDTO {
	fallback := summaryDTO{
		OverallFeedback: fallbackFeedback,
		Strengths:       fallbackStrengths,
		Improvements:    fallbackImprovements,
	}
	reference := referenceContext
	if strings.TrimSpace(reference) == "" {
		reference = "无"
	}
	system := evalSummarySystemPrompt + antiInjectionInstruction
	user := renderPrompt(evalSummaryUserPrompt, map[string]string{
		"resumeText":              resumeContext,
		"referenceContext":        reference,
		"categorySummary":         buildCategorySummary(qaRecords, evaluations),
		"questionHighlights":      buildQuestionHighlights(qaRecords, evaluations),
		"fallbackOverallFeedback": fallbackFeedback,
		"fallbackStrengths":       strings.Join(fallbackStrengths, "\n"),
		"fallbackImprovements":    strings.Join(fallbackImprovements, "\n"),
	})

	dto, err := ai.InvokeStructured[summaryDTO](ctx, invoker, completer,
		system, user, summarySchema, "总结评估", apperr.CodeInterviewEvaluationFailed)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("二次汇总评估失败，降级到批次聚合结果",
				slog.String("sessionId", sessionID), slog.String("error", err.Error()))
		}
		return fallback
	}

	feedback := fallbackFeedback
	if dto != nil && hasText(&dto.OverallFeedback) {
		feedback = dto.OverallFeedback
	}
	return summaryDTO{
		OverallFeedback: feedback,
		Strengths:       sanitizeItems(dtoStrengths(dto), fallbackStrengths),
		Improvements:    sanitizeItems(dtoImprovements(dto), fallbackImprovements),
	}
}

func dtoStrengths(dto *summaryDTO) []string {
	if dto == nil {
		return nil
	}
	return dto.Strengths
}

func dtoImprovements(dto *summaryDTO) []string {
	if dto == nil {
		return nil
	}
	return dto.Improvements
}

// sanitizeItems 主列表为空回退兜底列表；过滤空白、trim、去重、上限 8 条。
func sanitizeItems(primary, fallback []string) []string {
	source := primary
	if len(source) == 0 {
		source = fallback
	}
	out := []string{}
	seen := map[string]bool{}
	for _, item := range source {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	if len(out) > summaryItemLimit {
		out = out[:summaryItemLimit]
	}
	return out
}

// buildCategorySummary 类别得分概览行（逐字格式 `- <分类>: 平均分 <avg>, 题数 <n>`，
// 未回答题按 0 分计入均分与题数；按行字典序排序）。
func buildCategorySummary(qaRecords []qaRecord, evaluations []*questionEvalDTO) string {
	categoryScores := map[string][]int{}
	for i, q := range qaRecords {
		var eval *questionEvalDTO
		if i < len(evaluations) {
			eval = evaluations[i]
		}
		score := 0
		if eval != nil && q.answered() {
			score = eval.Score
		}
		categoryScores[q.category] = append(categoryScores[q.category], score)
	}
	lines := make([]string, 0, len(categoryScores))
	for category, scores := range categoryScores {
		lines = append(lines, fmt.Sprintf("- %s: 平均分 %d, 题数 %d", category, intAvg(scores), len(scores)))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// buildQuestionHighlights 题目评估高亮行（逐字格式 `- Q<n+1> | 题干前50字 | 分数:x | 反馈前80字`，
// Q 编号 1-based，最多 20 行）。
func buildQuestionHighlights(qaRecords []qaRecord, evaluations []*questionEvalDTO) string {
	highlights := make([]string, 0, len(qaRecords))
	for i, q := range qaRecords {
		var eval *questionEvalDTO
		if i < len(evaluations) {
			eval = evaluations[i]
		}
		score := 0
		feedback := ""
		if eval != nil {
			score = eval.Score
			if eval.Feedback != nil {
				feedback = *eval.Feedback
			}
		}
		shortQ := q.question
		if runeLen(shortQ) > highlightQuestionMax {
			shortQ = strutil.TruncateRunes(shortQ, highlightQuestionMax) + "..."
		}
		shortF := feedback
		if runeLen(shortF) > highlightFeedbackMax {
			shortF = strutil.TruncateRunes(shortF, highlightFeedbackMax) + "..."
		}
		highlights = append(highlights,
			fmt.Sprintf("- Q%d | %s | 分数:%d | 反馈:%s", q.questionIndex+1, shortQ, score, shortF))
	}
	if len(highlights) > highlightMaxLines {
		highlights = highlights[:highlightMaxLines]
	}
	return strings.Join(highlights, "\n")
}

// ===== 报告构建（buildReport，逐字对齐）=====

// buildReport 总分 = 全部题目（含追问、未答 0 分）简单平均，全部未答为 0；
// 分类得分 = 分类平均（int 截断）+ 题数（Go 侧按分类名稳定排序，裁决允许）。
func buildReport(sessionID string, qaRecords []qaRecord, evaluations []*questionEvalDTO,
	overallFeedback string, strengths, improvements []string) *ReportDTO {
	questionDetails := []QuestionEvaluationDTO{}
	referenceAnswers := []ReferenceAnswerDTO{}
	categoryScoresMap := map[string][]int{}

	answeredCount := 0
	for _, q := range qaRecords {
		if q.answered() {
			answeredCount++
		}
	}

	for i, q := range qaRecords {
		var eval *questionEvalDTO
		if i < len(evaluations) {
			eval = evaluations[i]
		}

		score := 0
		if q.answered() && eval != nil {
			score = eval.Score
		}
		feedback := noFeedbackMsg
		if eval != nil && eval.Feedback != nil {
			feedback = *eval.Feedback
		}
		refAnswer := ""
		if eval != nil && eval.ReferenceAnswer != nil {
			refAnswer = *eval.ReferenceAnswer
		}
		keyPoints := []string{}
		if eval != nil && eval.KeyPoints != nil {
			keyPoints = eval.KeyPoints
		}

		questionDetails = append(questionDetails, QuestionEvaluationDTO{
			QuestionIndex: q.questionIndex,
			Question:      q.question,
			Category:      q.category,
			UserAnswer:    q.userAnswer,
			Score:         score,
			Feedback:      feedback,
		})
		referenceAnswers = append(referenceAnswers, ReferenceAnswerDTO{
			QuestionIndex:   q.questionIndex,
			Question:        q.question,
			ReferenceAnswer: refAnswer,
			KeyPoints:       keyPoints,
		})
		categoryScoresMap[q.category] = append(categoryScoresMap[q.category], score)
	}

	categories := make([]string, 0, len(categoryScoresMap))
	for category := range categoryScoresMap {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	categoryScores := []CategoryScoreDTO{}
	for _, category := range categories {
		scores := categoryScoresMap[category]
		categoryScores = append(categoryScores, CategoryScoreDTO{
			Category:      category,
			Score:         intAvg(scores),
			QuestionCount: len(scores),
		})
	}

	overallScore := 0
	if answeredCount > 0 {
		all := make([]int, 0, len(questionDetails))
		for _, detail := range questionDetails {
			all = append(all, detail.Score)
		}
		overallScore = intAvg(all)
	}

	if strengths == nil {
		strengths = []string{}
	}
	if improvements == nil {
		improvements = []string{}
	}
	return &ReportDTO{
		SessionID:        sessionID,
		TotalQuestions:   len(qaRecords),
		OverallScore:     overallScore,
		CategoryScores:   categoryScores,
		QuestionDetails:  questionDetails,
		OverallFeedback:  overallFeedback,
		Strengths:        strengths,
		Improvements:     improvements,
		ReferenceAnswers: referenceAnswers,
	}
}

// withQuestionReferences 题目自带参考答案优先覆盖报告参考答案（对齐 Java 同名方法）。
func withQuestionReferences(report *ReportDTO, questions []QuestionDTO) {
	for i, reference := range report.ReferenceAnswers {
		var question *QuestionDTO
		for j := range questions {
			if questions[j].QuestionIndex == reference.QuestionIndex {
				question = &questions[j]
				break
			}
		}
		if question == nil || !hasText(question.ReferenceAnswer) {
			continue
		}
		keyPoints := []string{}
		if question.KeyPoints != nil {
			keyPoints = question.KeyPoints
		}
		report.ReferenceAnswers[i] = ReferenceAnswerDTO{
			QuestionIndex:   reference.QuestionIndex,
			Question:        reference.Question,
			ReferenceAnswer: *question.ReferenceAnswer,
			KeyPoints:       keyPoints,
		}
	}
}

// buildQARecordsText 批内问答记录拼接（逐字格式）：
// `问题 questionIndex=N [分类]（追问，针对 questionIndex=M）: 题干` + `回答: 内容或 (未回答)`。
func buildQARecordsText(batch []qaRecord) string {
	var sb strings.Builder
	for _, q := range batch {
		relation := ""
		if q.followUp && q.parentQuestionIndex != nil {
			relation = fmt.Sprintf("（追问，针对 questionIndex=%d）", *q.parentQuestionIndex)
		}
		sb.WriteString(fmt.Sprintf("问题 questionIndex=%d [%s]%s: %s\n",
			q.questionIndex, q.category, relation, q.question))
		answer := "(未回答)"
		if q.userAnswer != nil {
			answer = *q.userAnswer
		}
		sb.WriteString(fmt.Sprintf("回答: %s\n\n", answer))
	}
	return sb.String()
}

// intAvg 整数平均（int 截断，等价 Java (int) average()）。
func intAvg(scores []int) int {
	if len(scores) == 0 {
		return 0
	}
	total := 0
	for _, s := range scores {
		total += s
	}
	return total / len(scores)
}
