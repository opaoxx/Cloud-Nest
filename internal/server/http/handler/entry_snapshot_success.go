package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// writeEntrySnapshotSuccess 写出回收、恢复和预检的快照响应，正文由调用方编码。
func writeEntrySnapshotSuccess(requestContext *gin.Context, prepareResponse func(string) ([]byte, error)) {
	// 1. 检查响应是否可写以及请求是否取消，设置禁止缓存头。
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

	// 2. 用当次请求 ID 准备完整正文，编码异常时返回统一错误。
	responseBytes, err := prepareResponse(requestcontext.RequestID(ctx))
	if err != nil {
		InternalError(requestContext, nil)
		return
	}

	// 3. 写出前再次检查取消，清理旧正文头并返回 HTTP 200。
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
