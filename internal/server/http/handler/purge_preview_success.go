package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// PurgePreviewSuccess 交付预检服务生成的范围与短期凭证。
// 调用方完成认证与预检，请求 ID 由公共中间件提供。
func PurgePreviewSuccess(requestContext *gin.Context, result *domain.PurgePreview) {
	// 1. 复用公共写出逻辑，检查请求状态并设置禁止缓存头。
	writeEntrySnapshotSuccess(requestContext, http.StatusOK, func(requestID string) ([]byte, error) {
		// 2. 转换预检数据，加入当次请求 ID 并编码完整 JSON。
		data, err := dto.NewPurgePreviewData(result)
		if err != nil {
			return nil, err
		}
		response := dto.PurgePreviewResponse{Data: *data, RequestID: requestID}

		// 3. 把编码结果交给公共方法复查请求状态并写出 HTTP 200。
		return json.Marshal(response)
	})
}
