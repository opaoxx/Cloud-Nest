package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// RecordAccessSuccess 交付访问服务确认提交后的成功偏好。
// 调用方负责完成认证、参数及服务调用，请求 ID 由公共中间件提供。
func RecordAccessSuccess(requestContext *gin.Context, result *domain.EntryPreference) {
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

	// 2. 转换访问偏好，附加请求 ID 并预先编码 JSON。
	data, err := dto.NewRecordAccessData(result)
	if err != nil {
		InternalError(requestContext, nil)
		return
	}
	responseBytes, err := json.Marshal(dto.RecordAccessResponse{Data: *data, RequestID: requestcontext.RequestID(ctx)})
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
	requestContext.Data(http.StatusOK, "application/json; charset=utf-8", responseBytes)
}
