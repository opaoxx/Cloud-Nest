package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// PurgeEntryProgressSuccess 交付只读快照中的永久清理进度。
// 请求 ID 由公共中间件提供，调用方负责认证和读取进度。
func PurgeEntryProgressSuccess(requestContext *gin.Context, result *domain.PurgeProgress) {
	// 1. 复用公共写出方法，检查请求并设置禁止缓存头。
	writeEntrySnapshotSuccess(requestContext, http.StatusOK, func(requestID string) ([]byte, error) {
		// 2. 转换进度数据，关联本次请求 ID。
		data, err := dto.NewPurgeEntryProgressData(result)
		if err != nil {
			return nil, err
		}
		response := dto.PurgeEntryProgressResponse{Data: *data, RequestID: requestID}

		// 3. 编码完整 JSON，交由公共方法写出 HTTP 200。
		return json.Marshal(response)
	})
}
