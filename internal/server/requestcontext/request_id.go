// Package requestcontext 提供不依赖 HTTP 框架的请求上下文数据存取。
package requestcontext

import "context"

// requestIDKey 是私有键类型，避免与其他包使用的上下文键发生冲突。
type requestIDKey struct{}

// WithRequestID 返回携带最终请求 ID 的子上下文，保留父上下文的取消、期限和值。
// requestID 由调用方完成校验，本方法不修改父上下文。
func WithRequestID(parent context.Context, requestID string) context.Context {
	// 1. 使用私有类型键附加请求 ID，交给后续调用继续传递。
	return context.WithValue(parent, requestIDKey{}, requestID)
}

// RequestID 读取上下文中的请求 ID；尚未设置时返回空字符串。
func RequestID(ctx context.Context) string {
	// 1. 读取约定类型的值，缺失时返回字符串零值。
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}
