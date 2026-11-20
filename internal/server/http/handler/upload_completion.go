package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadCompletion 绑定完成确认服务，路由须先挂载认证中间件。
// confirmCompletion 按当前 Actor 和路径任务 ID 读取并校验任务快照。
func UploadCompletion(confirmCompletion func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error)) gin.HandlerFunc {
	// 1. 检查依赖，将完成确认交给共用上传任务请求流程。
	if confirmCompletion == nil {
		panic("上传完成处理需要完成确认方法")
	}
	return uploadTaskAction(confirmCompletion, UploadCompletionSuccess, writeUploadCompletionFailure)
}

func writeUploadCompletionFailure(requestContext *gin.Context, completionError error) {
	// 1. 按契约把任务未就绪交付为 409。
	if errors.Is(completionError, domain.ErrUploadTaskNotReady) {
		writeUploadTaskActionError(requestContext, http.StatusConflict, "TASK_NOT_READY", "上传任务当前不可完成")
		return
	}

	// 2. 复用授权错误与统一内部错误的交付。
	AuthorizationFailure(requestContext, completionError)
}
