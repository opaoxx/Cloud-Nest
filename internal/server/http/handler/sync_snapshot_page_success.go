package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// SyncSnapshotPageSuccess 交付续页服务已准备好的页面，使用公共中间件生成的请求 ID。
func SyncSnapshotPageSuccess(requestContext *gin.Context, result *domain.SyncSnapshotPageResult) {
	writeSyncSnapshotPageSuccess(requestContext, http.StatusOK, result)
}

// CreateSyncSnapshotSuccess 以 HTTP 201 交付首次创建或同键重试取得的原首页。
func CreateSyncSnapshotSuccess(requestContext *gin.Context, result *domain.SyncSnapshotPageResult) {
	writeSyncSnapshotPageSuccess(requestContext, http.StatusCreated, result)
}

func writeSyncSnapshotPageSuccess(requestContext *gin.Context, successStatus int, result *domain.SyncSnapshotPageResult) {
	// 1. 复用快照响应写出逻辑，检查响应状态并设置禁止缓存。
	writeEntrySnapshotSuccess(requestContext, successStatus, func(requestID string) ([]byte, error) {
		// 2. 转换公开页面，附上当次请求 ID。
		data, err := dto.NewSyncSnapshotPageData(result)
		if err != nil {
			return nil, err
		}
		response := dto.SyncSnapshotPageResponse{Data: *data, RequestID: requestID}

		// 3. 预先编码完整正文，由公共方法复查请求状态并写出指定状态码。
		return json.Marshal(response)
	})
}
