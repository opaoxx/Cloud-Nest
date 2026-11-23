package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// readPullSyncChangesRequest 读取增量拉取参数，校验失败时结束请求。
func readPullSyncChangesRequest(requestContext *gin.Context) (*dto.PullSyncChangesInput, bool) {
	// 1. 使用共享的受限 JSON 绑定，检查正文结构和凭据组合。
	var request dto.PullSyncChangesRequest
	if !binding.ReadJSON(requestContext, &request) {
		return nil, false
	}

	// 2. 解析页大小与凭据，成功时交给后续服务验签。
	input, fieldError := dto.ParsePullSyncChangesInput(request)
	if fieldError == nil {
		return input, true
	}

	// 3. 凭据字段错误使用 INVALID_CURSOR，页大小错误沿用 INVALID_ARGUMENT。
	if fieldError.Field == "baseline_token" || fieldError.Field == "cursor" {
		requestContext.Header("Cache-Control", "no-store")
		requestContext.AbortWithStatusJSON(http.StatusBadRequest, dto.ErrorResponse{
			Error: dto.ErrorDetail{
				Code:    "INVALID_CURSOR",
				Message: fieldError.Message,
				Details: map[string]any{"field": fieldError.Field},
			},
			RequestID: requestcontext.RequestID(requestContext.Request.Context()),
		})
		return nil, false
	}
	InvalidArgument(requestContext, fieldError)
	return nil, false
}
