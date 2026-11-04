// Package dto 定义 HTTP 层传输的数据结构和 JSON 字段。
package dto

// ErrorResponse 是统一错误响应，关联本次请求的最终 ID。
type ErrorResponse struct {
	// Error 保存可向客户端公开的错误信息。
	Error ErrorDetail `json:"error"`
	// RequestID 与响应头和请求完成日志中的标识保持一致。
	RequestID string `json:"request_id"`
}

// ErrorDetail 保存错误码、用户提示及可选的结构化补充信息。
type ErrorDetail struct {
	// Code 是供客户端判断错误类别的稳定业务码。
	Code string `json:"code"`
	// Message 是面向客户端的提示，不包含内部异常信息。
	Message string `json:"message"`
	// Details 保存可公开的补充信息，未提供时明确序列化为 null。
	Details map[string]any `json:"details"`
}
