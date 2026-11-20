package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// PurgeEntryAccepted 交付已提交的永久清理受理快照。
// 调用方负责认证与受理，请求 ID 由公共中间件提供。
func PurgeEntryAccepted(requestContext *gin.Context, stableBody []byte) {
	// 1. 复用公共写出方法，检查请求状态并设置禁止缓存头。
	writeEntrySnapshotSuccess(requestContext, http.StatusAccepted, func(requestID string) ([]byte, error) {
		// 2. 解码并校验受理快照，加入本次请求 ID。
		data, err := dto.DecodePurgeEntryAcceptedData(stableBody)
		if err != nil {
			return nil, err
		}
		response := dto.PurgeEntryAcceptedResponse{Data: *data, RequestID: requestID}

		// 3. 编码完整响应，交由公共方法写出 HTTP 202。
		return json.Marshal(response)
	})
}
