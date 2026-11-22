package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// readCreateSyncSnapshotRequest 读取创建参数，失败时交付统一错误。
func readCreateSyncSnapshotRequest(requestContext *gin.Context) (int, string, bool) {
	// 1. 使用共享规则读取并校验唯一的幂等键。
	key, fieldError := dto.ParseIdempotencyKey(requestContext.Request)
	if fieldError != nil {
		InvalidArgument(requestContext, fieldError)
		return 0, "", false
	}

	// 2. 绑定单个 JSON 对象，拒绝未知或重复字段。
	var request dto.CreateSyncSnapshotRequest
	if !binding.ReadJSON(requestContext, &request) {
		return 0, "", false
	}

	// 3. 省略 page_size 时取 50，显式值须为 1～100 的整数。
	pageSize, fieldError := dto.ParseCreateSyncSnapshotPageSize(request)
	if fieldError != nil {
		InvalidArgument(requestContext, fieldError)
		return 0, "", false
	}
	return pageSize, key, true
}
