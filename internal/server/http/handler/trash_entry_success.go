package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// TrashEntrySuccess 交付已确认提交的稳定回收快照。
// 调用方负责认证、事务和复用条件检查，请求 ID 由公共中间件提供。
func TrashEntrySuccess(requestContext *gin.Context, stableBody []byte) {
	// 1. 检查响应状态和请求取消，设置禁止缓存头。
	if requestContext.Writer.Written() || requestContext.IsAborted() {
		requestContext.Abort()
		return
	}
	requestContext.Header("Cache-Control", "no-store")
	requestContext.Header("Pragma", "no-cache")
	ctx := requestContext.Request.Context()
	if ctx.Err() != nil {
		InternalError(requestContext, nil)
		return
	}

	// 2. 恢复快照，关联本次请求 ID 并预先编码。
	data, err := dto.RestoreTrashEntryData(stableBody)
	if err != nil {
		InternalError(requestContext, nil)
		return
	}
	responseBytes, err := json.Marshal(dto.TrashEntryResponse{Data: *data, RequestID: requestcontext.RequestID(ctx)})
	if err != nil {
		InternalError(requestContext, nil)
		return
	}

	// 3. 写出前复查取消，清理旧正文头并交付 HTTP 200。
	if ctx.Err() != nil {
		InternalError(requestContext, nil)
		return
	}
	requestContext.Writer.Header().Del("Content-Length")
	requestContext.Writer.Header().Del("Content-Encoding")
	requestContext.Header("Content-Type", "application/json; charset=utf-8")
	requestContext.Abort()
	requestContext.Data(http.StatusOK, "application/json; charset=utf-8", responseBytes)
}
