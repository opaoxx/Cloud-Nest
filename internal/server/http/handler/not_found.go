package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// NotFound 返回未匹配路由的统一 404 响应，依赖前置 RequestID 中间件。
// 不回显请求内容，响应 ID 与上下文、响应头和完成日志保持一致。
func NotFound(requestContext *gin.Context) {
	// 1. 构造固定错误信息，不回显请求路径或内部数据。
	errorDetail := dto.ErrorDetail{
		Code:    "ROUTE_NOT_FOUND",
		Message: "未找到匹配的接口",
		Details: nil,
	}

	// 2. 从当前请求上下文读取最终请求 ID。
	response := dto.ErrorResponse{
		Error:     errorDetail,
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	}

	// 3. 返回禁止缓存的 404 JSON，并停止后续处理。
	requestContext.Header("Cache-Control", "no-store")
	requestContext.AbortWithStatusJSON(http.StatusNotFound, response)
}
