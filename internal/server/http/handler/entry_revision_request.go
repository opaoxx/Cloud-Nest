package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

type entryRevisionRequestScope struct {
	actor   domain.Actor
	entryID uint64
	key     string
}

// readEntryRevisionRequestScope 读取回收和恢复共用的认证身份、目标和幂等键。
func readEntryRevisionRequestScope(requestContext *gin.Context) (entryRevisionRequestScope, bool) {
	// 1. 检查请求状态和认证身份，准备统一的响应头。
	if requestContext.Writer.Written() || requestContext.IsAborted() {
		requestContext.Abort()
		return entryRevisionRequestScope{}, false
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
		return entryRevisionRequestScope{}, false
	}

	// 2. 依次检查路径和幂等头，交给调用方解析专属正文。
	path, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
	if fieldError != nil {
		InvalidArgument(requestContext, fieldError)
		return entryRevisionRequestScope{}, false
	}
	key, fieldError := dto.ParseIdempotencyKey(requestContext.Request)
	if fieldError != nil {
		InvalidArgument(requestContext, fieldError)
		return entryRevisionRequestScope{}, false
	}
	return entryRevisionRequestScope{actor: actor, entryID: path.EntryID, key: key}, true
}

// writeEntryRevisionFailure 交付回收和恢复共用的失败响应。
func writeEntryRevisionFailure(requestContext *gin.Context, err, rootError error) {
	// 1. 按已知业务错误选择状态和固定提示，认证失败沿用共享响应。
	status := http.StatusConflict
	var detail dto.ErrorDetail
	switch {
	case errors.Is(err, rootError):
		status = http.StatusForbidden
		detail = dto.ErrorDetail{Code: "ROOT_OPERATION_FORBIDDEN", Message: rootError.Error()}
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
