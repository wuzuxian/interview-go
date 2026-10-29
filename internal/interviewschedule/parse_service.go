// 邀约解析编排（Java InterviewParseService.parse L99-125）：规则优先（0.95）→
// AI 兜底（0.8）→ 全失败（0.0）。失败形态的 log 文案逐字对齐。
package interviewschedule

import (
	"context"
	"log/slog"
	"strings"

	"github.com/wuzuxian/interview/internal/interview"
)

// ParseService 解析服务。
type ParseService struct {
	models    ChatModelFactory           // AI 兜底模型（默认 Provider，可 nil = AI 不可用）
	sanitizer *interview.PromptSanitizer // 注入防御（Sanitize + WrapWithDelimiters）
	logger    *slog.Logger
}

// NewParseService 创建解析服务。models 为默认 Provider 的对话模型工厂
// （main 注入 aibridge.RagChatModelFor；nil 表示 AI 兜底不可用）。
func NewParseService(models ChatModelFactory, sanitizer *interview.PromptSanitizer, logger *slog.Logger) *ParseService {
	if logger == nil {
		logger = slog.Default()
	}
	if sanitizer == nil {
		sanitizer = interview.NewPromptSanitizer(logger)
	}
	return &ParseService{models: models, sanitizer: sanitizer, logger: logger}
}

// Parse 解析邀约文本。rawText 空白由控制器 @NotBlank 校验拦截（400「文本不能为空」），
// 此处的「输入文本为空」分支保留服务层语义（对齐 Java L102-105）。
func (s *ParseService) Parse(ctx context.Context, rawText, source string) *ParseResponse {
	s.logger.Info("开始解析文本",
		slog.String("来源", source), slog.Int("文本长度", len(rawText)))

	if strings.TrimSpace(rawText) == "" {
		s.logger.Warn("Input text is null or empty")
		return &ParseResponse{
			Success: false, Data: nil, Confidence: 0.0, ParseMethod: "none", Log: "输入文本为空",
		}
	}

	// Step 1: 规则解析。
	result := tryRuleParsing(rawText, source)
	if isValidResult(result) {
		s.logger.Info("规则解析成功")
		return &ParseResponse{
			Success: true, Data: result, Confidence: 0.95, ParseMethod: "rule", Log: "规则解析成功",
		}
	}

	// Step 2: 规则解析失败，AI 兜底。
	s.logger.Info("规则解析失败，尝试 AI 解析")
	result = s.parseWithAI(ctx, rawText, source) // Java 现状把 source 当 provider 参数传；Go 侧裁决见 ai_parser.go
	if isValidResult(result) {
		s.logger.Info("AI 解析成功")
		return &ParseResponse{
			Success: true, Data: result, Confidence: 0.8, ParseMethod: "ai", Log: "AI 解析成功",
		}
	}

	// Step 3: 全部失败。
	s.logger.Warn("所有解析方式均失败")
	return &ParseResponse{
		Success: false, Data: nil, Confidence: 0.0, ParseMethod: "none", Log: "解析失败",
	}
}
