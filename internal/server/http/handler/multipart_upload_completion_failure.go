package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// writeMultipartUploadCompletionFailure 按协议交付分片上传完成的公开错误。
func writeMultipartUploadCompletionFailure(requestContext *gin.Context, completionError error) {
	// 1. 确认响应可写，并使用完成操作的原始错误来选择公开响应。
	if requestContext.Writer.Written() || requestContext.IsAborted() {
		requestContext.Abort()
		return
	}
	requestContext.Writer.Header().Del("Content-Length")
	requestContext.Writer.Header().Del("Content-Encoding")
	requestContext.Header("Content-Type", "application/json; charset=utf-8")
	requestContext.Header("Cache-Control", "no-store")
	requestContext.Header("Pragma", "no-cache")
	if requestContext.Request.Context().Err() != nil {
		InternalError(requestContext, nil)
		return
	}
	var releaseFailure *domain.UploadCompletionReleaseError
	if errors.As(completionError, &releaseFailure) && releaseFailure != nil {
		completionError = releaseFailure.Cause
	}

	// 2. 按公开契约映射状态码。字段错误和授权错误复用现有写法。
	var declarationError *domain.UploadPartDeclarationError
	var mismatch *domain.UploadPartsMismatchError
	switch {
	case errors.As(completionError, &declarationError) && declarationError != nil:
		InvalidArgument(requestContext, &dto.FieldError{Field: declarationError.Field, Message: declarationError.Message})
	case errors.Is(completionError, domain.ErrUploadTaskNotFound):
		writeUploadTaskActionError(requestContext, http.StatusNotFound, "UPLOAD_NOT_FOUND", domain.ErrUploadTaskNotFound.Error())
	case errors.Is(completionError, domain.ErrUploadTaskNotReady):
		writeUploadTaskActionError(requestContext, http.StatusConflict, "TASK_NOT_READY", "上传任务当前不可完成")
	case errors.Is(completionError, domain.ErrUploadTaskBusy):
		writeUploadTaskActionError(requestContext, http.StatusConflict, "UPLOAD_BUSY", domain.ErrUploadTaskBusy.Error())
	case errors.As(completionError, &mismatch) && mismatch != nil:
		writeUploadPartsMismatch(requestContext, mismatch.PartNumbers)
	case errors.Is(completionError, domain.ErrUploadStorageUnavailable):
		writeUploadTaskActionError(requestContext, http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE", domain.ErrUploadStorageUnavailable.Error())
	default:
		AuthorizationFailure(requestContext, completionError)
	}
}

func writeUploadPartsMismatch(requestContext *gin.Context, partNumbers []uint32) {
	// 1. 确认失配编号在协议范围内，且清单有界并严格升序。
	if len(partNumbers) == 0 || len(partNumbers) > domain.MaxUploadPartsMismatchNumbers {
		InternalError(requestContext, nil)
		return
	}
	var previousNumber uint32
	for _, number := range partNumbers {
		if number <= previousNumber || number > domain.MaxUploadPartCount {
			InternalError(requestContext, nil)
			return
		}
		previousNumber = number
	}

	// 2. 写出固定错误、允许公开的编号和请求 ID。
	requestContext.AbortWithStatusJSON(http.StatusConflict, dto.ErrorResponse{
		Error: dto.ErrorDetail{Code: "UPLOAD_PARTS_MISMATCH", Message: domain.ErrUploadPartsMismatch.Error(),
			Details: map[string]any{"part_numbers": partNumbers}},
		RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
