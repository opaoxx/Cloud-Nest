package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// SyncSnapshotPage 绑定快照续页读取与组装，路由须先挂载共享认证中间件。
func SyncSnapshotPage(readPage func(context.Context, domain.Actor, uuid.UUID, string) (*domain.SyncSnapshotPageResult, error)) gin.HandlerFunc {
	if readPage == nil {
		panic("同步快照续页处理函数需要分页读取方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 读取已认证身份，解析快照 ID 和续页凭据。
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
		input, fieldError := dto.ParseSyncSnapshotPageRequest(requestContext.Param("snapshot_id"), requestContext.Request.URL.RawQuery)
		if fieldError != nil {
			if fieldError.Field == "cursor" {
				writeSyncSnapshotPageFailure(requestContext, domain.ErrInvalidSyncSnapshotCursor)
				return
			}
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 2. 调用续页读取与分页组装，按契约映射错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := readPage(ctx, actor, input.SnapshotID, input.Cursor)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writeSyncSnapshotPageFailure(requestContext, err)
			return
		}

		// 3. 核对结果属于所请求的快照，再交付 HTTP 200。
		if result == nil || result.SnapshotID != input.SnapshotID {
			InternalError(requestContext, nil)
			return
		}
		SyncSnapshotPageSuccess(requestContext, result)
	}
}

func writeSyncSnapshotPageFailure(requestContext *gin.Context, err error) {
	// 1. 区分凭据无效和可信快照失效，其余沿用共享错误映射。
	var status int
	var detail dto.ErrorDetail
	switch {
	case errors.Is(err, domain.ErrInvalidSyncSnapshotCursor):
		status = http.StatusBadRequest
		detail = dto.ErrorDetail{
			Code: "INVALID_CURSOR", Message: domain.ErrInvalidSyncSnapshotCursor.Error(),
			Details: map[string]any{"field": "cursor"},
		}
	case errors.Is(err, domain.ErrSyncSnapshotExpired):
		status = http.StatusGone
		detail = dto.ErrorDetail{Code: "CURSOR_EXPIRED", Message: domain.ErrSyncSnapshotExpired.Error()}
	default:
		AuthorizationFailure(requestContext, err)
		return
	}

	// 2. 使用固定提示和当次请求 ID，结束请求处理。
	requestContext.AbortWithStatusJSON(status, dto.ErrorResponse{
		Error: detail, RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
