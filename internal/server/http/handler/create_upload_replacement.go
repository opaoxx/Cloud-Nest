package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// CreateUploadReplacement 绑定幂等替换创建服务，路由须先挂载认证中间件。
func CreateUploadReplacement(create func(ctx context.Context, actor domain.Actor, entryID uint64,
	input dto.CreateUploadReplacementInput, key string,
) (json.RawMessage, error)) gin.HandlerFunc {
	if create == nil {
		panic("内容替换创建处理函数需要幂等创建服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 读取认证身份、路径 ID 和幂等键。
		scope, valid := readEntryRevisionRequestScope(requestContext)
		if !valid {
			return
		}

		// 2. 解析受限 JSON，校验替换声明。
		var request dto.CreateUploadReplacementRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseCreateUploadReplacementRequest(request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用幂等服务，映射业务错误。
		ctx := requestContext.Request.Context()
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := create(ctx, scope.actor, scope.entryID, *input, scope.key)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writeCreateUploadReplacementFailure(requestContext, err)
			return
		}

		// 4. 核对创建快照与请求一致，再交付 HTTP 201。
		data, task, restoreError := restoreCreateUploadReplacementResponse(result)
		if restoreError != nil || task.SizeBytes != input.SizeBytes || task.Replacement == nil ||
			task.Replacement.EntryID != strconv.FormatUint(scope.entryID, 10) ||
			task.Replacement.ExpectedRevision != strconv.FormatUint(input.ExpectedRevision, 10) {
			InternalError(requestContext, nil)
			return
		}
		writeCreateUploadSuccess(requestContext, data)
	}
}

func writeCreateUploadReplacementFailure(requestContext *gin.Context, err error) {
	// 1. 补充替换目标类型和容量错误，其余沿用目录项版本操作的映射。
	var sizeLimit *domain.UploadSizeLimitError
	var detail dto.ErrorDetail
	var status int
	switch {
	case errors.Is(err, domain.ErrFileEntryNotFile):
		InvalidArgument(requestContext, &dto.FieldError{Field: "id", Message: "目标目录项须为文件"})
		return
	case errors.As(err, &sizeLimit) && sizeLimit != nil:
		status = http.StatusRequestEntityTooLarge
		detail = dto.ErrorDetail{Code: "FILE_TOO_LARGE", Message: "文件大小超过当前上传限制",
			Details: map[string]any{"field": "size_bytes", "max_size_bytes": sizeLimit.MaxSizeBytes}}
	case errors.Is(err, domain.ErrQuotaExceeded):
		status = http.StatusInsufficientStorage
		detail = dto.ErrorDetail{Code: "QUOTA_EXCEEDED", Message: "存储空间不足"}
	default:
		writeEntryRevisionFailure(requestContext, err, nil)
		return
	}

	// 2. 关联当前请求 ID，返回固定业务错误。
	requestContext.AbortWithStatusJSON(status, dto.ErrorResponse{
		Error: detail, RequestID: requestcontext.RequestID(requestContext.Request.Context()),
	})
}
