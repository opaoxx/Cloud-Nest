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

// RenameDevice 绑定设备改名服务，路由须先挂载认证中间件。
// renameDevice 绑定 RenameDeviceService.Rename，目标来自路径 id，名称来自 JSON 正文。
func RenameDevice(renameDevice func(context.Context, domain.Actor, uint64, string) (*domain.UserDevice, error)) gin.HandlerFunc {
	// 1. 检查改名服务依赖，构造每次请求的处理函数。
	if renameDevice == nil {
		panic("设备改名处理函数需要改名服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查响应状态、请求取消和已认证身份。
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
		actor, exists := requestcontext.Actor(ctx)
		if !exists {
			InternalError(requestContext, nil)
			return
		}

		// 2. 解析路径中的目标设备 ID。
		input, fieldError := dto.ParseUserDevicePath(requestContext.Param("id"))
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 读取 JSON 正文并校验设备名称。
		var request dto.RenameDeviceRequest
		if !binding.ReadJSON(requestContext, &request) {
			return
		}
		if fieldError := request.Validate(); fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 4. 调用改名服务，处理取消及目标与认证错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := renameDevice(ctx, actor, input.DeviceID, request.Name)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if errors.Is(err, domain.ErrTargetDeviceNotFound) {
			response := dto.ErrorResponse{
				Error:     dto.ErrorDetail{Code: "DEVICE_NOT_FOUND", Message: "设备不存在", Details: nil},
				RequestID: requestcontext.RequestID(ctx),
			}
			requestContext.Writer.Header().Del("Content-Length")
			requestContext.Writer.Header().Del("Content-Encoding")
			requestContext.Header("Content-Type", "application/json; charset=utf-8")
			requestContext.AbortWithStatusJSON(http.StatusNotFound, response)
			return
		}
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}

		// 5. 核对设备结果并交付成功响应。
		if result == nil || result.ID != input.DeviceID || result.UserID != actor.UserID || result.Name != request.Name {
			InternalError(requestContext, nil)
			return
		}
		RenameDeviceSuccess(requestContext, result)
	}
}
