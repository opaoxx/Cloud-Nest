package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// PurgeEntryAccepted 交付与请求目标一致的已提交永久清理受理快照。
// 调用方负责认证与受理，请求 ID 由公共中间件提供。
func PurgeEntryAccepted(requestContext *gin.Context, entryID uint64, stableBody []byte) {
	// 1. 复用公共写出方法，检查请求状态并设置禁止缓存头。
	writeEntrySnapshotSuccess(requestContext, http.StatusAccepted, func(requestID string) ([]byte, error) {
		// 2. 解码受理快照，核对请求目标后加入本次请求 ID。
		data, err := dto.DecodePurgeEntryAcceptedData(stableBody)
		if err != nil {
			return nil, err
		}
		if data.EntryID != strconv.FormatUint(entryID, 10) {
			return nil, domain.ErrInvalidIdempotencyRecordData
		}
		response := dto.PurgeEntryAcceptedResponse{Data: *data, RequestID: requestID}

		// 3. 编码完整响应，交由公共方法写出 HTTP 202。
		return json.Marshal(response)
	})
}
