package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// RecordAccess 绑定访问核心服务，路由须先挂载认证中间件。
// recordAccess 绑定 RecordAccessService.Record，身份来自 Actor，目标来自路径 id。
func RecordAccess(recordAccess func(context.Context, domain.Actor, uint64, string) (*domain.EntryPreference, error)) gin.HandlerFunc {
	// 1. 检查服务依赖，构造每次请求的处理函数。
	if recordAccess == nil {
		panic("访问记录处理函数需要访问服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查响应状态、请求取消和已认证身份。
		if requestContext.Writer.Written() {
			requestContext.Abort()
			return
		}
		requestContext.Header("Cache-Control", "no-store")
		requestContext.Header("Pragma", "no-cache")
		requestContext.Writer.Header().Del("Content-Length")
		requestContext.Writer.Header().Del("Content-Encoding")
		requestContext.Header("Content-Type", "application/json; charset=utf-8")
		ctx := requestContext.Request.Context()
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		actor, exists := requestcontext.Actor(ctx)
		if !exists {
			InternalError(requestContext, nil)
			return
		}

		// 2. 解析路径中的目录项 ID。
		path, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 读取 JSON 正文并校验访问用途。
		var request dto.RecordAccessRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseRecordAccessRequest(request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 4. 调用访问服务，处理取消、目标、用途及认证错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := recordAccess(ctx, actor, path.EntryID, input.Purpose)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		switch {
		case errors.Is(err, domain.ErrFileEntryNotFile):
			InvalidArgument(requestContext, &dto.FieldError{Field: "id", Message: domain.ErrFileEntryNotFile.Error()})
			return
		case errors.Is(err, domain.ErrInvalidEntryAccessPurpose):
			InvalidArgument(requestContext, &dto.FieldError{Field: "purpose", Message: domain.ErrInvalidEntryAccessPurpose.Error()})
			return
		case errors.Is(err, domain.ErrFileEntryNotFound):
			response := dto.ErrorResponse{
				Error:     dto.ErrorDetail{Code: "ENTRY_NOT_FOUND", Message: domain.ErrFileEntryNotFound.Error(), Details: nil},
				RequestID: requestcontext.RequestID(ctx),
			}
			requestContext.AbortWithStatusJSON(http.StatusNotFound, response)
			return
		case err != nil:
			AuthorizationFailure(requestContext, err)
			return
		}

		// 5. 核对偏好归属、目标和访问时间，交付成功响应。
		if result == nil || result.UserID != actor.UserID || result.EntryID != path.EntryID || result.LastOpenedAt == nil {
			InternalError(requestContext, nil)
			return
		}
		RecordAccessSuccess(requestContext, result)
	}
}
