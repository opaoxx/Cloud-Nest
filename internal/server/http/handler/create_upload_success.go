package handler

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// CreateUploadSuccess 交付已确认提交并完成复用条件校验的上传创建快照。
// 请求 ID 由公共中间件提供，异常快照和取消交由统一错误响应处理。
func CreateUploadSuccess(requestContext *gin.Context, stableBody []byte) {
	// 1. 检查响应状态和取消信号，设置禁止缓存头。
	if !prepareCreateUploadSuccess(requestContext) {
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

// CreateUploadReplacementSuccess 交付已提交并校验过回执关联的内容替换创建快照。
func CreateUploadReplacementSuccess(requestContext *gin.Context, stableBody []byte) {
	// 1. 检查响应状态和取消信号，设置禁止缓存头。
	if !prepareCreateUploadSuccess(requestContext) {
		return
	}

	// 2. 按保存的模式严格恢复快照。
	data, _, err := restoreCreateUploadReplacementResponse(stableBody)
	if err != nil {
		InternalError(requestContext, nil)
		return
	}

	// 3. 关联当前请求 ID，交付 HTTP 201。
	writeCreateUploadSuccess(requestContext, data)
}

// restoreCreateUploadReplacementResponse 恢复完整响应，同时交付供请求关联核对的任务。
func restoreCreateUploadReplacementResponse(stableBody []byte) (any, *dto.UploadTaskData, error) {
	var snapshot struct {
		UploadMode string `json:"upload_mode"`
	}
	if err := json.Unmarshal(stableBody, &snapshot); err != nil {
		return nil, nil, domain.ErrInvalidIdempotencyRecordData
	}
	switch snapshot.UploadMode {
	case "SINGLE":
		data, err := dto.RestoreCreateUploadReplacementData(stableBody)
		if err != nil {
			return nil, nil, err
		}
		return data, &data.Task, nil
	case "MULTIPART":
		data, err := dto.RestoreCreateMultipartUploadReplacementData(stableBody)
		if err != nil {
			return nil, nil, err
		}
		return data, &data.Task, nil
	default:
		return nil, nil, domain.ErrInvalidIdempotencyRecordData
	}
}

func prepareCreateUploadSuccess(requestContext *gin.Context) bool {
	if requestContext.Writer.Written() {
		requestContext.Abort()
		return false
	}
	requestContext.Header("Cache-Control", "no-store")
	requestContext.Header("Pragma", "no-cache")
	if requestContext.Request.Context().Err() != nil {
		InternalError(requestContext, nil)
		return false
	}
	return true
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
