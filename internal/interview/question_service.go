// 真实 LLM 出题：从 Java InterviewQuestionService（笔记 01/02/04 + Java 源码 121-473 行）
// 逐字移植。无简历单路方向题；有简历双路 goroutine 并行（简历 60% + 方向 40%），
// 简历路失败降级为全方向题、方向路失败降级为简历题/兜底题；
// 结果经数量裁剪与空题单兜底后返回。兜底题为最终降级路径（票 03 逻辑复用）。
package interview

import (
	"context"
	_ "embed"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/wuzuxian/interview/internal/ai"
	"github.com/wuzuxian/interview/internal/apperr"
)

// 四组出题 prompt 逐字节拷贝自 Java 参考实现 app/src/main/resources/prompts/
//（{xxx} 为占位符，运行时按 Java 变量名替换）。
var (
	//go:embed prompts/interview-question-skill-system.st
	skillSystemPromptTemplate string

	//go:embed prompts/interview-question-skill-user.st
	skillUserPromptTemplate string

	//go:embed prompts/interview-question-resume-system.st
	resumeSystemPromptTemplate string

	//go:embed prompts/interview-question-resume-user.st
	resumeUserPromptTemplate string
)

// 常量对齐 Java InterviewQuestionService.java:49-64。
const (
	// defaultQuestionType LLM 返回 type 为空时的大写兜底分类 key。
	defaultQuestionType = "GENERAL"
	// resumeQuestionRatio 有简历时简历题占比（数量 = max(1, round(count*0.6))）。
	resumeQuestionRatio = 0.6
)

// genericModeSystemAppend 无简历通用模式附加段（对齐 Java GENERIC_MODE_SYSTEM_APPEND
// text block 实际值：前导两个换行 + 正文 + 尾随换行）。只追加在方向题 system，
// 简历题 system 不追加（对齐 Java 206-208 与 251-254 行的差异）。
const genericModeSystemAppend = "\n\n# 通用面试模式\n" +
	"本次面试无候选人简历，请出该方向的标准面试题。\n" +
	"- 禁止出现\"你在简历中提到...\"、\"你在项目中...\"等暗示存在简历的表述\n" +
	"- 问题表述应与简历无关，直接考察该方向的技术能力\n"

// difficultyDescriptions 难度描述映射（对齐 Java DIFFICULTY_DESCRIPTIONS 逐字）；
// 未知难度一律回落默认难度（mid）的描述，不报错。
var difficultyDescriptions = map[string]string{
	"junior": "校招/0-1年经验。考察基础概念和简单应用。",
	"mid":    "1-3年经验。考察原理理解和实战经验。",
	"senior": "3年+经验。考察架构设计和深度调优。",
}

// questionSchemaDesc 输出 JSON 形状说明，等价 Java BeanOutputConverter#getFormat()
// 为 QuestionListDTO 生成的结构约束（由结构化输出器追加到 system prompt 最末）。
const questionSchemaDesc = "{questions:[{question:string, type:string, category:string, topicSummary:string, followUps:[string]}]}"

// llmQuestion / llmQuestionList LLM 出题输出目标结构，
// 对齐 Java 私有 record QuestionDTO(question, type, category, topicSummary, followUps)
// 与 QuestionListDTO(questions)。category/topicSummary 可空（Java String 语义）。
type llmQuestion struct {
	Question     string   `json:"question"`
	Type         string   `json:"type"`
	Category     *string  `json:"category"`
	TopicSummary *string  `json:"topicSummary"`
	FollowUps    []string `json:"followUps"`
}

type llmQuestionList struct {
	Questions []llmQuestion `json:"questions"`
}

// QuestionService 出题服务：对应 Java InterviewQuestionService。
// 复用 Skill 服务的注册表/参考题库注入/清洗器（Java 中三者同为注入的 bean）。
type QuestionService struct {
	skills  *Service
	models  ModelResolver
	invoker *ai.StructuredInvoker
	// followUpCount 每题追问数（构造时钳制到 0-2，对齐 Java 第 109 行）。
	followUpCount int
	logger        *slog.Logger
}

// NewQuestionService 创建出题服务；followUpCount 钳制规则对齐 Java 构造器。
func NewQuestionService(skills *Service, models ModelResolver, invoker *ai.StructuredInvoker, followUpCount int, logger *slog.Logger) *QuestionService {
	return &QuestionService{
		skills:        skills,
		models:        models,
		invoker:       invoker,
		followUpCount: ClampFollowUpCount(followUpCount),
		logger:        logger,
	}
}

