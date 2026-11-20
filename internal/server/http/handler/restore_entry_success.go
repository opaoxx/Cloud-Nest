package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// RestoreEntrySuccess 交付已确认提交的稳定恢复快照。
// 调用方负责认证、事务和复用条件检查，请求 ID 由公共中间件提供。
func RestoreEntrySuccess(requestContext *gin.Context, stableBody []byte) {
	// 2. 解码恢复快照，加入当次 request_id，提前编码 JSON。
	writeEntrySnapshotSuccess(requestContext, http.StatusOK, func(requestID string) ([]byte, error) {
		data, err := dto.DecodeRestoreEntryData(stableBody)
		if err != nil {
			return nil, err
		}
		return json.Marshal(dto.RestoreEntryResponse{Data: *data, RequestID: requestID})
	})
}
