package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// PullSyncChanges 绑定增量拉取服务，路由须先挂载共享认证中间件。
func PullSyncChanges(pull func(context.Context, domain.Actor, string, string, int) (*domain.SyncChangePullResult, error)) gin.HandlerFunc {
	if pull == nil {
		panic("增量拉取处理函数需要拉取方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 取得认证身份并解析受限 JSON 正文。
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
		input, ok := readPullSyncChangesRequest(requestContext)
		if !ok {
			return
		}

		// 2. 调用拉取服务，把业务错误映射为公开响应。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := pull(ctx, actor, input.BaselineToken, input.Cursor, input.PageSize)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writePullSyncChangesFailure(requestContext, err)
			return
		}

		// 3. 完整编码成功响应，再写出 HTTP 200。
		writeEntrySnapshotSuccess(requestContext, http.StatusOK, func(requestID string) ([]byte, error) {
			data, err := dto.NewPullSyncChangesData(result)
			if err != nil {
				return nil, err
			}
			return json.Marshal(dto.PullSyncChangesResponse{Data: *data, RequestID: requestID})
		})
	}
}

func writePullSyncChangesFailure(requestContext *gin.Context, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken):
		invalidSyncSnapshotBaselineToken(requestContext)
	case errors.Is(err, domain.ErrInvalidSyncChangeCursor):
		invalidSyncChangeCursor(requestContext)
	case errors.Is(err, domain.ErrSyncSnapshotExpired):
		writeSyncSnapshotPageFailure(requestContext, err)
	default:
		AuthorizationFailure(requestContext, err)
	}
}