// QuestionGenParams 一次出题请求，字段对应 Java generateQuestionsBySkill 入参。
type QuestionGenParams struct {
	LLMProvider      *string
	SkillID          string
	Difficulty       string
	ResumeText       *string
	QuestionCount    int
	Historical       []HistoricalQuestion
	CustomCategories []CategoryDTO
	JDText           *string
}

// GenerateQuestionsBySkill 出题总入口（对齐 Java generateQuestionsBySkill，121-191 行）：
// 方向解析（custom 临时构建）→ 难度描述 → LLM 客户端 → 无简历单路 / 有简历双路并行。
func (s *QuestionService) GenerateQuestionsBySkill(ctx context.Context, p QuestionGenParams) ([]QuestionDTO, error) {
	skill, err := s.resolveSkill(p.SkillID, p.CustomCategories, p.JDText)
	if err != nil {
		return nil, err
	}
	difficultyDesc := resolveDifficulty(p.Difficulty)

	// LLM 客户端解析一次、双路共用（对齐 Java 133-134 行在路径外解析）。
	providerID := DefaultChatProviderID(p.LLMProvider, s.models.DefaultChatID())
	cm, err := s.models.JSONChatModelFor(ctx, providerID)
	if err != nil {
		// Provider 不存在/禁用：透传 aibridge/设置模块既有业务错误。
		return nil, err
	}

	historicalSection := BuildHistoricalSection(p.Historical)

	hasResume := p.ResumeText != nil && strings.TrimSpace(*p.ResumeText) != ""
	if !hasResume {
		return s.generateDirectionOnly(ctx, cm, skill, difficultyDesc, p.QuestionCount, historicalSection)
	}

	// 简历题/方向题比例切分（对齐 Java 143-144 行）：
	// resumeCount = max(1, round(count*0.6))；questionCount=1 → resumeCount=1, directionCount=0。
	resumeCount := int(float64(p.QuestionCount)*resumeQuestionRatio + 0.5)
	if resumeCount < 1 {
		resumeCount = 1
	}
	directionCount := p.QuestionCount - resumeCount

	if s.logger != nil {
		s.logger.Info("并行出题",
			slog.String("skill", p.SkillID), slog.Int("total", p.QuestionCount),
			slog.Int("resumeCount", resumeCount), slog.Int("directionCount", directionCount))
	}

	// 双路并行（对齐 Java CompletableFuture.supplyAsync）：方向路用可取消子上下文，
	// 简历路失败时中断在途的方向调用后再整场重出（对齐 Java directionFuture.cancel(true)）。
	dirCtx, dirCancel := context.WithCancel(ctx)
	defer dirCancel()

	type pathResult struct {
		questions []QuestionDTO
		err       error
	}
	resumeCh := make(chan pathResult, 1)
	directionCh := make(chan pathResult, 1)

	go func() {
		qs, err := s.generateResumeQuestions(ctx, cm, *p.ResumeText, resumeCount, skill, difficultyDesc, historicalSection)
		resumeCh <- pathResult{questions: qs, err: err}
	}()
	go func() {
		qs, err := s.generateDirectionOnly(dirCtx, cm, skill, difficultyDesc, directionCount, historicalSection)
		directionCh <- pathResult{questions: qs, err: err}
	}()

	resumeRes := <-resumeCh
	if resumeRes.err != nil {
		if s.logger != nil {
			s.logger.Error("简历题生成失败，降级为全方向题", slog.String("error", resumeRes.err.Error()))
		}
		dirCancel()
		return s.generateDirectionOnly(ctx, cm, skill, difficultyDesc, p.QuestionCount, historicalSection)
	}
	resumeQuestions := resumeRes.questions

	directionRes := <-directionCh
	if directionRes.err != nil {
		if s.logger != nil {
			s.logger.Error("方向题生成失败，降级为全简历题", slog.String("error", directionRes.err.Error()))
		}
		if len(resumeQuestions) == 0 {
			return s.generateFallbackQuestions(skill, p.QuestionCount), nil
		}
		return resumeQuestions, nil
	}
	directionQuestions := directionRes.questions

	if len(resumeQuestions) == 0 && len(directionQuestions) == 0 {
		if s.logger != nil {
			s.logger.Warn("简历题和方向题均为空，回退到默认问题")
		}
		return s.generateFallbackQuestions(skill, p.QuestionCount), nil
	}

	merged := mergeQuestionBatches(resumeQuestions, directionQuestions)
	if s.logger != nil {
		s.logger.Info("并行出题成功",
			slog.Int("resume", countMainQuestions(resumeQuestions)),
			slog.Int("direction", countMainQuestions(directionQuestions)),
			slog.Int("total", len(merged)))
	}
	return merged, nil
}

