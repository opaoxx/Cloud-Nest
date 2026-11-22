package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// ReuseFile 连接秒传 HTTP 请求和幂等服务，由认证中间件先提供可信身份。
func ReuseFile(reuse func(context.Context, domain.Actor, uint64, string, uint64, [sha256.Size]byte, string) (json.RawMessage, error)) gin.HandlerFunc {
	if reuse == nil {
		panic("秒传处理函数需要幂等服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查认证上下文和请求状态，设置统一响应头。
		if requestContext.Writer.Written() || requestContext.IsAborted() {
			requestContext.Abort()
			return
		}
		requestContext.Header("Cache-Control", "no-store")
		requestContext.Header("Pragma", "no-cache")
		requestContext.Writer.Header().Del("Content-Length")
		requestContext.Writer.Header().Del("Content-Encoding")
		requestContext.Header("Content-Type", "application/json; charset=utf-8")
		ctx := requestContext.Request.Context()
		actor, exists := requestcontext.Actor(ctx)
		if ctx.Err() != nil || !exists {
			InternalError(requestContext, nil)
			return
		}

		// 2. 绑定请求参数，调用幂等秒传服务。
		input, key := readReuseFileRequest(requestContext)
		if input == nil {
			return
		}
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		body, err := reuse(ctx, actor, input.ParentID, input.Name, input.SizeBytes, input.SHA256, key)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}

		// 3. 映射公开错误，或校验并交付 201 创建快照。
		if err != nil {
			writeReuseFileFailure(requestContext, err)
			return
		}
		data, err := dto.RestoreReuseFileData(body)
		if err != nil || data.Entry.ParentID != strconv.FormatUint(input.ParentID, 10) || data.Entry.SizeBytes != input.SizeBytes {
			InternalError(requestContext, nil)
			return
		}
		writeEntrySnapshotSuccess(requestContext, http.StatusCreated, func(requestID string) ([]byte, error) {
			return json.Marshal(dto.ReuseFileResponse{Data: *data, RequestID: requestID})
		})
	}
}

func writeReuseFileFailure(requestContext *gin.Context, err error) {
	// 1. 选择约定的状态、固定提示和允许公开的字段。
	status := http.StatusConflict
	var detail dto.ErrorDetail
	switch {
	case errors.Is(err, domain.ErrInvalidEntryName):
		status = http.StatusBadRequest
		detail = dto.ErrorDetail{Code: "INVALID_NAME", Message: domain.ErrInvalidEntryName.Error(),
			Details: map[string]any{"field": "name"}}
	case errors.Is(err, domain.ErrFileEntryNotDirectory):
		InvalidArgument(requestContext, &dto.FieldError{Field: "parent_id", Message: "父目录须为文件夹"})
		return
	case errors.Is(err, domain.ErrFileEntryDepthExceeded):
		InvalidArgument(requestContext, &dto.FieldError{Field: "parent_id", Message: domain.ErrFileEntryDepthExceeded.Error()})
		return
	case errors.Is(err, domain.ErrFileEntryNotFound):
		status = http.StatusNotFound
		detail = dto.ErrorDetail{Code: "ENTRY_NOT_FOUND", Message: domain.ErrFileEntryNotFound.Error()}
	case errors.Is(err, domain.ErrReusableContentNotFound):
		status = http.StatusNotFound
		detail = dto.ErrorDetail{Code: "REUSABLE_CONTENT_NOT_FOUND", Message: domain.ErrReusableContentNotFound.Error()}
	case errors.Is(err, domain.ErrIdempotencyRequestConflict):
		detail = dto.ErrorDetail{Code: "IDEMPOTENCY_CONFLICT", Message: domain.ErrIdempotencyRequestConflict.Error()}
	case errors.Is(err, domain.ErrIdempotencyRecordExpired):
		detail = dto.ErrorDetail{Code: "IDEMPOTENCY_EXPIRED", Message: domain.ErrIdempotencyRecordExpired.Error()}
	case errors.Is(err, domain.ErrQuotaExceeded):
		status = http.StatusInsufficientStorage
		detail = dto.ErrorDetail{Code: "QUOTA_EXCEEDED", Message: domain.ErrQuotaExceeded.Error()}
	default:
		AuthorizationFailure(requestContext, err)
		return
	}

	// 2. 附加当前请求标识，结束处理。
	requestContext.AbortWithStatusJSON(status, dto.ErrorResponse{
		Error: detail, RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
