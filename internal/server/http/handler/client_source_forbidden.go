package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// ClientSourceForbidden 返回登录来源/客户端声明不合规的统一 403，不回显输入。
func ClientSourceForbidden(requestContext *gin.Context) {
	// 1. 构造固定公开错误，关联本次请求 ID。
	response := dto.ErrorResponse{
		Error:     dto.ErrorDetail{Code: "CLIENT_SOURCE_FORBIDDEN", Message: "客户端类型或请求来源不被允许", Details: nil},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	}
	// 2. 禁止缓存，返回 403 并停止后续处理。
	requestContext.Header("Cache-Control", "no-store")
	requestContext.AbortWithStatusJSON(http.StatusForbidden, response)
}