// generateResumeQuestions 简历题路（对齐 Java 193-228 行）：任何错误都作为路径失败上抛，
// 由调用方决定降级。system 不追加通用模式段（对齐 Java 206-208 行）。
func (s *QuestionService) generateResumeQuestions(ctx context.Context, cm ai.ChatCompleter,
	resumeText string, questionCount int, skill *SkillDTO, difficultyDesc, historicalSection string) ([]QuestionDTO, error) {

	user := renderPrompt(resumeUserPromptTemplate, map[string]string{
		"questionCount":         strconv.Itoa(questionCount),
		"followUpCount":         strconv.Itoa(s.followUpCount),
		"skillName":             skill.Name,
		"skillDescription":      skill.Description,
		"difficultyDescription": difficultyDesc,
		"resumeText":            resumeText,
		"historicalSection":     historicalSection,
	})
	system := resumeSystemPromptTemplate + s.buildSkillPersonaSection(skill) + antiInjectionInstruction

	out, err := ai.InvokeStructured[llmQuestionList](ctx, s.invoker, cm, system, user,
		questionSchemaDesc, "简历题", apperr.CodeInterviewQuestionGenerationFailed)
	if err != nil {
		return nil, err
	}

	questions := s.convertToQuestions(out)
	questions = capToMainCount(questions, questionCount, s.logger)
	if s.logger != nil {
		s.logger.Info("简历题生成完成",
			slog.Int("请求", questionCount), slog.Int("实际主问题", countMainQuestions(questions)))
	}
	return questions, nil
}

// generateDirectionOnly 方向题路（对齐 Java 230-278 行）：
// 分配表 → 参考题库/JD 注入 → 结构化调用；主问题数为 0 回退兜底题；
// 失败上抛（Go 结构化调用器失败一律为业务错误，等价 Java BusinessException 上抛，
// 由调用方按路径失败降级）。
func (s *QuestionService) generateDirectionOnly(ctx context.Context, cm ai.ChatCompleter,
	skill *SkillDTO, difficultyDesc string, questionCount int, historicalSection string) ([]QuestionDTO, error) {

	allocation := CalculateAllocation(skill.Categories, questionCount)
	allocationTable := BuildAllocationDescription(allocation, skill.Categories)

	if s.logger != nil {
		s.logger.Info("方向题生成",
			slog.String("skill", skill.ID), slog.Int("total", questionCount),
			slog.String("allocation", fmtAllocation(allocation)))
	}

	user := renderPrompt(skillUserPromptTemplate, map[string]string{
		"questionCount":         strconv.Itoa(questionCount),
		"followUpCount":         strconv.Itoa(s.followUpCount),
		"difficultyDescription": difficultyDesc,
		"skillName":             skill.Name,
		"skillDescription":      skill.Description,
		"allocationTable":       allocationTable,
		"historicalSection":     historicalSection,
		"referenceSection":      s.skills.BuildReferenceSection(skill, allocation),
		"jdSection":             buildJdSection(s.skills.sanitizer, skill.SourceJd),
	})
	system := skillSystemPromptTemplate + s.buildSkillPersonaSection(skill) +
		genericModeSystemAppend + antiInjectionInstruction

	out, err := ai.InvokeStructured[llmQuestionList](ctx, s.invoker, cm, system, user,
		questionSchemaDesc, "方向题", apperr.CodeInterviewQuestionGenerationFailed)
	if err != nil {
		return nil, err
	}

	questions := s.convertToQuestions(out)
	if countMainQuestions(questions) == 0 {
		if s.logger != nil {
			s.logger.Warn("方向题返回空题单，回退到默认问题")
		}
		return s.generateFallbackQuestions(skill, questionCount), nil
	}
	questions = capToMainCount(questions, questionCount, s.logger)
	if s.logger != nil {
		s.logger.Info("方向题生成完成",
			slog.Int("请求", questionCount), slog.Int("实际主问题", countMainQuestions(questions)))
	}
	return questions, nil
}

// generateFallbackQuestions 兜底题（对齐 Java generateFallbackQuestions，372-407 行；
// 复用票 03 移植的静态生成器）。
func (s *QuestionService) generateFallbackQuestions(skill *SkillDTO, count int) []QuestionDTO {
	categories := []CategoryDTO{}
	if skill != nil {
		categories = skill.Categories
	}
	return GenerateFallbackQuestions(categories, count, s.followUpCount)
}

