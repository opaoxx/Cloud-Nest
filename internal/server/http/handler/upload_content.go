package handler

import (
	"context"
	"io"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// UploadContent 绑定任务读取与原始正文接收，路由须先挂载认证中间件。
// getTask 读取当前用户的声明，receive 在读取正文前重新核对任务并领取接收租约。
func UploadContent(
	getTask func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error),
	receive func(context.Context, domain.Actor, *domain.UploadTask, io.Reader) (*domain.UploadTask, error),
) gin.HandlerFunc {
	if getTask == nil || receive == nil {
		panic("上传内容处理需要任务读取和内容接收方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查响应状态、请求取消和认证身份。
		if requestContext.Writer.Written() || requestContext.IsAborted() {
			requestContext.Abort()
			return
		}
		request := requestContext.Request
		ctx := request.Context()
		actor, exists := requestcontext.Actor(ctx)
		if ctx.Err() != nil || !exists {
			UploadContentFailure(requestContext, nil)
			return
		}

		// 2. 校验路径 ID 与媒体格式。
		input, fieldError := dto.ParseUploadTaskPath(requestContext.Param("id"))
		if fieldError != nil {
			requestContext.Writer.Header().Del("Content-Length")
			requestContext.Writer.Header().Del("Content-Encoding")
			requestContext.Header("Content-Type", "application/json; charset=utf-8")
			requestContext.Header("Pragma", "no-cache")
			InvalidArgument(requestContext, fieldError)
			return
		}
		if !binding.ValidateUploadContentHeaders(requestContext) {
			return
		}

		// 3. 读取当前用户的任务，核对声明和已知正文长度。
		task, err := getTask(ctx, actor, input.TaskID)
		if err != nil || ctx.Err() != nil {
			UploadContentFailure(requestContext, err)
			return
		}
		if task == nil || task.ID != input.TaskID || task.UserID != actor.UserID {
			UploadContentFailure(requestContext, domain.ErrInvalidUploadTaskData)
			return
		}
		if task.FileSize > domain.MaxSingleUploadSizeBytes {
			UploadContentFailure(requestContext, domain.ErrUploadFileTooLarge)
			return
		}
		if !binding.ValidateUploadContentLength(requestContext, task.FileSize) {
			return
		}

		// 4. 在传输期限内调用接收服务。
		var result *domain.UploadTask
		err = binding.WithUploadTransferDeadline(ctx, requestContext.Writer, func(transferContext context.Context) error {
			received, receiveError := receive(transferContext, actor, task, request.Body)
			result = received
			return receiveError
		})
		if err != nil {
			UploadContentFailure(requestContext, err)
			return
		}

		// 5. 核对返回任务，交付 202 或固定错误。
		if result == nil || result.ID != input.TaskID || result.UserID != actor.UserID {
			UploadContentFailure(requestContext, domain.ErrInvalidUploadTaskData)
			return
		}
		UploadContentSuccess(requestContext, result)
	}
}
