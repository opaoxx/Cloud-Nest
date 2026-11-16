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

// RecentEntryPage 绑定最近使用分页服务，路由须先挂载认证中间件。
// listPage 绑定 RecentEntryPageService.List，接收可信 Actor 及查询 DTO。
func RecentEntryPage(listPage func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.RecentEntryPageResult, error)) gin.HandlerFunc {
	// 1. 检查分页服务依赖，构造每次请求的处理函数。
	if listPage == nil {
		panic("最近使用页处理函数需要最近使用分页服务方法")
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

		// 2. 解析完整最近使用查询参数。
		input, fieldError := dto.ParseRecentEntryPageQuery(requestContext.Request.URL.RawQuery)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用分页服务，处理取消及业务错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := listPage(ctx, actor, input.Query, input.Cursor)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			if errors.Is(err, domain.ErrInvalidRecentEntryCursor) {
				response := dto.ErrorResponse{
					Error: dto.ErrorDetail{
						Code: "INVALID_CURSOR", Message: "最近使用分页游标无效，请重新查询",
						Details: map[string]any{"field": "cursor"},
					},
					RequestID: requestcontext.RequestID(ctx),
				}
				requestContext.AbortWithStatusJSON(http.StatusBadRequest, response)
				return
			}
			AuthorizationFailure(requestContext, err)
			return
		}

		// 4. 核对结果数量及归属，交付最近使用分页响应。
		if result == nil || len(result.Entries) > input.Query.PageSize {
			InternalError(requestContext, nil)
			return
		}
		for _, candidate := range result.Entries {
			if candidate.Entry.UserID != actor.UserID {
				InternalError(requestContext, nil)
				return
			}
		}
		RecentEntryPageSuccess(requestContext, result)
	}
}
