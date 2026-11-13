package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterRenameDeviceRoute 使用已有授权及改名服务注册正式设备改名路径。
func RegisterRenameDeviceRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	renameDevice func(context.Context, domain.Actor, uint64, string) (*domain.UserDevice, error),
) {
	// 1. 检查路由器，构造认证链和设备改名处理函数。
	if router == nil {
		panic("注册设备改名接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	rename := handler.RenameDevice(renameDevice)

	// 2. 为正式设备改名路径挂载认证与处理函数。
	router.PATCH("/api/v1/devices/:id", protected, rename)
}
