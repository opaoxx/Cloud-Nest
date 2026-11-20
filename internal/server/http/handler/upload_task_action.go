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

// uploadTaskAction 处理完成或取消请求共用的认证上下文、路径解析和结果核对。
func uploadTaskAction(
	execute func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error),
	succeed func(*gin.Context, *domain.UploadTask),
	reject func(*gin.Context, error),
) gin.HandlerFunc {
	return func(requestContext *gin.Context) {
		// 1. 检查响应和请求状态，读取认证身份。
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

		// 2. 解析路径任务 ID，用同一上下文调用当前操作。
		input, fieldError := dto.ParseUploadTaskPath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := execute(ctx, actor, input.TaskID)

		// 3. 复查请求取消，再交付固定错误或通过归属核对的任务。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if errors.Is(err, domain.ErrUploadTaskNotFound) {
			writeUploadTaskActionError(requestContext, http.StatusNotFound, "UPLOAD_NOT_FOUND", domain.ErrUploadTaskNotFound.Error())
			return
		}
		if err != nil {
			reject(requestContext, err)
			return
		}
		if result == nil || result.ID != input.TaskID || result.UserID != actor.UserID {
			InternalError(requestContext, nil)
			return
		}
		succeed(requestContext, result)
	}
}

func writeUploadTaskActionError(requestContext *gin.Context, status int, code, message string) {
	// 1. 关联本次请求 ID，写出固定公开错误并结束处理。
	requestContext.AbortWithStatusJSON(status, dto.ErrorResponse{
		Error:     dto.ErrorDetail{Code: code, Message: message, Details: nil},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
