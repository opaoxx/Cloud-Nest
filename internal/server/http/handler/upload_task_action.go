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
		// 1. 统一检查认证身份和路径，用同一上下文调用当前操作。
		ctx, actor, taskID, ready := prepareUploadTaskAction(requestContext)
		if !ready {
			return
		}
		result, err := execute(ctx, actor, taskID)

		// 2. 复查请求取消，再交付固定错误或通过归属核对的任务。
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
		if result == nil || result.ID != taskID || result.UserID != actor.UserID {
			InternalError(requestContext, nil)
			return
		}
		succeed(requestContext, result)
	}
}

// prepareUploadTaskAction 确认请求可继续，并提取可信身份和任务 ID。
func prepareUploadTaskAction(requestContext *gin.Context) (context.Context, domain.Actor, uint64, bool) {
	// 1. 检查是否已结束，清理暂存正文头并读取认证身份。
	if requestContext.Writer.Written() || requestContext.IsAborted() {
		requestContext.Abort()
		return nil, domain.Actor{}, 0, false
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
		return nil, domain.Actor{}, 0, false
	}

	// 2. 只从当前路由解析任务 ID，并复查请求取消。
	input, fieldError := dto.ParseUploadTaskPath(requestContext.Param("id"))
	if fieldError != nil {
		InvalidArgument(requestContext, fieldError)
		return nil, domain.Actor{}, 0, false
	}
	if ctx.Err() != nil {
		InternalError(requestContext, nil)
		return nil, domain.Actor{}, 0, false
	}
	return ctx, actor, input.TaskID, true
}

func writeUploadTaskActionError(requestContext *gin.Context, status int, code, message string) {
	// 1. 关联本次请求 ID，写出固定公开错误并结束处理。
	requestContext.AbortWithStatusJSON(status, dto.ErrorResponse{
		Error:     dto.ErrorDetail{Code: code, Message: message, Details: nil},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
