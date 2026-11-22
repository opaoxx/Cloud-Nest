package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadCancellationSuccess 交付已提交的取消受理状态或任务已有的终态。
// 调用方完成认证、归属检查和取消服务调用。
func UploadCancellationSuccess(requestContext *gin.Context, result *domain.UploadTask) {
	// 1. 确认响应还可写，再核对受理状态。
	if requestContext.Writer.Written() || requestContext.IsAborted() {
		requestContext.Abort()
		return
	}
	if result == nil || (result.Status != domain.UploadStatusCancelling && !result.Status.IsTerminal()) {
		requestContext.Header("Pragma", "no-cache")
		InternalError(requestContext, nil)
		return
	}

	// 2. 取消处理返回 202，已有终态返回 200 并保留实际状态。
	responseStatus := http.StatusOK
	if result.Status == domain.UploadStatusCancelling {
		responseStatus = http.StatusAccepted
	}

	// 3. 复用任务公开响应，由任务 DTO 校验上传模式等公开字段后写出。
	writeUploadTaskSuccess(requestContext, result, responseStatus)
}
