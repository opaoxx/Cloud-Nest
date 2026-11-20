package handler

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// TrashEntry 绑定幂等回收服务，路由须先挂载认证中间件。
func TrashEntry(trash func(context.Context, domain.Actor, uint64, uint64, string) (json.RawMessage, error)) gin.HandlerFunc {
	// 1. 检查回收服务依赖。
	if trash == nil {
		panic("目录项回收处理函数需要幂等服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查请求状态并读取已认证身份。
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

		// 2. 解析路径、幂等键和受限 JSON 正文。
		path, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}
		key, fieldError := dto.ParseIdempotencyKey(requestContext.Request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}
		var request dto.TrashEntryRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseTrashEntryRequest(request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用幂等回收服务，映射错误或交付成功快照。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := trash(ctx, actor, path.EntryID, input.ExpectedRevision, key)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writeTrashEntryFailure(requestContext, err)
			return
		}
		data, err := dto.RestoreTrashEntryData(result)
		if err != nil || input.ExpectedRevision == math.MaxUint64 ||
			data.Entry.ID != strconv.FormatUint(path.EntryID, 10) ||
			data.Entry.Revision != strconv.FormatUint(input.ExpectedRevision+1, 10) {
			InternalError(requestContext, nil)
			return
		}
		TrashEntrySuccess(requestContext, result)
	}
}

func writeTrashEntryFailure(requestContext *gin.Context, err error) {
	// 1. 按已知业务错误选择状态和固定提示，认证失败沿用共享响应。
	status := http.StatusConflict
	var detail dto.ErrorDetail
	switch {
	case errors.Is(err, domain.ErrRootEntryTrash):
		status = http.StatusForbidden
		detail = dto.ErrorDetail{Code: "ROOT_OPERATION_FORBIDDEN", Message: domain.ErrRootEntryTrash.Error()}
	case errors.Is(err, domain.ErrFileEntryNotFound):
		status = http.StatusNotFound
		detail = dto.ErrorDetail{Code: "ENTRY_NOT_FOUND", Message: domain.ErrFileEntryNotFound.Error()}
	case errors.Is(err, domain.ErrEntryRevisionConflict):
		var conflict *domain.EntryRevisionConflictError
		if !errors.As(err, &conflict) || conflict == nil || conflict.CurrentRevision == 0 {
			InternalError(requestContext, nil)
			return
		}
		detail = dto.ErrorDetail{Code: "REVISION_CONFLICT", Message: domain.ErrEntryRevisionConflict.Error(),
			Details: map[string]any{"current_revision": strconv.FormatUint(conflict.CurrentRevision, 10)}}
	case errors.Is(err, domain.ErrIdempotencyRequestConflict):
		detail = dto.ErrorDetail{Code: "IDEMPOTENCY_CONFLICT", Message: domain.ErrIdempotencyRequestConflict.Error()}
	case errors.Is(err, domain.ErrIdempotencyRecordExpired):
		detail = dto.ErrorDetail{Code: "IDEMPOTENCY_EXPIRED", Message: domain.ErrIdempotencyRecordExpired.Error()}
	default:
		AuthorizationFailure(requestContext, err)
		return
	}

	// 2. 关联本次请求 ID，只交付明确允许的错误信息。
	requestContext.AbortWithStatusJSON(status, dto.ErrorResponse{
		Error: detail, RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
