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

// CreateDirectory 绑定幂等创建服务，路由须先挂载认证中间件。
// create 绑定 IdempotentCreateDirectoryService.Create，身份取自认证上下文。
func CreateDirectory(create func(context.Context, domain.Actor, uint64, string, *string, string) (json.RawMessage, error)) gin.HandlerFunc {
	// 1. 检查创建服务依赖，构造每次请求的处理函数。
	if create == nil {
		panic("创建文件夹处理函数需要幂等创建服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查响应状态、请求取消和已认证身份。
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

		// 3. 读取 JSON 正文并解析创建参数。
		var request dto.CreateDirectoryRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseCreateDirectoryRequest(request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 4. 调用幂等创建服务，处理取消和领域错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := create(ctx, actor, input.ParentID, input.Name, input.ColorKey, key)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			response := dto.ErrorResponse{RequestID: requestcontext.RequestID(ctx)}
			status := http.StatusConflict
			switch {
			case errors.Is(err, domain.ErrInvalidEntryName):
				status = http.StatusBadRequest
				response.Error = dto.ErrorDetail{Code: "INVALID_NAME", Message: domain.ErrInvalidEntryName.Error(), Details: map[string]any{"field": "name"}}
			case errors.Is(err, domain.ErrInvalidDirectoryColorKey):
				InvalidArgument(requestContext, &dto.FieldError{Field: "color_key", Message: domain.ErrInvalidDirectoryColorKey.Error()})
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
			case errors.Is(err, domain.ErrEntryNameConflict):
				response.Error = dto.ErrorDetail{Code: "NAME_CONFLICT", Message: domain.ErrEntryNameConflict.Error(), Details: nil}
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

		// 5. 核对返回快照与请求参数，交付创建响应。
		data, err := dto.RestoreCreateDirectoryData(result)
		if err != nil || data.Entry.ParentID != strconv.FormatUint(input.ParentID, 10) || data.Entry.Name != strings.TrimSpace(input.Name) ||
			(data.Entry.ColorKey == nil) != (input.ColorKey == nil) || (input.ColorKey != nil && *data.Entry.ColorKey != *input.ColorKey) {
			InternalError(requestContext, nil)
			return
		}
		CreateDirectorySuccess(requestContext, result)
	}
}
