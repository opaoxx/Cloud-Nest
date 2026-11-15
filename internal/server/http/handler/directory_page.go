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

// DirectoryPage 绑定目录分页服务，路由须先挂载认证中间件。
// listPage 绑定 DirectoryPageService.List，输入来自认证 Actor 与查询 DTO。
func DirectoryPage(listPage func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error)) gin.HandlerFunc {
	// 1. 检查分页服务依赖，构造每次请求的处理函数。
	if listPage == nil {
		panic("目录页处理函数需要目录分页服务方法")
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

		// 2. 解析完整目录查询参数。
		input, fieldError := dto.ParseDirectoryPageQuery(requestContext.Request.URL.RawQuery)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用目录分页服务，处理取消及业务错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := listPage(ctx, actor, input.ParentID, input.Cursor, input.PageSize)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			response := dto.ErrorResponse{RequestID: requestcontext.RequestID(ctx)}
			status := http.StatusBadRequest
			switch {
			case errors.Is(err, domain.ErrInvalidDirectoryCursor):
				response.Error = dto.ErrorDetail{
					Code: "INVALID_CURSOR", Message: "目录分页游标无效，请重新查询",
					Details: map[string]any{"field": "cursor"},
				}
			case errors.Is(err, domain.ErrFileEntryNotFound):
				status = http.StatusNotFound
				response.Error = dto.ErrorDetail{Code: "ENTRY_NOT_FOUND", Message: "目录项不存在", Details: nil}
			case errors.Is(err, domain.ErrFileEntryNotDirectory):
				InvalidArgument(requestContext, &dto.FieldError{Field: "parent_id", Message: "父目录须为文件夹"})
				return
			default:
				AuthorizationFailure(requestContext, err)
				return
			}
			requestContext.AbortWithStatusJSON(status, response)
			return
		}

		// 4. 核对当前目录及用户，交付目录页响应。
		if result == nil || len(result.Path.Entries) == 0 {
			InternalError(requestContext, nil)
			return
		}
		parent := result.Path.Entries[len(result.Path.Entries)-1]
		if parent.ID != input.ParentID || parent.UserID != actor.UserID {
			InternalError(requestContext, nil)
			return
		}
		DirectoryPageSuccess(requestContext, result)
	}
}
