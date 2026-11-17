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

// UploadTask 绑定任务读取服务，路由先挂载认证中间件。
// getTask 绑定 UploadTaskService.Get，目标来自路径 id，身份来自认证上下文。
func UploadTask(getTask func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error)) gin.HandlerFunc {
	// 1. 检查任务服务依赖，构造每次请求的处理函数。
	if getTask == nil {
		panic("上传任务查询处理函数需要任务服务方法")
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

		// 2. 解析路径中的上传任务 ID。
		input, fieldError := dto.ParseUploadTaskPath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用任务服务，处理取消、任务缺失和授权错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := getTask(ctx, actor, input.TaskID)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if errors.Is(err, domain.ErrUploadTaskNotFound) {
			response := dto.ErrorResponse{
				Error:     dto.ErrorDetail{Code: "UPLOAD_NOT_FOUND", Message: "上传任务不存在", Details: nil},
				RequestID: requestcontext.RequestID(ctx),
			}
			requestContext.AbortWithStatusJSON(http.StatusNotFound, response)
			return
		}
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}

		// 4. 核对返回目标及用户，交付任务响应。
		if result == nil || result.ID != input.TaskID || result.UserID != actor.UserID {
			InternalError(requestContext, nil)
			return
		}
		UploadTaskSuccess(requestContext, result)
	}
}
