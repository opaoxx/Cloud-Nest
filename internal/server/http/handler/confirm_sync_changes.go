package handler

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// ConfirmSyncChanges 绑定增量确认服务，路由须先挂载共享认证中间件。
func ConfirmSyncChanges(confirm func(context.Context, domain.Actor, string) (uint64, error)) gin.HandlerFunc {
	if confirm == nil {
		panic("设备增量确认处理函数需要确认方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 取得认证身份并解析确认正文。
		if requestContext.Writer.Written() || requestContext.IsAborted() {
			requestContext.Abort()
			return
		}
		requestContext.Writer.Header().Del("Content-Length")
		requestContext.Writer.Header().Del("Content-Encoding")
		requestContext.Header("Content-Type", "application/json; charset=utf-8")
		requestContext.Header("Cache-Control", "no-store")
		requestContext.Header("Pragma", "no-cache")
		ctx := requestContext.Request.Context()
		actor, exists := requestcontext.Actor(ctx)
		if ctx.Err() != nil || !exists {
			InternalError(requestContext, nil)
			return
		}
		cursor, ok := readConfirmSyncChangesRequest(requestContext)
		if !ok {
			return
		}

		// 2. 调用确认服务，将失败映射为公开错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		position, err := confirm(ctx, actor, cursor)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			switch {
			case errors.Is(err, domain.ErrInvalidSyncChangeCursor):
				invalidSyncChangeCursor(requestContext)
			case errors.Is(err, domain.ErrSyncSnapshotExpired):
				writeSyncSnapshotPageFailure(requestContext, err)
			default:
				AuthorizationFailure(requestContext, err)
			}
			return
		}

		// 3. 写出已提交的确认位置和请求 ID。
		writeSyncConfirmationSuccess(requestContext, position)
	}
}
