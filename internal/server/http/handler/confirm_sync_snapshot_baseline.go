package handler

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// ConfirmSyncSnapshotBaseline 绑定基线确认服务，路由须先挂载共享认证中间件。
func ConfirmSyncSnapshotBaseline(confirm func(context.Context, domain.Actor, uuid.UUID, string) (uint64, error)) gin.HandlerFunc {
	if confirm == nil {
		panic("同步快照基线确认处理函数需要确认方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 从已认证上下文取身份，并解析路径和末页凭据。
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
		snapshotID, token, ok := readConfirmSyncSnapshotBaselineRequest(requestContext)
		if !ok {
			return
		}

		// 2. 调用确认服务，按快照契约及共享授权规则处理错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		position, err := confirm(ctx, actor, snapshotID, token)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			switch {
			case errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken):
				invalidSyncSnapshotBaselineToken(requestContext)
			case errors.Is(err, domain.ErrSyncSnapshotExpired):
				writeSyncSnapshotPageFailure(requestContext, err)
			default:
				AuthorizationFailure(requestContext, err)
			}
			return
		}

		// 3. 写出服务已确认提交的位置，允许合法的零序号。
		ConfirmSyncSnapshotBaselineSuccess(requestContext, position)
	}
}
