// Package middleware 提供 HTTP 请求处理链中的公共能力。
package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// RequestIDHeader 是请求和响应共同使用的请求标识头名称。
const RequestIDHeader = "X-Request-ID"

// RequestID 校验单个 36 字符 UUID v4 请求头，缺失或无效时生成新值。
// 最终小写 ID 写入请求上下文、请求头及响应头，随后继续处理请求。
func RequestID() gin.HandlerFunc {
	return func(requestContext *gin.Context) {
		// 1. 读取单个请求 ID，校验长度、格式和 UUID 类型。
		headerValues := requestContext.Request.Header.Values(RequestIDHeader)
		requestID := ""
		if len(headerValues) == 1 && len(headerValues[0]) == 36 {
			parsedID, err := uuid.Parse(headerValues[0])
			if err == nil && parsedID.Version() == 4 && parsedID.Variant() == uuid.RFC4122 {
				requestID = parsedID.String()
			}
		}

		// 2. 输入无效或缺失时，生成新的 UUID v4。
		if requestID == "" {
			requestID = uuid.NewString()
		}

		// 3. 将最终 ID 写入请求上下文、请求头和响应头。
		requestContext.Request.Header.Set(RequestIDHeader, requestID)
		ctx := requestcontext.WithRequestID(requestContext.Request.Context(), requestID)
		requestContext.Request = requestContext.Request.WithContext(ctx)
		requestContext.Header(RequestIDHeader, requestID)

		// 4. 继续执行后续中间件和处理函数。
		requestContext.Next()
	}
}
