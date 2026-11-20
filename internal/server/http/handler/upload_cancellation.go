package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadCancellation 绑定取消受理事务，路由须先挂载认证中间件。
// beginCancellation 按当前 Actor 和路径任务 ID 受理取消，提交后返回任务。
func UploadCancellation(beginCancellation func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error)) gin.HandlerFunc {
	// 1. 确认取消受理入口已装配。
	if beginCancellation == nil {
		panic("上传取消处理需要取消受理方法")
	}
	return uploadTaskAction(beginCancellation, UploadCancellationSuccess, writeUploadCancellationFailure)
}

// writeUploadCancellationFailure 写出受理事务返回的公开错误。
// 调用方已经准备响应头并确认请求仍有效。
func writeUploadCancellationFailure(requestContext *gin.Context, cancellationError error) {
	// 1. 选择固定业务错误，账号或会话状态交给已有授权处理。
	switch {
	case errors.Is(cancellationError, domain.ErrUploadTaskBusy):
		writeUploadTaskActionError(requestContext, http.StatusConflict, "UPLOAD_BUSY", domain.ErrUploadTaskBusy.Error())
	case errors.Is(cancellationError, domain.ErrUploadTaskNotReady):
		writeUploadTaskActionError(requestContext, http.StatusConflict, "TASK_NOT_READY", "上传任务当前不可取消")
	default:
		AuthorizationFailure(requestContext, cancellationError)
	}
}
