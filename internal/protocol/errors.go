// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

import "net/http"

// 统一错误码。Agent 与 Hub 都只使用这些取值，不各自造词，
// 否则 Hub 无法判断一个失败到底是 Agent 不可用还是请求本身有问题。
const (
	// CodeBadRequest 请求参数或请求体不合法。
	CodeBadRequest = "bad_request"
	// CodeUnauthorized 凭证缺失或无效（未带 token、token 已撤销等）。
	CodeUnauthorized = "unauthorized"
	// CodeAgentSignatureInvalid HMAC 签名校验失败，或时间戳超出允许的窗口。
	// 单独成一个码，是为了让「签名不对」和「token 不对」在审计里可区分。
	CodeAgentSignatureInvalid = "agent_signature_invalid"
	// CodeForbidden 凭证有效但权限不足（scope 不够、试图操作保护对象等）。
	CodeForbidden = "forbidden"
	// CodeNotFound 目标对象不存在。
	CodeNotFound = "not_found"
	// CodeConflict 对象存在但状态不允许该操作（如对已停止的容器执行 stop）。
	CodeConflict = "conflict"
	// CodeRateLimited 触发限流。
	CodeRateLimited = "rate_limited"
	// CodeTooLarge 上传或请求体超出限制。
	CodeTooLarge = "too_large"
	// CodePathEscape 路径逃逸：含 ..、符号链接越界，或 realpath 后不在根白名单内。
	CodePathEscape = "path_escape"
	// CodePathDenied 路径合法但命中敏感文件黑名单（私钥、凭据目录等）。
	CodePathDenied = "path_denied"
	// CodeUpstreamUnavailable Agent 不可达或超时。
	CodeUpstreamUnavailable = "upstream_unavailable"
	// CodeUpstreamError Agent 可达但返回了非预期的结果。
	CodeUpstreamError = "upstream_error"
	// CodeInternal 兜底错误码。
	CodeInternal = "internal"
)

// Error 是 Hub 与 Agent 之间传递的业务错误。
//
// 它同时实现 error 接口与 JSON 序列化，Agent 侧可直接作为错误响应体写出，
// Hub 侧可直接解回，不需要再做一层字符串解析。
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error 实现 error 接口。
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// NewError 构造一个业务错误。
func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// codeStatus 是错误码到 HTTP 状态码的唯一映射表。
// 新增错误码必须同步此表，TestStatusForCodeCoversAllCodes 会拦住漏加的情况。
var codeStatus = map[string]int{
	CodeBadRequest:            http.StatusBadRequest,
	CodeUnauthorized:          http.StatusUnauthorized,
	CodeAgentSignatureInvalid: http.StatusUnauthorized,
	CodeForbidden:             http.StatusForbidden,
	CodeNotFound:              http.StatusNotFound,
	CodeConflict:              http.StatusConflict,
	CodeRateLimited:           http.StatusTooManyRequests,
	CodeTooLarge:              http.StatusRequestEntityTooLarge,
	CodePathEscape:            http.StatusBadRequest,
	CodePathDenied:            http.StatusForbidden,
	CodeUpstreamUnavailable:   http.StatusBadGateway,
	CodeUpstreamError:         http.StatusBadGateway,
	CodeInternal:              http.StatusInternalServerError,
}

// StatusForCode 返回错误码对应的 HTTP 状态码。
//
// Hub 转发 Agent 的错误时必须经过它：Agent 的内部故障应表现为 upstream_*，
// 而不是让调用方误以为 Hub 自己挂了。未知错误码落到 500，避免把无法归类的失败
// 伪装成客户端错误。
func StatusForCode(code string) int {
	if status, ok := codeStatus[code]; ok {
		return status
	}
	return http.StatusInternalServerError
}
