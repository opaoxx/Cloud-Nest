package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterConfirmSyncChangesRoute 注册受 Bearer 认证保护的增量确认路由。
func RegisterConfirmSyncChangesRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	confirm func(context.Context, domain.Actor, string) (uint64, error),
) {
	// 1. 绑定共享 Bearer 认证和增量确认处理函数。
	if router == nil {
		panic("注册设备增量确认接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	confirmChanges := handler.ConfirmSyncChanges(confirm)

	// 2. 在正式 POST 路径上按认证、确认的顺序注册。
	router.POST("/api/v1/sync/changes/confirm", protected, confirmChanges)
}
