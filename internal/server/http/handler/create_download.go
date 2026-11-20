package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// CreateDownload 绑定下载授权服务，路由须先挂载认证中间件。
// create 依次接收当前 Actor、目录项 ID、预期版本和用途。
func CreateDownload(create func(context.Context, domain.Actor, uint64, uint64, string) (*domain.EntryDownloadAuthorization, error)) gin.HandlerFunc {
	// 1. 检查授权服务依赖。
	if create == nil {
		panic("下载授权处理需要授权服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查请求状态，读取认证身份。
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

		// 2. 解析路径和受限 JSON，校验版本与用途。
		path, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}
		var request dto.CreateDownloadRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseCreateDownloadRequest(request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用下载授权服务，交付成功响应或映射后的错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := create(ctx, actor, path.EntryID, input.ExpectedRevision, input.Purpose)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writeCreateDownloadFailure(requestContext, err)
			return
		}
		if result == nil || result.Revision != input.ExpectedRevision {
			InternalError(requestContext, nil)
			return
		}
		CreateDownloadSuccess(requestContext, result)
	}
}

func writeCreateDownloadFailure(requestContext *gin.Context, err error) {
	// 1. 选择公开错误码和固定提示，复用字段与认证错误响应。
	var status int
	var detail dto.ErrorDetail
	switch {
	case errors.Is(err, domain.ErrFileEntryNotFile):
		InvalidArgument(requestContext, &dto.FieldError{Field: "id", Message: domain.ErrFileEntryNotFile.Error()})
		return
	case errors.Is(err, domain.ErrInvalidDownloadPurpose):
		InvalidArgument(requestContext, &dto.FieldError{Field: "purpose", Message: domain.ErrInvalidDownloadPurpose.Error()})
		return
	case errors.Is(err, domain.ErrFileEntryNotFound):
		status = http.StatusNotFound
		detail = dto.ErrorDetail{Code: "ENTRY_NOT_FOUND", Message: domain.ErrFileEntryNotFound.Error()}
	case errors.Is(err, domain.ErrEntryRevisionConflict):
		var conflict *domain.EntryRevisionConflictError
		if !errors.As(err, &conflict) || conflict == nil || conflict.CurrentRevision == 0 {
			InternalError(requestContext, nil)
			return
		}
		status = http.StatusConflict
		detail = dto.ErrorDetail{Code: "REVISION_CONFLICT", Message: domain.ErrEntryRevisionConflict.Error(),
			Details: map[string]any{"current_revision": strconv.FormatUint(conflict.CurrentRevision, 10)}}
	case errors.Is(err, domain.ErrDownloadTargetUnavailable):
		status = http.StatusConflict
		detail = dto.ErrorDetail{Code: "TARGET_UNAVAILABLE", Message: domain.ErrDownloadTargetUnavailable.Error()}
	case errors.Is(err, domain.ErrUnsupportedPreview):
		status = http.StatusUnprocessableEntity
		detail = dto.ErrorDetail{Code: "UNSUPPORTED_PREVIEW", Message: domain.ErrUnsupportedPreview.Error()}
	case errors.Is(err, domain.ErrDownloadStorageUnavailable):
		status = http.StatusServiceUnavailable
		detail = dto.ErrorDetail{Code: "STORAGE_UNAVAILABLE", Message: domain.ErrDownloadStorageUnavailable.Error()}
	case errors.Is(err, domain.ErrDownloadDatabaseUnavailable):
		status = http.StatusServiceUnavailable
		detail = dto.ErrorDetail{Code: "DATABASE_UNAVAILABLE", Message: domain.ErrDownloadDatabaseUnavailable.Error()}
	default:
		AuthorizationFailure(requestContext, err)
		return
	}

	// 2. 关联当前请求 ID，只返回固定的公开错误信息。
	requestContext.AbortWithStatusJSON(status, dto.ErrorResponse{
		Error: detail, RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
