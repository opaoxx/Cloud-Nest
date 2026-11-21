package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// CreateUploadSuccess 交付已确认提交并完成复用条件校验的上传创建快照。
// 请求 ID 由公共中间件提供，异常快照和取消交由统一错误响应处理。
func CreateUploadSuccess(requestContext *gin.Context, stableBody []byte) {
	// 1. 检查响应是否已发送，以及请求是否已取消。
	if requestContext.Writer.Written() {
		requestContext.Abort()
		return
	}
	requestContext.Header("Cache-Control", "no-store")
	requestContext.Header("Pragma", "no-cache")
	ctx := requestContext.Request.Context()
	if ctx.Err() != nil {
		InternalError(requestContext, nil)
		return
	}

	// 2. 恢复稳定快照，交给共同的成功响应交付。
	data, err := dto.RestoreCreateUploadData(stableBody)
	if err != nil {
		InternalError(requestContext, nil)
		return
	}
	writeCreateUploadSuccess(requestContext, data)
}

func writeCreateUploadSuccess(requestContext *gin.Context, data any) {
	// 1. 关联当前请求 ID，预先编码公开快照。
	ctx := requestContext.Request.Context()
	responseBytes, err := json.Marshal(struct {
		Data      any    `json:"data"`
		RequestID string `json:"request_id"`
	}{Data: data, RequestID: requestcontext.RequestID(ctx)})
	if err != nil {
		InternalError(requestContext, nil)
		return
	}

	// 2. 复查取消状态，设置响应头并交付 HTTP 201。
	if ctx.Err() != nil {
		InternalError(requestContext, nil)
		return
	}
	requestContext.Writer.Header().Del("Content-Length")
	requestContext.Writer.Header().Del("Content-Encoding")
	requestContext.Header("Content-Type", "application/json; charset=utf-8")
	requestContext.Abort()
	requestContext.Data(http.StatusCreated, "application/json; charset=utf-8", responseBytes)
}