// resolveSkill 方向解析（对齐 Java resolveSkill，301-307 行）：
// custom 且给了 customCategories → 临时构建自定义方向；否则查预设注册表。
func (s *QuestionService) resolveSkill(skillID string, customCategories []CategoryDTO, jdText *string) (*SkillDTO, error) {
	if skillID == CustomSkillID && len(customCategories) > 0 {
		jd := ""
		if jdText != nil {
			jd = *jdText
		}
		return s.skills.BuildCustomSkill(customCategories, jd), nil
	}
	return s.skills.GetSkill(skillID)
}

// resolveDifficulty 未知难度回落默认难度描述（对齐 Java 309-313 行，不报错）。
func resolveDifficulty(difficulty string) string {
	if d, ok := difficultyDescriptions[difficulty]; ok {
		return d
	}
	return difficultyDescriptions[DefaultDifficulty]
}

// DefaultChatProviderID 对齐 Java resolveProviderId：空/空白/“default”（忽略大小写）→ 默认 Provider。
func DefaultChatProviderID(providerID *string, defaultID string) string {
	if providerID == nil {
		return defaultID
	}
	trimmed := strings.TrimSpace(*providerID)
	if trimmed == "" || strings.EqualFold(trimmed, "default") {
		return defaultID
	}
	return trimmed
}

// convertToQuestions LLM 输出 → 题目列表（对齐 Java convertToQuestions，315-341 行）：
// 跳过空白题面；type 大写、空白兜底 GENERAL；追问展开为主问题后的独立题目
//（isFollowUp=true、parentQuestionIndex 指向主问题）。
func (s *QuestionService) convertToQuestions(dto *llmQuestionList) []QuestionDTO {
	questions := []QuestionDTO{}
	index := 0
	if dto == nil {
		return questions
	}

	for _, q := range dto.Questions {
		if strings.TrimSpace(q.Question) == "" {
			continue
		}
		typ := strings.ToUpper(strings.TrimSpace(q.Type))
		if typ == "" {
			typ = defaultQuestionType
		}
		mainIndex := index
		questions = append(questions, createQuestion(index, q.Question, typ, q.Category, q.TopicSummary, false, nil))
		index++

		for i, fu := range sanitizeFollowUps(q.FollowUps, s.followUpCount) {
			questions = append(questions, createQuestion(
				index, fu, typ, stringPtr(BuildFollowUpCategory(categoryValue(q.Category), i+1)), nil, true, intPtr(mainIndex)))
			index++
		}
	}
	return questions
}

// createQuestion 对齐 Java InterviewQuestionDTO.create。
func createQuestion(index int, question, typ string, category, topicSummary *string, isFollowUp bool, parentQuestionIndex *int) QuestionDTO {
	return QuestionDTO{
		QuestionIndex:       index,
		Question:            question,
		Type:                stringPtr(typ),
		Category:            category,
		TopicSummary:        topicSummary,
		IsFollowUp:          isFollowUp,
		ParentQuestionIndex: parentQuestionIndex,
		KeyPoints:           nil,
	}
}

// sanitizeFollowUps 对齐 Java sanitizeFollowUps（452-461 行）：过滤空白、trim、截到 followUpCount。
func sanitizeFollowUps(followUps []string, followUpCount int) []string {
	if followUpCount == 0 || len(followUps) == 0 {
		return nil
	}
	out := make([]string, 0, len(followUps))
	for _, f := range followUps {
		if strings.TrimSpace(f) == "" {
			continue
		}
		out = append(out, strings.TrimSpace(f))
		if len(out) == followUpCount {
			break
		}
	}
	return out
}

// countMainQuestions 主问题数（isFollowUp=false）。
func countMainQuestions(questions []QuestionDTO) int {
	n := 0
	for _, q := range questions {
		if !q.IsFollowUp {
			n++
		}
	}
	return n
}

// capToMainCount 主问题数量裁剪（对齐 Java capToMainCount，346-370 行）：
// 多生截断（追问随主问题截断）；少生保留原样并告警。
func capToMainCount(questions []QuestionDTO, maxMainCount int, logger *slog.Logger) []QuestionDTO {
	current := countMainQuestions(questions)
	if current <= maxMainCount {
		if current < maxMainCount && logger != nil {
			logger.Warn("AI 生成主问题不足", slog.Int("请求", maxMainCount), slog.Int("实际", current))
		}
		return questions
	}

	capped := []QuestionDTO{}
	mainSeen := 0
	for _, q := range questions {
		if !q.IsFollowUp {
			mainSeen++
		}
		if mainSeen > maxMainCount {
			break
		}
		capped = append(capped, q)
	}
	if logger != nil {
		logger.Info("题目截断", slog.Int("from", current), slog.Int("to", maxMainCount))
	}
	return capped
}

