package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// UploadContentFailure 将任务查询或内容接收错误转换为固定公开响应。
func UploadContentFailure(requestContext *gin.Context, receiveError error) {
	// 1. 检查响应状态，准备统一错误响应头。
	if requestContext.Writer.Written() || requestContext.IsAborted() {
		requestContext.Abort()
		return
	}
	requestContext.Writer.Header().Del("Content-Length")
	requestContext.Writer.Header().Del("Content-Encoding")
	requestContext.Header("Content-Type", "application/json; charset=utf-8")
	requestContext.Header("Cache-Control", "no-store")
	requestContext.Header("Pragma", "no-cache")

	// 2. 按超时、请求取消和业务原因选择公开错误。
	ctx := requestContext.Request.Context()
	requestError := ctx.Err()
	var status int
	var code, message string
	switch {
	case errors.Is(receiveError, binding.ErrUploadTransferTimeout), errors.Is(requestError, context.DeadlineExceeded):
		status, code, message = http.StatusRequestTimeout, "UPLOAD_TIMEOUT", "上传传输超时，请查询任务状态后重试"
	case requestError != nil:
		InternalError(requestContext, nil)
		return
	case errors.Is(receiveError, domain.ErrUploadTaskNotFound):
		status, code, message = http.StatusNotFound, "UPLOAD_NOT_FOUND", domain.ErrUploadTaskNotFound.Error()
	case errors.Is(receiveError, domain.ErrUploadTaskNotReady), errors.Is(receiveError, domain.ErrUploadLeaseVersionExhausted):
		status, code, message = http.StatusConflict, "TASK_NOT_READY", domain.ErrUploadTaskNotReady.Error()
	case errors.Is(receiveError, domain.ErrUploadTaskBusy), errors.Is(receiveError, domain.ErrUploadObjectExists):
		status, code, message = http.StatusConflict, "UPLOAD_BUSY", domain.ErrUploadTaskBusy.Error()
	case errors.Is(receiveError, domain.ErrUploadReceiverBusy):
		status, code, message = http.StatusConflict, "UPLOAD_BUSY", domain.ErrUploadReceiverBusy.Error()
	case errors.Is(receiveError, domain.ErrUploadFileTooLarge):
		status, code, message = http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", domain.ErrUploadFileTooLarge.Error()
	case errors.Is(receiveError, domain.ErrUploadBodySizeMismatch):
		status, code, message = http.StatusRequestEntityTooLarge, "BODY_SIZE_MISMATCH", domain.ErrUploadBodySizeMismatch.Error()
	case errors.Is(receiveError, domain.ErrUploadChecksumMismatch):
		status, code, message = http.StatusUnprocessableEntity, "CHECKSUM_MISMATCH", domain.ErrUploadChecksumMismatch.Error()
	case errors.Is(receiveError, domain.ErrUploadTempSpaceExhausted):
		status, code, message = http.StatusInsufficientStorage, "TEMP_SPACE_EXHAUSTED", domain.ErrUploadTempSpaceExhausted.Error()
	case errors.Is(receiveError, domain.ErrUploadStorageUnavailable):
		status, code, message = http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE", domain.ErrUploadStorageUnavailable.Error()
	default:
		// 用户事务可能发现账号、设备或会话失效，复用现有授权响应。
		AuthorizationFailure(requestContext, receiveError)
		return
	}

	// 3. 写出带请求 ID 的固定 JSON，并结束处理。
	requestContext.AbortWithStatusJSON(status, dto.ErrorResponse{
		Error:     dto.ErrorDetail{Code: code, Message: message, Details: nil},
		RequestID: requestcontext.RequestID(ctx),
	})
}
