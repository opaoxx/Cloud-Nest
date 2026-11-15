package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// CreateDirectorySuccess 交付已确认提交的稳定创建结果。
// 调用方完成认证及结果复用条件检查，请求 ID 由公共中间件提供。
func CreateDirectorySuccess(requestContext *gin.Context, stableBody []byte) {
	// 1. 检查响应状态和请求取消。
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

	// 2. 恢复稳定创建快照，附加当前请求 ID 并预先编码 JSON。
	data, err := dto.RestoreCreateDirectoryData(stableBody)
	if err != nil {
		InternalError(requestContext, nil)
		return
	}
	responseBytes, err := json.Marshal(dto.CreateDirectoryResponse{Data: *data, RequestID: requestcontext.RequestID(ctx)})
	if err != nil {
		InternalError(requestContext, nil)
		return
	}

	// 3. 复查取消，设置响应头并交付 201 正文。
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
