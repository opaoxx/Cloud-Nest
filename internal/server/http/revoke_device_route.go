package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterRevokeDeviceRoute 使用已有授权及撤销服务注册正式设备撤销路径。
func RegisterRevokeDeviceRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	revokeDevice func(context.Context, domain.Actor, uint64) (*domain.RevokeDeviceResult, error),
) {
	// 1. 检查路由器，构造认证链和设备撤销处理函数。
	if router == nil {
		panic("注册设备撤销接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	revoke := handler.RevokeDevice(revokeDevice)

	// 2. 为正式设备撤销路径挂载认证与处理函数。
	router.DELETE("/api/v1/devices/:id", protected, revoke)
}
