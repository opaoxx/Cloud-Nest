package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// InvalidArgument 将固定字段校验错误转换为统一 400，不回显实际输入。
func InvalidArgument(requestContext *gin.Context, validationError *dto.FieldError) {
	// 1. 使用公开提示、字段名及本次请求 ID 构造统一错误。
	if validationError == nil {
		panic("InvalidArgument 必须传入字段校验错误")
	}
	response := dto.ErrorResponse{
		Error: dto.ErrorDetail{
			Code:    "INVALID_ARGUMENT",
			Message: validationError.Message,
			Details: map[string]any{"field": validationError.Field},
		},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	}
	// 2. 返回禁止缓存的 400，并停止后续处理。
	requestContext.Header("Cache-Control", "no-store")
	requestContext.AbortWithStatusJSON(http.StatusBadRequest, response)
}
