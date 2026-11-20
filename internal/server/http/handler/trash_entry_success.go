package handler

import (
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// TrashEntrySuccess 交付已确认提交的稳定回收快照。
// 调用方负责认证、事务和复用条件检查，请求 ID 由公共中间件提供。
func TrashEntrySuccess(requestContext *gin.Context, stableBody []byte) {
	// 2. 恢复快照，关联本次请求 ID 并预先编码。
	writeEntrySnapshotSuccess(requestContext, func(requestID string) ([]byte, error) {
		data, err := dto.RestoreTrashEntryData(stableBody)
		if err != nil {
			return nil, err
		}
		return json.Marshal(dto.TrashEntryResponse{Data: *data, RequestID: requestID})
	})
}
