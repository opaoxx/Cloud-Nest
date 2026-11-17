package binding

import (
	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// abortRequest 输出固定输入错误并停止处理，返回 false 供调用方直接返回。
func abortRequest(requestContext *gin.Context, status int, code, message string) bool {
	// 1. 沿用公共错误 DTO，关联请求 ID，不包含解析器错误或原始输入。
	response := dto.ErrorResponse{
		Error:     dto.ErrorDetail{Code: code, Message: message, Details: nil},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	}
	// 2. 返回不可缓存的错误，并告诉调用方停止处理。
	requestContext.Header("Cache-Control", "no-store")
	requestContext.AbortWithStatusJSON(status, response)
	return false
}
