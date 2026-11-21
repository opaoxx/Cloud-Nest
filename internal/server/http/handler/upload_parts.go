package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// UploadParts 绑定 UploadPartReceiptService.List，路由须先挂载认证中间件。
func UploadParts(list func(context.Context, domain.Actor, uint64) (*domain.UploadPartReceiptList, error)) gin.HandlerFunc {
	if list == nil {
		panic("分片回执处理函数需要只读核对服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查响应状态及认证身份，准备禁止缓存的 JSON 响应。
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

		// 2. 解析路径任务 ID，将可信身份和任务交给只读核对服务。
		path, fieldError := dto.ParseUploadTaskPath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := list(ctx, actor, path.TaskID)

		// 3. 复查请求取消，转换整批公开数据或返回固定错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			writeUploadPartsFailure(requestContext, err)
			return
		}
		data, err := dto.NewUploadPartsData(result)
		if err != nil {
			InternalError(requestContext, nil)
			return
		}
		body, err := json.Marshal(dto.UploadPartsResponse{Data: *data, RequestID: requestcontext.RequestID(ctx)})
		if err != nil || ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		requestContext.Abort()
		requestContext.Data(http.StatusOK, "application/json; charset=utf-8", body)
	}
}

func writeUploadPartsFailure(requestContext *gin.Context, err error) {
	// 1. 只返回约定的公开错误，保留统一认证和内部错误处理。
	switch {
	case errors.Is(err, domain.ErrUploadTaskNotFound):
		writeUploadTaskActionError(requestContext, http.StatusNotFound, "UPLOAD_NOT_FOUND", domain.ErrUploadTaskNotFound.Error())
	case errors.Is(err, domain.ErrUploadTaskNotReady), errors.Is(err, domain.ErrInvalidUploadTaskData):
		writeUploadTaskActionError(requestContext, http.StatusConflict, "TASK_NOT_READY", "上传任务当前不可核对分片回执")
	case errors.Is(err, domain.ErrUploadStorageUnavailable):
		writeUploadTaskActionError(requestContext, http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE", domain.ErrUploadStorageUnavailable.Error())
	default:
		AuthorizationFailure(requestContext, err)
	}
}
