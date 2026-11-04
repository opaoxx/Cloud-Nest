package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// InternalError 处理可恢复的普通 panic，忽略原始载荷，不回显内部信息。
// 响应未提交时返回统一 500；已提交时只中止处理，不改写已发送的响应。
func InternalError(requestContext *gin.Context, _ any) {
	// 1. 已提交响应时停止处理，避免覆盖或拼接正文。
	if requestContext.Writer.Written() {
		requestContext.Abort()
		return
	}

	// 2. 复用错误 DTO，并读取当前请求 ID。
	response := dto.ErrorResponse{
		Error: dto.ErrorDetail{
			Code:    "INTERNAL_ERROR",
			Message: "服务内部错误，请稍后重试",
			Details: nil,
		},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	}

	// 3. 清理未提交的正文相关头，设置 JSON 和禁止缓存。
	requestContext.Writer.Header().Del("Content-Length")
	requestContext.Writer.Header().Del("Content-Encoding")
	requestContext.Header("Content-Type", "application/json; charset=utf-8")
	requestContext.Header("Cache-Control", "no-store")

	// 4. 返回统一的 500 JSON。
	requestContext.AbortWithStatusJSON(http.StatusInternalServerError, response)
}
