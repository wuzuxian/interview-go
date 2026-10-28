// 评估执行接缝（票 06）：消费者与 report 同步兜底共用的引擎调用封装。
// 生产实现经 ModelResolver 解析 ChatCompleter 后调用票 05 引擎；测试注入 stub 引擎。
package interview

import (
	"context"
	"errors"
	"log/slog"

	"github.com/wuzuxian/interview/internal/ai"
	"github.com/wuzuxian/interview/internal/apperr"
)

// EvaluationRunner 评估执行接缝：DB 重放后的会话数据 → 报告。
// providerID 为场次 llm_provider 列（nil/空/"default" → 默认 Provider）。
type EvaluationRunner interface {
	Evaluate(ctx context.Context, input EvaluateInput, providerID *string) (*ReportDTO, error)
}

// ReferenceSource 评估参考段来源（方向参考基线兜底，Java
// InterviewSkillService.buildEvaluationReferenceSectionSafe 语义）。
type ReferenceSource interface {
	BuildEvaluationReferenceSectionSafe(skillID string) string
}

// EngineRunner 生产实现：ModelResolver 按会话 llm_provider 解析 ChatCompleter
// （空则默认 provider），调用票 05 评估引擎。
type EngineRunner struct {
	engine  *EvaluationEngine
	invoker *ai.StructuredInvoker
	models  ModelResolver
	logger  *slog.Logger
}

// NewEngineRunner 创建引擎执行器。
func NewEngineRunner(engine *EvaluationEngine, invoker *ai.StructuredInvoker, models ModelResolver, logger *slog.Logger) *EngineRunner {
	return &EngineRunner{engine: engine, invoker: invoker, models: models, logger: logger}
}

// Evaluate 解析 ChatCompleter 后执行整场评估。异常归一对齐 Java
// AnswerEvaluationService.evaluateInterview：业务错误原样透传，其余包装为
// 3005 "面试评估失败"。
func (r *EngineRunner) Evaluate(ctx context.Context, input EvaluateInput, providerID *string) (*ReportDTO, error) {
	provider := DefaultChatProviderID(providerID, r.models.DefaultChatID())
	completer, err := r.models.JSONChatModelFor(ctx, provider)
	if err != nil {
		return nil, wrapEvaluationError(err)
	}
	report, err := r.engine.EvaluateSession(ctx, completer, r.invoker, input)
	if err != nil {
		return nil, wrapEvaluationError(err)
	}
	return report, nil
}

// wrapEvaluationError 异常归一（对齐 Java catch BusinessException 原样透传、
// catch Exception → INTERVIEW_EVALUATION_FAILED "面试评估失败"）。
func wrapEvaluationError(err error) error {
	var ae *apperr.AppError
	if errors.As(err, &ae) {
		return ae
	}
	return apperr.Wrap(apperr.CodeInterviewEvaluationFailed, "面试评估失败", err)
}
