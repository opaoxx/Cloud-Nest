package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// readConfirmSyncChangesRequest 读取合法的游标文本，校验失败时结束请求。
func readConfirmSyncChangesRequest(requestContext *gin.Context) (string, bool) {
	// 1. 复用受限 JSON 绑定，交付非法正文的统一错误。
	var request dto.ConfirmSyncChangesRequest
	if !binding.ReadJSON(requestContext, &request) {
		return "", false
	}

	// 2. 校验游标字段；通过时原样交给之后的验签。
	cursor, valid := request.ParseCursor()
	if !valid {
		invalidSyncChangeCursor(requestContext)
		return "", false
	}
	return cursor, true
}

func invalidSyncChangeCursor(requestContext *gin.Context) {
	requestContext.Header("Cache-Control", "no-store")
	requestContext.AbortWithStatusJSON(http.StatusBadRequest, dto.ErrorResponse{
		Error: dto.ErrorDetail{
			Code:    "INVALID_CURSOR",
			Message: domain.ErrInvalidSyncChangeCursor.Error(),
			Details: map[string]any{"field": "cursor"},
		},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
