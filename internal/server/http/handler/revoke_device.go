package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// RevokeDevice 绑定设备及关联会话撤销服务，路由须先挂载认证中间件。
// revokeDevice 绑定 RevokeDeviceService.Revoke，目标取自路径 id。
func RevokeDevice(revokeDevice func(context.Context, domain.Actor, uint64) (*domain.RevokeDeviceResult, error)) gin.HandlerFunc {
	// 1. 检查撤销服务依赖，构造每次请求的处理函数。
	if revokeDevice == nil {
		panic("设备及关联会话撤销处理函数需要撤销服务方法")
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

		// 3. 调用撤销服务，处理取消及目标与认证错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := revokeDevice(ctx, actor, input.DeviceID)
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

		// 4. 交付设备撤销成功响应。
		RevokeDeviceSuccess(requestContext, result)
	}
}
