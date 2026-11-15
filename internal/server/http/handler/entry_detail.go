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

// EntryDetail 绑定详情服务，路由须先挂载认证中间件。
// getDetail 绑定 EntryDetailService.Get，目标来自路径 id，身份来自认证上下文。
func EntryDetail(getDetail func(context.Context, domain.Actor, uint64) (*domain.EntryDetail, error)) gin.HandlerFunc {
	// 1. 检查详情服务依赖，构造每次请求的处理函数。
	if getDetail == nil {
		panic("目录项详情处理函数需要详情服务方法")
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

		// 2. 解析路径中的目标目录项 ID。
		input, fieldError := dto.ParseEntryPath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用详情服务，处理取消、目标和认证错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := getDetail(ctx, actor, input.EntryID)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if errors.Is(err, domain.ErrFileEntryNotFound) {
			response := dto.ErrorResponse{
				Error:     dto.ErrorDetail{Code: "ENTRY_NOT_FOUND", Message: "目录项不存在", Details: nil},
				RequestID: requestcontext.RequestID(ctx),
			}
			requestContext.AbortWithStatusJSON(http.StatusNotFound, response)
			return
		}
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}

		// 4. 核对返回目标及用户，交付详情响应。
		if result == nil || len(result.Path.Entries) == 0 {
			InternalError(requestContext, nil)
			return
		}
		target := result.Path.Entries[len(result.Path.Entries)-1]
		if target.ID != input.EntryID || target.UserID != actor.UserID {
			InternalError(requestContext, nil)
			return
		}
		EntryDetailSuccess(requestContext, result)
	}
}
