package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// ConfirmSyncSnapshotBaselineSuccess 交付服务已确认提交的设备位置。
func ConfirmSyncSnapshotBaselineSuccess(requestContext *gin.Context, ackSequence uint64) {
	writeEntrySnapshotSuccess(requestContext, http.StatusOK, func(requestID string) ([]byte, error) {
		// 1. 用确认序号和当次请求 ID 组装公开响应。
		response := dto.ConfirmSyncSnapshotBaselineResponse{
			Data:      dto.NewConfirmSyncSnapshotBaselineData(ackSequence),
			RequestID: requestID,
		}

		// 2. 预先编码完整 JSON，由共用方法按 200 OK 写出。
		return json.Marshal(response)
	})
}
