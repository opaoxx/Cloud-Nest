package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// UploadTaskSuccess 交付上传任务服务成功返回的内部记录。
// 调用方完成认证、目标解析和服务调用，请求 ID 由公共中间件提供。
func UploadTaskSuccess(requestContext *gin.Context, result *domain.UploadTask) {
	// 1. 检查响应状态，再交付公开任务查询结果。
	if requestContext.Writer.Written() {
		requestContext.Abort()
		return
	}

	// 2. 复用任务 JSON 的编码和写出步骤。
	writeUploadTaskSuccess(requestContext, result, http.StatusOK)
}

// writeUploadTaskSuccess 为查询和取消响应写出同一套公开任务字段。
// 调用方已经检查响应状态并选好成功状态码。
func writeUploadTaskSuccess(requestContext *gin.Context, result *domain.UploadTask, responseStatus int) {
	// 1. 设置禁止缓存并检查请求取消。
	requestContext.Header("Cache-Control", "no-store")
	requestContext.Header("Pragma", "no-cache")
	ctx := requestContext.Request.Context()
	if ctx.Err() != nil {
		InternalError(requestContext, nil)
		return
	}

	// 2. 转换任务数据，附加请求 ID 并预先编码 JSON。
	data, err := dto.NewUploadTaskData(result)
	if err != nil {
		InternalError(requestContext, nil)
		return
	}
	responseBytes, err := json.Marshal(dto.UploadTaskResponse{Data: *data, RequestID: requestcontext.RequestID(ctx)})
	if err != nil {
		InternalError(requestContext, nil)
		return
	}

	// 3. 复查取消，设置响应头并交付成功正文。
	if ctx.Err() != nil {
		InternalError(requestContext, nil)
		return
	}
	requestContext.Writer.Header().Del("Content-Length")
	requestContext.Writer.Header().Del("Content-Encoding")
	requestContext.Header("Content-Type", "application/json; charset=utf-8")
	requestContext.Abort()
	requestContext.Data(responseStatus, "application/json; charset=utf-8", responseBytes)
}
