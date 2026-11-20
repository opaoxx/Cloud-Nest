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

// PurgePreview 绑定预检服务，路由须先挂载共享认证中间件。
func PurgePreview(preview func(context.Context, domain.Actor, uint64) (*domain.PurgePreview, error)) gin.HandlerFunc {
	// 1. 检查预检服务依赖。
	if preview == nil {
		panic("永久清理预检处理函数需要预检服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查请求状态，读取已认证身份。
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

		// 2. 解析路径中的目标 ID。
		path, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用预检服务，将业务错误转换成固定 HTTP 响应。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := preview(ctx, actor, path.EntryID)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writePurgePreviewFailure(requestContext, err)
			return
		}

		// 4. 核对返回的目标 ID，再交付成功响应。
		if result == nil || result.EntryID != path.EntryID {
			InternalError(requestContext, nil)
			return
		}
		PurgePreviewSuccess(requestContext, result)
	}
}

func writePurgePreviewFailure(requestContext *gin.Context, err error) {
	// 1. 将预检业务错误映射为固定提示，其他错误沿用共享处理。
	var status int
	var detail dto.ErrorDetail
	switch {
	case errors.Is(err, domain.ErrFileEntrySubtreeTooLarge):
		InvalidArgument(requestContext, &dto.FieldError{Field: "id", Message: domain.ErrFileEntrySubtreeTooLarge.Error()})
		return
	case errors.Is(err, domain.ErrRootEntryPurge):
		status = http.StatusForbidden
		detail = dto.ErrorDetail{Code: "ROOT_OPERATION_FORBIDDEN", Message: domain.ErrRootEntryPurge.Error()}
	case errors.Is(err, domain.ErrFileEntryNotFound):
		status = http.StatusNotFound
		detail = dto.ErrorDetail{Code: "ENTRY_NOT_FOUND", Message: domain.ErrFileEntryNotFound.Error()}
	default:
		AuthorizationFailure(requestContext, err)
		return
	}

	// 2. 关联当前请求 ID，只交付固定的公开错误。
	requestContext.AbortWithStatusJSON(status, dto.ErrorResponse{
		Error: detail, RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
