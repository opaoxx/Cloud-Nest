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

// TrashPage 绑定回收站分页服务。
// 路由先挂载共享认证中间件，分页服务使用认证后的 Actor。
func TrashPage(listPage func(context.Context, domain.Actor, int, string) (*domain.TrashEntryPageResult, error)) gin.HandlerFunc {
	// 1. 确认分页服务已装配。
	if listPage == nil {
		panic("回收站列表处理函数需要分页服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查请求状态，从上下文读取已认证身份。
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
		if !exists || ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}

		// 2. 解析 cursor 和 limit，拒绝无效查询。
		input, fieldError := dto.ParseTrashPageQuery(requestContext.Request.URL.RawQuery)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用分页服务，按约定处理错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := listPage(ctx, actor, input.PageSize, input.Cursor)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			if errors.Is(err, domain.ErrInvalidTrashEntryCursor) {
				requestContext.AbortWithStatusJSON(http.StatusBadRequest, dto.ErrorResponse{
					Error: dto.ErrorDetail{
						Code: "INVALID_CURSOR", Message: "回收站分页游标无效，请重新查询",
						Details: map[string]any{"field": "cursor"},
					},
					RequestID: requestcontext.RequestID(ctx),
				})
				return
			}
			AuthorizationFailure(requestContext, err)
			return
		}

		// 4. 核对结果数量和归属，交给成功响应函数。
		if result == nil || len(result.Entries) > input.PageSize {
			InternalError(requestContext, nil)
			return
		}
		for _, item := range result.Entries {
			if item.Entry.UserID != actor.UserID {
				InternalError(requestContext, nil)
				return
			}
		}
		TrashPageSuccess(requestContext, result)
	}
}
