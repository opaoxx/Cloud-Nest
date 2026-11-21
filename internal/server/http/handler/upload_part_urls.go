package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// UploadPartURLs 绑定 UploadPartURLService.Create，路由须先挂载认证中间件。
func UploadPartURLs(
	create func(context.Context, domain.Actor, uint64, []domain.UploadPartDeclaration, *time.Time) (*domain.UploadPartURLBatch, error),
) gin.HandlerFunc {
	if create == nil {
		panic("分片地址签发处理函数需要批量签发服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查请求状态，读取可信身份并准备禁止缓存的响应。
		if requestContext.Writer.Written() || requestContext.IsAborted() {
			requestContext.Abort()
			return
		}
		requestContext.Writer.Header().Del("Content-Length")
		requestContext.Writer.Header().Del("Content-Encoding")
		requestContext.Header("Content-Type", "application/json; charset=utf-8")
		requestContext.Header("Cache-Control", "no-store")
		requestContext.Header("Pragma", "no-cache")
		ctx := requestContext.Request.Context()
		actor, exists := requestcontext.Actor(ctx)
		if ctx.Err() != nil || !exists {
			InternalError(requestContext, nil)
			return
		}

		// 2. 解析任务 ID 和有界 JSON，调用批量签发服务。
		path, fieldError := dto.ParseUploadTaskPath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}
		var request dto.UploadPartURLsRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		input, fieldError := dto.ParseUploadPartURLsRequest(request)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := create(ctx, actor, path.TaskID, input.Parts, input.ExpectedExpiry)

		// 3. 先复查取消，再映射错误或交付与请求一致的完整批次。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writeUploadPartURLsFailure(requestContext, err)
			return
		}
		data, err := dto.NewUploadPartURLsData(result)
		if err != nil || !matchesUploadPartDeclarations(data.Parts, input.Parts) {
			InternalError(requestContext, nil)
			return
		}
		body, err := json.Marshal(dto.UploadPartURLsResponse{Data: *data, RequestID: requestcontext.RequestID(ctx)})
		if err != nil || ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		requestContext.Abort()
		requestContext.Data(http.StatusOK, "application/json; charset=utf-8", body)
	}
}

func matchesUploadPartDeclarations(parts []dto.UploadPartURLData, declarations []domain.UploadPartDeclaration) bool {
	// 1. 逐项确认结果的数量、顺序和摘要匹配原声明。
	if len(parts) != len(declarations) {
		return false
	}
	for index, part := range parts {
		declaration := declarations[index]
		digest, valid := domain.DecodeUploadPartSHA256(declaration.SHA256)
		if !valid || part.PartNumber != declaration.PartNumber ||
			part.RequiredHeaders.ChecksumSHA256 != base64.StdEncoding.EncodeToString(digest[:]) {
			return false
		}
	}
	return true
}

func writeUploadPartURLsFailure(requestContext *gin.Context, err error) {
	// 1. 使用可定位的输入错误或固定的公开业务错误，不回显底层错误文本。
	var declarationError *domain.UploadPartDeclarationError
	switch {
	case errors.As(err, &declarationError) && declarationError != nil:
		InvalidArgument(requestContext, &dto.FieldError{Field: declarationError.Field, Message: declarationError.Message})
	case errors.Is(err, domain.ErrUploadTaskNotFound):
		writeUploadTaskActionError(requestContext, http.StatusNotFound, "UPLOAD_NOT_FOUND", domain.ErrUploadTaskNotFound.Error())
	case errors.Is(err, domain.ErrUploadTaskNotReady):
		writeUploadTaskActionError(requestContext, http.StatusConflict, "TASK_NOT_READY", "上传任务当前不可签发分片地址")
	case errors.Is(err, domain.ErrUploadTaskBusy):
		writeUploadTaskActionError(requestContext, http.StatusConflict, "UPLOAD_BUSY", domain.ErrUploadTaskBusy.Error())
	case errors.Is(err, domain.ErrUploadStorageUnavailable):
		writeUploadTaskActionError(requestContext, http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE", domain.ErrUploadStorageUnavailable.Error())
	case errors.Is(err, domain.ErrQuotaExceeded):
		writeUploadTaskActionError(requestContext, http.StatusInsufficientStorage, "QUOTA_EXCEEDED", domain.ErrQuotaExceeded.Error())
	default:
		AuthorizationFailure(requestContext, err)
	}
}
