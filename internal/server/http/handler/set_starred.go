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

// SetStarred 绑定收藏核心服务，路由须先挂载认证中间件。
// setStarred 绑定 SetStarredService.Set，身份来自 Actor，目标来自路径 id。
func SetStarred(setStarred func(context.Context, domain.Actor, uint64, bool) (*domain.EntryPreference, error)) gin.HandlerFunc {
	// 1. 检查服务依赖，构造每次请求的处理函数。
	if setStarred == nil {
		panic("收藏处理函数需要收藏服务方法")
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

		// 3. 读取 JSON 正文并校验明确的收藏状态。
		var request dto.SetStarredRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseSetStarredRequest(request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 4. 调用收藏服务，处理取消、可见性和认证错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := setStarred(ctx, actor, path.EntryID, input.Starred)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if errors.Is(err, domain.ErrFileEntryNotFound) {
			response := dto.ErrorResponse{
				Error:     dto.ErrorDetail{Code: "ENTRY_NOT_FOUND", Message: domain.ErrFileEntryNotFound.Error(), Details: nil},
				RequestID: requestcontext.RequestID(ctx),
			}
			requestContext.AbortWithStatusJSON(http.StatusNotFound, response)
			return
		}
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}

		// 5. 核对偏好归属、目标和状态，交付成功响应。
		if result == nil || result.UserID != actor.UserID || result.EntryID != path.EntryID || (result.StarredAt != nil) != input.Starred {
			InternalError(requestContext, nil)
			return
		}
		SetStarredSuccess(requestContext, result)
	}
}
