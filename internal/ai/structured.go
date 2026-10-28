package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/wuzuxian/interview/internal/apperr"
)

// StructuredConfig 结构化输出调用配置，键与 Java 版 app.ai.structured-* 对齐。
type StructuredConfig struct {
	MaxAttempts                 int           // 解析失败业务层重试次数（总调用 ≤ MaxAttempts）
	IncludeLastError            bool          // 重试时注入上次错误
	RetryUseRepairPrompt        bool          // 使用修复型重试提示词
	AppendStrictJSONInstruction bool          // 重试提示词追加严格 JSON 指令
	ErrorMessageMaxLength       int           // 注入错误信息最大长度
	CallTimeout                 time.Duration // 单次调用超时
}

// DefaultStructuredConfig 默认值。
// CallTimeout 覆盖单次 LLM 调用（含长简历的长输出生成），过短会把慢而成功的
// 响应误杀为超时；消费者层还有 Stream 级 3 次重试兜底，因此这里给足预算。
func DefaultStructuredConfig() StructuredConfig {
	return StructuredConfig{
		MaxAttempts:                 2,
		IncludeLastError:            true,
		RetryUseRepairPrompt:        true,
		AppendStrictJSONInstruction: true,
		ErrorMessageMaxLength:       200,
		CallTimeout:                 300 * time.Second,
	}
}

// ChatCompleter 抽象 eino BaseChatModel.Generate，便于测试替身。
type ChatCompleter interface {
	Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

// StructuredInvoker 带修复重试的结构化输出调用器，
// 对应 Java 版 StructuredOutputInvoker + BeanOutputConverter。
type StructuredInvoker struct {
	Cfg    StructuredConfig
	Logger *slog.Logger
}

// NewStructuredInvoker 创建调用器。
func NewStructuredInvoker(cfg StructuredConfig, logger *slog.Logger) *StructuredInvoker {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}
	return &StructuredInvoker{Cfg: cfg, Logger: logger}
}

// formatInstruction 追加到 system prompt 的输出格式说明，
// 作用等价于 Java 版 BeanOutputConverter#getFormat()。
func formatInstruction(typeName string) string {
	return fmt.Sprintf(
		"Your response should be in JSON format.\n"+
			"Do not include XML, JSON escaping, markdown fences or any other formatting. "+
			"Output must be a single valid JSON object conforming to the schema of: %s.", typeName)
}

const strictJSONInstruction = "严格输出一个 JSON 对象，不要包含 Markdown 代码块标记（如 ```json）或任何解释文字。"

// InvokeStructured 调用 model（建议 JSON 响应模式）并解析到 T；
// 解析失败按配置修复重试，整体失败返回带 errCode 的业务错误。
func InvokeStructured[T any](
	ctx context.Context,
	invoker *StructuredInvoker,
	cm ChatCompleter,
	systemPrompt string,
	userPrompt string,
	typeName string,
	label string,
	errCode int,
) (*T, error) {
	systemWithFormat := systemPrompt + "\n\n" + formatInstruction(typeName)

	var lastErr string
	for attempt := 1; attempt <= invoker.Cfg.MaxAttempts; attempt++ {
		sys := systemWithFormat
		usr := userPrompt
		if attempt > 1 {
			if invoker.Cfg.RetryUseRepairPrompt && invoker.Cfg.AppendStrictJSONInstruction {
				sys = systemWithFormat + "\n\n" + strictJSONInstruction
			}
			if invoker.Cfg.IncludeLastError && lastErr != "" {
				snippet := lastErr
				if len(snippet) > invoker.Cfg.ErrorMessageMaxLength {
					snippet = snippet[:invoker.Cfg.ErrorMessageMaxLength]
				}
				usr = usr + "\n\n上一次输出解析失败，请修复以下问题后重新仅输出 JSON：" + snippet
			}
		}

		callCtx, cancel := context.WithTimeout(ctx, invoker.Cfg.CallTimeout)
		started := time.Now()
		msg, err := cm.Generate(callCtx, []*schema.Message{
			schema.SystemMessage(sys),
			schema.UserMessage(usr),
		})
		cancel()
		if invoker.Logger != nil {
			invoker.Logger.Info("结构化输出调用完成",
				slog.String("label", label), slog.Int("attempt", attempt),
				slog.Duration("elapsed", time.Since(started)),
				slog.Int("promptChars", len(sys)+len(usr)))
		}
		if err != nil {
			return nil, normalizeAIErr(err, label, errCode)
		}

		var out T
		if err := json.Unmarshal([]byte(msg.Content), &out); err != nil {
			lastErr = fmt.Sprintf("JSON 解析失败: %v", err)
			invoker.logf(label, attempt, lastErr, msg.Content)
			continue
		}
		return &out, nil
	}
	return nil, apperr.New(errCode, label+"失败："+lastErr)
}

func (i *StructuredInvoker) logf(label string, attempt int, reason, content string) {
	if i.Logger == nil {
		return
	}
	snippet := content
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	i.Logger.Warn("结构化输出解析失败",
		slog.String("label", label), slog.Int("attempt", attempt),
		slog.String("reason", reason), slog.String("content_snippet", snippet))
}

// normalizeAIErr 将网络/HTTP 错误转换为 Java 版全局异常处理器的语义：
// 超时 → 7002，401 → 7004，429 → 7005，连接类 → 7001，其余 → errCode/7003。
func normalizeAIErr(err error, label string, errCode int) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "Client.Timeout"):
		return apperr.New(apperr.CodeAIServiceTimeout, "AI服务响应超时，请稍后重试")
	case strings.Contains(msg, "401"), strings.Contains(msg, "Unauthorized"):
		return apperr.New(apperr.CodeAIAPIKeyInvalid, "AI服务密钥无效，请联系管理员")
	case strings.Contains(msg, "429"), strings.Contains(msg, "Too Many Requests"):
		return apperr.New(apperr.CodeAIRateLimitExceeded, "AI服务调用过于频繁，请稍后重试")
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "handshake"),
		strings.Contains(msg, "no such host"), strings.Contains(msg, "proxyconnect"):
		return apperr.New(apperr.CodeAIServiceUnavailable, "AI服务暂时不可用，请稍后重试")
	default:
		if errCode != 0 && errCode != apperr.CodeAIServiceError {
			return apperr.New(errCode, label+"失败")
		}
		return apperr.New(apperr.CodeAIServiceError, "AI服务调用失败，请稍后重试")
	}
}