// mergeQuestionBatches 双路合并（对齐 Java mergeQuestionBatches，280-299 行）：
// 简历题在前，方向题索引整体后移（含 parentQuestionIndex）。
func mergeQuestionBatches(first, second []QuestionDTO) []QuestionDTO {
	if len(second) == 0 {
		return first
	}
	if len(first) == 0 {
		return second
	}
	offset := len(first)
	merged := make([]QuestionDTO, 0, len(first)+len(second))
	merged = append(merged, first...)
	for _, q := range second {
		var newParent *int
		if q.ParentQuestionIndex != nil {
			newParent = intPtr(*q.ParentQuestionIndex + offset)
		}
		merged = append(merged, createQuestion(
			q.QuestionIndex+offset, q.Question, categoryValue(q.Type), q.Category, q.TopicSummary, q.IsFollowUp, newParent))
	}
	return merged
}

// BuildHistoricalSection 历史知识点段落（对齐 Java buildHistoricalSection，409-432 行，逐字）：
// 按分类分组（type 空白兜底 GENERAL；topicSummary 空白截取题面前 30 字兜底）。
// Java 用 HashMap 无序，这里按分类名排序保证输出稳定（主控裁决 #11 同款原则）。
func BuildHistoricalSection(hqs []HistoricalQuestion) string {
	if len(hqs) == 0 {
		return "暂无历史提问"
	}

	grouped := map[string][]string{}
	var order []string
	for _, hq := range hqs {
		typ := hq.Type
		if strings.TrimSpace(typ) == "" {
			typ = defaultQuestionType
		}
		summary := ""
		if hq.TopicSummary != nil {
			summary = *hq.TopicSummary
		}
		if strings.TrimSpace(summary) == "" {
			q := hq.Question
			r := []rune(q)
			if len(r) > 30 {
				summary = string(r[:30]) + "…"
			} else {
				summary = q
			}
		}
		if _, ok := grouped[typ]; !ok {
			order = append(order, typ)
		}
		grouped[typ] = append(grouped[typ], summary)
	}
	sort.Strings(order)

	var sb strings.Builder
	sb.WriteString("已考过的知识点（避免重复出题）：\n")
	for _, typ := range order {
		sb.WriteString("- " + typ + ": ")
		sb.WriteString(strings.Join(grouped[typ], ", "))
		sb.WriteString("\n")
	}
	return sb.String()
}

// buildJdSection JD 注入段（对齐 Java buildJdSection，434-441 行，逐字）：
// 空 JD 返回 ""；否则数据边界声明 + 清洗后的 JD 随机边界包裹。
func buildJdSection(sanitizer *PromptSanitizer, sourceJd *string) string {
	if sourceJd == nil || strings.TrimSpace(*sourceJd) == "" {
		return ""
	}
	return dataBoundaryInstruction + "\n" +
		"## 职位描述（JD）\n根据以下 JD 关键要求出题，确保题目与岗位实际需求相关：\n" +
		sanitizer.WrapWithDelimiters("jd", sanitizer.Sanitize(*sourceJd))
}

// buildSkillPersonaSection 考官人设段（对齐 Java buildSkillPersonaSection，443-450 行）：
// persona 为空返回 ""；否则 SKILL.md 正文经随机边界包裹（不清洗，对齐 Java）。
func (s *QuestionService) buildSkillPersonaSection(skill *SkillDTO) string {
	if skill == nil || skill.Persona == nil || strings.TrimSpace(*skill.Persona) == "" {
		return ""
	}
	return "\n\n# Skill Persona\n" +
		"以下内容来自当前面试方向的 SKILL.md，请作为面试官角色、风格与出题约束：\n" +
		s.skills.sanitizer.WrapWithDelimiters("skill_persona", *skill.Persona)
}

// renderPrompt 模板占位符替换（{xxx} → 变量值；同一占位符可出现多次）。
func renderPrompt(tpl string, vars map[string]string) string {
	for k, v := range vars {
		tpl = strings.ReplaceAll(tpl, "{"+k+"}", v)
	}
	return tpl
}

func categoryValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// fmtAllocation 分配表的日志摘要（键排序保证稳定）。
func fmtAllocation(allocation map[string]int) string {
	keys := make([]string, 0, len(allocation))
	for k := range allocation {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Itoa(allocation[k]))
	}
	return strings.Join(parts, ",")
}
