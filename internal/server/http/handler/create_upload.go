package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// CreateUpload 绑定上传创建幂等服务，路由须先挂载认证中间件。
// create 绑定 IdempotentCreateUploadService.Create，身份来自可信请求上下文。
func CreateUpload(create func(context.Context, domain.Actor, domain.CreateUploadInput, string) (json.RawMessage, error)) gin.HandlerFunc {
	return createUploadHandler(create, false)
}

// CreateUploadWithMultipart 交付两种模式的创建结果，依赖已装配的分片创建服务。
func CreateUploadWithMultipart(create func(context.Context, domain.Actor, domain.CreateUploadInput, string) (json.RawMessage, error)) gin.HandlerFunc {
	return createUploadHandler(create, true)
}

func createUploadHandler(create func(context.Context, domain.Actor, domain.CreateUploadInput, string) (json.RawMessage, error), supportsMultipart bool) gin.HandlerFunc {
	// 1. 检查服务依赖，构造每次请求的处理函数。
	if create == nil {
		panic("上传创建处理函数需要幂等创建服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查响应状态、取消信号和认证身份。
		if requestContext.Writer.Written() {
			requestContext.Abort()
			return
		}
		requestContext.Header("Cache-Control", "no-store")
		requestContext.Header("Pragma", "no-cache")
		requestContext.Writer.Header().Del("Content-Length")
		requestContext.Writer.Header().Del("Content-Encoding")
		requestContext.Header("Content-Type", "application/json; charset=utf-8")
		ctx := requestContext.Request.Context()
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		actor, exists := requestcontext.Actor(ctx)
		if !exists {
			InternalError(requestContext, nil)
			return
		}

		// 2. 解析幂等请求头。
		key, fieldError := dto.ParseIdempotencyKey(requestContext.Request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 读取 JSON 并转换上传声明。
		var request dto.CreateUploadRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseCreateUploadRequest(request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 4. 调用幂等创建服务，映射业务及认证错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := create(ctx, actor, *input, key)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			response := dto.ErrorResponse{RequestID: requestcontext.RequestID(ctx)}
			status := http.StatusConflict
			var sizeLimit *domain.UploadSizeLimitError
			switch {
			case errors.Is(err, domain.ErrInvalidEntryName):
				status = http.StatusBadRequest
				response.Error = dto.ErrorDetail{Code: "INVALID_NAME", Message: domain.ErrInvalidEntryName.Error(), Details: map[string]any{"field": "name"}}
			case errors.Is(err, domain.ErrInvalidUploadParentID):
				InvalidArgument(requestContext, &dto.FieldError{Field: "parent_id", Message: domain.ErrInvalidUploadParentID.Error()})
				return
			case errors.Is(err, domain.ErrFileEntryNotDirectory):
				InvalidArgument(requestContext, &dto.FieldError{Field: "parent_id", Message: "父目录须为文件夹"})
				return
			case errors.Is(err, domain.ErrFileEntryDepthExceeded):
				InvalidArgument(requestContext, &dto.FieldError{Field: "parent_id", Message: domain.ErrFileEntryDepthExceeded.Error()})
				return
			case errors.Is(err, domain.ErrFileEntryNotFound):
				status = http.StatusNotFound
				response.Error = dto.ErrorDetail{Code: "ENTRY_NOT_FOUND", Message: "目录项不存在", Details: nil}
			case supportsMultipart && errors.Is(err, domain.ErrMultipartUploadSHA256Required):
				InvalidArgument(requestContext, &dto.FieldError{Field: "sha256", Message: domain.ErrMultipartUploadSHA256Required.Error()})
				return
			case supportsMultipart && errors.As(err, &sizeLimit) && sizeLimit != nil:
				status = http.StatusRequestEntityTooLarge
				response.Error = dto.ErrorDetail{Code: "FILE_TOO_LARGE", Message: "文件大小超过当前上传限制", Details: map[string]any{"field": "size_bytes", "max_size_bytes": sizeLimit.MaxSizeBytes}}
			case errors.Is(err, domain.ErrUploadFileTooLarge):
				status = http.StatusRequestEntityTooLarge
				response.Error = dto.ErrorDetail{Code: "FILE_TOO_LARGE", Message: domain.ErrUploadFileTooLarge.Error(), Details: map[string]any{"field": "size_bytes", "max_size_bytes": domain.MaxSingleUploadSizeBytes}}
			case errors.Is(err, domain.ErrQuotaExceeded):
				status = http.StatusInsufficientStorage
				response.Error = dto.ErrorDetail{Code: "QUOTA_EXCEEDED", Message: domain.ErrQuotaExceeded.Error(), Details: nil}
			case errors.Is(err, domain.ErrIdempotencyRequestConflict):
				response.Error = dto.ErrorDetail{Code: "IDEMPOTENCY_CONFLICT", Message: domain.ErrIdempotencyRequestConflict.Error(), Details: nil}
			case errors.Is(err, domain.ErrIdempotencyRecordExpired):
				response.Error = dto.ErrorDetail{Code: "IDEMPOTENCY_EXPIRED", Message: domain.ErrIdempotencyRecordExpired.Error(), Details: nil}
			default:
				AuthorizationFailure(requestContext, err)
				return
			}
			requestContext.AbortWithStatusJSON(status, response)
			return
		}

		// 5. 核对返回快照与请求声明，再交付成功响应。
		if supportsMultipart && input.SizeBytes > domain.MaxSingleUploadSizeBytes {
			data, err := dto.RestoreCreateMultipartUploadData(result)
			if err != nil || !matchesCreateUploadDeclaration(data.Task, *input) {
				InternalError(requestContext, nil)
				return
			}
			writeCreateUploadSuccess(requestContext, data)
			return
		}
		data, err := dto.RestoreCreateUploadData(result)
		if err != nil || !matchesCreateUploadDeclaration(data.Task, *input) {
			InternalError(requestContext, nil)
			return
		}
		CreateUploadSuccess(requestContext, result)
	}
}

func matchesCreateUploadDeclaration(task dto.UploadTaskData, input domain.CreateUploadInput) bool {
	return task.ParentID == strconv.FormatUint(input.ParentID, 10) &&
		task.RequestedName == strings.TrimSpace(input.Name) && task.SizeBytes == input.SizeBytes
}
