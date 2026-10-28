// Package apperr 定义业务错误：错误码对齐 Java 版 ErrorCode，
// 由统一响应层转换为 HTTP 200 + Result{code,message} 结构。
package apperr

import "fmt"

// AppError 业务错误，携带业务码与用户可读消息。
type AppError struct {
	Code    int
	Message string
	Cause   error
}

func (e *AppError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%d] %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

func (e *AppError) Unwrap() error { return e.Cause }

// New 创建业务错误。
func New(code int, message string) *AppError { return &AppError{Code: code, Message: message} }

// Wrap 创建带原因的业务错误。
func Wrap(code int, message string, cause error) *AppError {
	return &AppError{Code: code, Message: message, Cause: cause}
}

// From 将任意错误转换为 *AppError；已是 AppError 时原样返回，否则按 500 兜底。
func From(err error) *AppError {
	if err == nil {
		return nil
	}
	if ae, ok := err.(*AppError); ok {
		return ae
	}
	return &AppError{Code: CodeInternalError, Message: "系统繁忙，请稍后重试", Cause: err}
}

// 业务错误码，与 Java 版 interview.guide.common.exception.ErrorCode 对齐。
const (
	CodeBadRequest           = 400
	CodeUnauthorized         = 401
	CodeForbidden            = 403
	CodeNotFound             = 404
	CodeMethodNotAllowed     = 405
	CodeInternalError        = 500
	CodeRateLimitExceeded    = 8001
	CodeResumeNotFound       = 2001
	CodeResumeParseFailed    = 2002
	CodeResumeUploadFailed   = 2003
	CodeResumeAnalysisFailed = 2007
	CodeAnalysisNotFound     = 2008
	// 面试模块 3xxx（对齐 Java ErrorCode 面试模块段）。
	// 3001-3009 为 Java 会话段原文（含默认消息），见笔记 08 §1。
	CodeInterviewSessionNotFound          = 3001
	CodeInterviewSessionExpired           = 3002
	CodeInterviewQuestionNotFound         = 3003
	CodeInterviewAlreadyCompleted         = 3004
	CodeInterviewEvaluationFailed         = 3005
	CodeInterviewQuestionGenerationFailed = 3006
	CodeInterviewNotCompleted             = 3007
	CodeInterviewAnswerSaveFailed         = 3008
	CodeInterviewQuestionInsufficient     = 3009
	// Java 版查询不存在的方向复用 BAD_REQUEST，
	// 本仓库按票面要求给 404 语义，取 3101 避让会话段。
	CodeSkillNotFound        = 3101
	CodeStorageUploadFailed  = 4001
	CodeStorageDownloadFail  = 4002
	CodeStorageDeleteFailed  = 4003
	CodeExportPDFFailed      = 5001
	CodeAIServiceUnavailable = 7001
	CodeAIServiceTimeout     = 7002
	CodeAIServiceError       = 7003
	CodeAIAPIKeyInvalid      = 7004
	CodeAIRateLimitExceeded  = 7005
	CodeProviderNotFound     = 11001
	CodeProviderExists       = 11002
	CodeProviderReadFailed   = 11004
	CodeProviderWriteFailed  = 11005
	CodeDefaultNoDelete      = 11007
	CodeVoiceReadFailed      = 11009
	CodeVoiceWriteFailed     = 11010
)

// 预置常用错误。
func BadRequest(message string) *AppError { return New(CodeBadRequest, message) }

func NotFound(message string) *AppError { return New(CodeNotFound, message) }

func Internal(message string) *AppError { return New(CodeInternalError, message) }

func ResumeNotFound() *AppError { return New(CodeResumeNotFound, "简历不存在") }

// SkillNotFound 面试方向不存在，消息与 Java 版 getSkill 一致。
func SkillNotFound(skillID string) *AppError {
	return New(CodeSkillNotFound, "未找到面试主题: "+skillID)
}

// InterviewSessionNotFound 面试会话不存在（3001 默认消息）。
func InterviewSessionNotFound() *AppError {
	return New(CodeInterviewSessionNotFound, "面试会话不存在")
}

// InterviewSessionNotFoundWithMessage 3001 + 自定义消息（unfinished 无记录用）。
func InterviewSessionNotFoundWithMessage(message string) *AppError {
	return New(CodeInterviewSessionNotFound, message)
}
