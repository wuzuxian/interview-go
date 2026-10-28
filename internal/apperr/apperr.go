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
