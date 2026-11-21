package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadCompletionSuccess 交付已受理的上传进度或已发布结果。
// 调用方已完成认证、归属检查和完成确认服务调用。
func UploadCompletionSuccess(requestContext *gin.Context, result *domain.UploadTask) {
	// 1. 确认响应还可写，再复核上传模式、已受理阶段和发布结果。
	if requestContext.Writer.Written() || requestContext.IsAborted() {
		requestContext.Abort()
		return
	}
	status, err := domain.ResolveUploadCompletionResultStatus(result)
	if err != nil {
		requestContext.Header("Pragma", "no-cache")
		InternalError(requestContext, nil)
		return
	}

	// 2. 进行中的阶段返回 202，已发布返回 200。
	responseStatus := http.StatusOK
	if status == domain.UploadStatusCompleting || status == domain.UploadStatusVerifying {
		responseStatus = http.StatusAccepted
	}

	// 3. 复用公开任务 JSON 和请求 ID，写出前复查请求取消。
	writeUploadTaskSuccess(requestContext, result, responseStatus)
}
