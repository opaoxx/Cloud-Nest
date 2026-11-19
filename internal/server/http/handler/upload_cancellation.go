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

// UploadCancellation 绑定取消受理事务，路由须先挂载认证中间件。
// beginCancellation 按当前 Actor 和路径任务 ID 受理取消，提交后返回任务。
func UploadCancellation(beginCancellation func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error)) gin.HandlerFunc {
	// 1. 确认取消受理入口已装配。
	if beginCancellation == nil {
		panic("上传取消处理需要取消受理方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查响应和请求是否仍有效，再读取认证身份。
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

		// 2. 解析路径任务 ID，再用同一请求上下文调用取消受理事务。
		input, fieldError := dto.ParseUploadTaskPath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := beginCancellation(ctx, actor, input.TaskID)

		// 3. 复查请求取消，按错误或任务归属决定能否交付。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writeUploadCancellationFailure(requestContext, err)
			return
		}
		if result == nil || result.ID != input.TaskID || result.UserID != actor.UserID {
			InternalError(requestContext, nil)
			return
		}
		UploadCancellationSuccess(requestContext, result)
	}
}

// writeUploadCancellationFailure 写出受理事务返回的公开错误。
// 调用方已经准备响应头并确认请求仍有效。
func writeUploadCancellationFailure(requestContext *gin.Context, cancellationError error) {
	// 1. 选择固定业务错误，账号或会话状态交给已有授权处理。
	var status int
	var code, message string
	switch {
	case errors.Is(cancellationError, domain.ErrUploadTaskNotFound):
		status, code, message = http.StatusNotFound, "UPLOAD_NOT_FOUND", domain.ErrUploadTaskNotFound.Error()
	case errors.Is(cancellationError, domain.ErrUploadTaskBusy):
		status, code, message = http.StatusConflict, "UPLOAD_BUSY", domain.ErrUploadTaskBusy.Error()
	case errors.Is(cancellationError, domain.ErrUploadTaskNotReady):
		status, code, message = http.StatusConflict, "TASK_NOT_READY", "上传任务当前不可取消"
	default:
		AuthorizationFailure(requestContext, cancellationError)
		return
	}

	// 2. 关联本次请求 ID，写出固定错误并结束处理。
	requestContext.AbortWithStatusJSON(status, dto.ErrorResponse{
		Error:     dto.ErrorDetail{Code: code, Message: message, Details: nil},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
