package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// readConfirmSyncSnapshotBaselineRequest 读取快照 ID 和末页凭据，失败时结束请求。
func readConfirmSyncSnapshotBaselineRequest(requestContext *gin.Context) (uuid.UUID, string, bool) {
	// 1. 与快照续页共用路径 ID 规则。
	snapshotID, fieldError := dto.ParseSyncSnapshotID(requestContext.Param("snapshot_id"))
	if fieldError != nil {
		InvalidArgument(requestContext, fieldError)
		return uuid.Nil, "", false
	}

	// 2. 复用受限 JSON 绑定，拒绝含重复键或额外字段的正文。
	var request dto.ConfirmSyncSnapshotBaselineRequest
	if !binding.ReadJSON(requestContext, &request) {
		return uuid.Nil, "", false
	}

	// 3. 检查末页凭据文本，成功时保留解码原文交给验签。
	baselineToken, valid := request.ParseBaselineToken()
	if !valid {
		invalidSyncSnapshotBaselineToken(requestContext)
		return uuid.Nil, "", false
	}
	return snapshotID, baselineToken, true
}

func invalidSyncSnapshotBaselineToken(requestContext *gin.Context) {
	requestContext.Header("Cache-Control", "no-store")
	requestContext.AbortWithStatusJSON(http.StatusBadRequest, dto.ErrorResponse{
		Error: dto.ErrorDetail{
			Code:    "INVALID_CURSOR",
			Message: domain.ErrInvalidSyncSnapshotBaselineToken.Error(),
			Details: map[string]any{"field": "baseline_token"},
		},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
