package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

// UserDevices 绑定 UserDeviceService.List，路由须先挂载认证中间件。
func UserDevices(
	listDevices func(context.Context, domain.Actor, uint64, int) (*domain.UserDeviceList, error),
) gin.HandlerFunc {
	// 1. 确认列表服务已经提供，再构造每次请求的处理函数。
	if listDevices == nil {
		panic("用户设备处理函数需要列表服务方法")
	}
	return func(requestContext *gin.Context) {
		// 1. 检查响应状态、请求取消和已认证 Actor。
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

		// 2. 解析设备列表查询参数。
		input, fieldError := dto.ParseUserDeviceListQuery(requestContext.Request.URL.RawQuery)
		if fieldError != nil {
			InvalidArgument(requestContext, fieldError)
			return
		}

		// 3. 调用列表服务，处理取消与服务错误。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		result, err := listDevices(ctx, actor, input.BeforeDeviceID, input.PageSize)
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		if err != nil {
			AuthorizationFailure(requestContext, err)
			return
		}

		// 4. 核对归属，转换并预先编码响应。
		if result == nil {
			InternalError(requestContext, nil)
			return
		}
		for _, device := range result.Devices {
			if device.UserID != actor.UserID {
				InternalError(requestContext, nil)
				return
			}
		}
		data, err := dto.NewUserDeviceListData(result)
		if err != nil {
			InternalError(requestContext, nil)
			return
		}
		responseBytes, err := json.Marshal(dto.UserDeviceListResponse{Data: *data, RequestID: requestcontext.RequestID(ctx)})
		if err != nil {
			InternalError(requestContext, nil)
			return
		}

		// 5. 复查取消，交付列表 JSON。
		if ctx.Err() != nil {
			InternalError(requestContext, nil)
			return
		}
		requestContext.Writer.Header().Del("Content-Length")
		requestContext.Writer.Header().Del("Content-Encoding")
		requestContext.Header("Content-Type", "application/json; charset=utf-8")
		requestContext.Abort()
		requestContext.Data(http.StatusOK, "application/json; charset=utf-8", responseBytes)
	}
}
