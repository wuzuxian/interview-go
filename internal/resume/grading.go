package resume

import (
	"context"
	_ "embed"
	"log/slog"
	"strings"

	"github.com/wuzuxian/interview/internal/ai"
	"github.com/wuzuxian/interview/internal/apperr"
)

//go:embed prompts/resume-analysis-system.st
var systemPrompt string

//go:embed prompts/resume-analysis-user.st
var userPromptTemplate string

// ModelFactory 由 ai.Registry 适配：按 Provider ID 构建 JSON 模式的 eino ChatModel。
type ModelFactory interface {
	JSONChatModelFor(ctx context.Context, providerID string) (ai.ChatCompleter, error)
	DefaultChatID() string
}

// GradingService 简历评分服务：调用默认 Provider 的 LLM 输出结构化评分。
// 对应 Java 版 ResumeGradingService。
type GradingService struct {
	models  ModelFactory
	invoker *ai.StructuredInvoker
	logger  *slog.Logger
}

// NewGradingService 创建评分服务。
func NewGradingService(models ModelFactory, invoker *ai.StructuredInvoker, logger *slog.Logger) *GradingService {
	return &GradingService{models: models, invoker: invoker, logger: logger}
}

// gradeLLMResult LLM 返回的中间结构（字段名由提示词约束为驼峰，与 Java DTO 对齐）。
type gradeLLMResult struct {
	OverallScore int `json:"overallScore"`
	ScoreDetail  struct {
		ContentScore    int `json:"contentScore"`
		StructureScore  int `json:"structureScore"`
		SkillMatchScore int `json:"skillMatchScore"`
		ExpressionScore int `json:"expressionScore"`
		ProjectScore    int `json:"projectScore"`
	} `json:"scoreDetail"`
	Summary     string       `json:"summary"`
	Strengths   []string     `json:"strengths"`
	Suggestions []Suggestion `json:"suggestions"`
}

const gradeSchemaDesc = "{overallScore:int, scoreDetail:{contentScore:int, structureScore:int, " +
	"skillMatchScore:int, expressionScore:int, projectScore:int}, summary:string, " +
	"strengths:string[], suggestions:[{category:string, priority:string, issue:string, recommendation:string}]}"

// Analyze 分析简历文本。AI 调用失败（超时/网络/限流）时返回错误，
// 由 Stream 消费者按重试策略重新入队，重试耗尽后任务转 FAILED（前端可手动重分析）；
// 不再落库兜底的 0 分结果，避免瞬时网络故障被固化为假"分析完成"。
func (g *GradingService) Analyze(ctx context.Context, resumeText string) (*AnalysisResponse, error) {
	res, err := g.analyzeStrict(ctx, resumeText)
	if err != nil {
		g.logger.Error("简历分析失败", slog.Int("textLen", len(resumeText)), slog.String("error", err.Error()))
		return nil, err
	}
	return res, nil
}

// maxGradeTextChars 评分输入文本上限：超长简历截断，防止提示词失控导致超时。
const maxGradeTextChars = 15000

func (g *GradingService) analyzeStrict(ctx context.Context, resumeText string) (*AnalysisResponse, error) {
	chatID := g.models.DefaultChatID()
	cm, err := g.models.JSONChatModelFor(ctx, chatID)
	if err != nil {
		return nil, err
	}

	if len(resumeText) > maxGradeTextChars {
		g.logger.Warn("简历文本过长，评分前截断",
			slog.Int("originLen", len(resumeText)), slog.Int("cappedTo", maxGradeTextChars))
		resumeText = truncateRunes(resumeText, maxGradeTextChars)
	}

	userPrompt := strings.ReplaceAll(userPromptTemplate, "{resumeText}", resumeText)

	out, err := ai.InvokeStructured[gradeLLMResult](
		ctx, g.invoker, cm,
		systemPrompt, userPrompt,
		gradeSchemaDesc, "简历分析", apperr.CodeResumeAnalysisFailed)
	if err != nil {
		return nil, err
	}

	if out.Suggestions == nil {
		out.Suggestions = []Suggestion{}
	}
	if out.Strengths == nil {
		out.Strengths = []string{}
	}
	// 归一化：LLM 偶发截断输出会产生 recommendation 为空的建议，
	// 前端 `{s.recommendation || s}` 遇空值会渲染整个对象导致页面崩溃。
	out.Suggestions = NormalizeSuggestions(out.Suggestions)
	return &AnalysisResponse{
		OverallScore: out.OverallScore,
		ScoreDetail: ScoreDetail{
			ContentScore:    out.ScoreDetail.ContentScore,
			StructureScore:  out.ScoreDetail.StructureScore,
			SkillMatchScore: out.ScoreDetail.SkillMatchScore,
			ExpressionScore: out.ScoreDetail.ExpressionScore,
			ProjectScore:    out.ScoreDetail.ProjectScore,
		},
		Summary:      out.Summary,
		Strengths:    out.Strengths,
		Suggestions:  out.Suggestions,
		OriginalText: resumeText,
	}, nil
}

// truncateRunes 按字符数截断，避免切出残缺的 UTF-8 序列。
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
