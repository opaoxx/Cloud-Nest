package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterUserDevicesRoute 使用已有授权和列表服务注册正式设备列表路径。
func RegisterUserDevicesRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	listDevices func(context.Context, domain.Actor, uint64, int) (*domain.UserDeviceList, error),
) {
	// 1. 检查路由器，构造认证链和列表处理函数。
	if router == nil {
		panic("注册设备列表接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	userDevices := handler.UserDevices(listDevices)

	// 2. 为正式设备列表路径挂载认证及处理函数。
	router.GET("/api/v1/devices", protected, userDevices)
}
