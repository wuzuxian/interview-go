// Package result 实现统一响应结构 Result{code,message,data}。
// 前端约定：所有业务响应都是 HTTP 200 + Result；code==200 表示成功。
package result

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wuzuxian/interview/internal/apperr"
)

// CodeSuccess 业务成功码。
const CodeSuccess = 200

// Result 统一响应体。
type Result struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// Success 构造成功响应。
func Success(data any) Result { return Result{Code: CodeSuccess, Message: "success", Data: data} }

// SuccessWithMessage 构造带自定义消息的成功响应。
func SuccessWithMessage(message string, data any) Result {
	return Result{Code: CodeSuccess, Message: message, Data: data}
}

// Error 构造失败响应。
func Error(code int, message string) Result {
	return Result{Code: code, Message: message, Data: nil}
}

// OK 以 HTTP 200 输出成功响应。
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Success(data))
}

// OKMessage 以 HTTP 200 输出带消息的成功响应。
func OKMessage(c *gin.Context, message string, data any) {
	c.JSON(http.StatusOK, SuccessWithMessage(message, data))
}

// Fail 以 HTTP 200 输出失败响应（前端按 code != 200 处理）。
func Fail(c *gin.Context, code int, message string) {
	c.JSON(http.StatusOK, Error(code, message))
}

// FailErr 将错误转换为失败响应：AppError 用其业务码，其余按 500 兜底。
func FailErr(c *gin.Context, err error) {
	ae := apperr.From(err)
	Fail(c, ae.Code, ae.Message)
}

// AbortErr 输出失败响应并终止后续处理器。
func AbortErr(c *gin.Context, err error) {
	FailErr(c, err)
	c.Abort()
}

// ErrRateLimited 限流统一错误，消息与 Java 版 RateLimitExceededException 一致。
var ErrRateLimited = apperr.New(apperr.CodeRateLimitExceeded, "请求过于频繁，请稍后再试")

// IsResultShape 判断任意值是否满足 Result 结构（供测试与调试使用）。
func IsResultShape(v any) bool {
	_, ok := v.(Result)
	return ok
}

// EnsureNoRawError 兜底确保 handler 写出的都是 Result；未写时写 500。
// 由 httpx.Recovery 在 panic 恢复与兜底路径调用。
func EnsureNoRawError(c *gin.Context) {
	if c.Writer.Written() {
		return
	}
	Fail(c, apperr.CodeInternalError, "系统繁忙，请稍后重试")
}

// AsAppError 暴露给其他包把 error 归一化为 AppError 的便捷函数。
func AsAppError(err error) *apperr.AppError { return apperr.From(err) }

// ErrNotFound 统一 404 业务错误（前端展示 "API 接口不存在"）。
var ErrNotFound = apperr.New(apperr.CodeNotFound, "API 接口不存在")

// ErrMethodNotAllowed 统一 405 业务错误。
func ErrMethodNotAllowed(method string) error {
	return apperr.New(apperr.CodeMethodNotAllowed, "请求方法不支持: "+method)
}

// IsSuccess 判断业务码是否成功。
func IsSuccess(code int) bool { return code == CodeSuccess }
