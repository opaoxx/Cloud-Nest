package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// CreateSyncSnapshot 绑定快照创建服务，路由须先挂载共享认证中间件。
func CreateSyncSnapshot(create func(context.Context, domain.Actor, string, int) (*domain.SyncSnapshotPageResult, error)) gin.HandlerFunc {
	if create == nil {
		panic("创建同步快照处理函数需要创建方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 读取已认证身份，解析幂等键和页大小。
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
		pageSize, key, ok := readCreateSyncSnapshotRequest(requestContext)
		if !ok {
			return
		}

		// 2. 调用创建服务，按接口契约处理错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := create(ctx, actor, key, pageSize)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writeCreateSyncSnapshotFailure(requestContext, err)
			return
		}

		// 3. 核对首页大小，交付 HTTP 201。
		if result == nil || len(result.Items) > pageSize || (result.NextCursor != nil && len(result.Items) != pageSize) {
			InternalError(requestContext, nil)
			return
		}
		CreateSyncSnapshotSuccess(requestContext, result)
	}
}

func writeCreateSyncSnapshotFailure(requestContext *gin.Context, err error) {
	// 1. 匹配快照创建的公开业务错误，其余沿用共享认证与内部错误处理。
	var status int
	var detail dto.ErrorDetail
	switch {
	case errors.Is(err, domain.ErrSyncSnapshotExpired):
		writeSyncSnapshotPageFailure(requestContext, err)
		return
	case errors.Is(err, domain.ErrSyncSnapshotTooLarge):
		status = http.StatusUnprocessableEntity
		detail = dto.ErrorDetail{Code: "SNAPSHOT_TOO_LARGE", Message: domain.ErrSyncSnapshotTooLarge.Error()}
	case errors.Is(err, domain.ErrSyncSnapshotBusy):
		status = http.StatusServiceUnavailable
		detail = dto.ErrorDetail{Code: "SNAPSHOT_BUSY", Message: domain.ErrSyncSnapshotBusy.Error()}
	case errors.Is(err, domain.ErrIdempotencyRequestConflict):
		status = http.StatusConflict
		detail = dto.ErrorDetail{Code: "IDEMPOTENCY_CONFLICT", Message: domain.ErrIdempotencyRequestConflict.Error()}
	case errors.Is(err, domain.ErrIdempotencyRecordExpired):
		status = http.StatusConflict
		detail = dto.ErrorDetail{Code: "IDEMPOTENCY_EXPIRED", Message: domain.ErrIdempotencyRecordExpired.Error()}
	default:
		AuthorizationFailure(requestContext, err)
		return
	}

	// 2. 只写出固定提示和本次请求 ID。
	requestContext.AbortWithStatusJSON(status, dto.ErrorResponse{
		Error: detail, RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
