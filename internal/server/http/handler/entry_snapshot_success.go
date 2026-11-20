package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// writeEntrySnapshotSuccess 用调用方提供的状态码和编码方法写出快照响应。
func writeEntrySnapshotSuccess(requestContext *gin.Context, successStatus int, prepareResponse func(string) ([]byte, error)) {
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

	// 3. 写出前再次检查取消，清理旧正文头并按指定状态码返回。
	if ctx.Err() != nil {
		InternalError(requestContext, nil)
		return
	}
	requestContext.Writer.Header().Del("Content-Length")
	requestContext.Writer.Header().Del("Content-Encoding")
	requestContext.Header("Content-Type", "application/json; charset=utf-8")
	requestContext.Abort()
	requestContext.Data(successStatus, "application/json; charset=utf-8", responseBytes)
}
