package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadCompletionSuccess 交付已确认处于校验中或已发布的单文件任务。
// 调用方已完成认证、归属检查和完成确认服务调用。
func UploadCompletionSuccess(requestContext *gin.Context, result *domain.UploadTask) {
	// 1. 确认响应还可写，再复核完成状态和发布结果。
	if requestContext.Writer.Written() || requestContext.IsAborted() {
		requestContext.Abort()
		return
	}
	status, err := domain.ResolveSingleUploadCompletionStatus(result)
	if err != nil {
		requestContext.Header("Pragma", "no-cache")
		InternalError(requestContext, nil)
		return
	}

	// 2. 校验中选择 202，已发布选择 200。
	responseStatus := http.StatusOK
	if status == domain.UploadStatusVerifying {
		responseStatus = http.StatusAccepted
	}

	// 3. 复用公开任务响应，在写出前复查请求取消。
	writeUploadTaskSuccess(requestContext, result, responseStatus)
}
