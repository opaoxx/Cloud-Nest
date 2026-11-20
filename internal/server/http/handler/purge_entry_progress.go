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

// PurgeEntryProgress 绑定只读进度服务，路由须先挂载共享 Bearer 认证。
func PurgeEntryProgress(getProgress func(context.Context, domain.Actor, uint64) (*domain.PurgeProgress, error)) gin.HandlerFunc {
	// 1. 检查进度服务依赖。
	if getProgress == nil {
		panic("永久清理进度处理函数需要只读进度服务方法")
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

		// 2. 解析路径中的目标 ID。
		path, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用进度服务，映射公开错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := getProgress(ctx, actor, path.EntryID)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if errors.Is(err, domain.ErrFileEntryNotFound) {
			requestContext.AbortWithStatusJSON(http.StatusNotFound, dto.ErrorResponse{
				Error:     dto.ErrorDetail{Code: "ENTRY_NOT_FOUND", Message: domain.ErrFileEntryNotFound.Error()},
				RequestID: requestcontext.RequestID(ctx),
			})
			return
		}
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}

		// 4. 核对结果目标，交付 HTTP 200。
		if result == nil || result.EntryID != path.EntryID {
			InternalError(requestContext, nil)
			return
		}
		PurgeEntryProgressSuccess(requestContext, result)
	}
}
