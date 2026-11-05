package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// CORSForbidden 返回固定的跨域拒绝响应，不回显原始来源或请求头。
func CORSForbidden(requestContext *gin.Context) {
	// 1. 构造公开的 CORS_FORBIDDEN 错误，并关联本次请求 ID。
	response := dto.ErrorResponse{
		Error: dto.ErrorDetail{
			Code:    "CORS_FORBIDDEN",
			Message: "请求来源或跨域预检不被允许",
			Details: nil,
		},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	}
	// 2. 禁止缓存，返回 403 并停止后续处理。
	requestContext.Header("Cache-Control", "no-store")
	requestContext.AbortWithStatusJSON(http.StatusForbidden, response)
}
