package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterPurgePreviewRoute 注册受 Bearer 授权保护的永久清理预检路由。
func RegisterPurgePreviewRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	preview func(context.Context, domain.Actor, uint64) (*domain.PurgePreview, error),
) {
	// 1. 绑定共享 Bearer 认证和永久清理预检处理函数。
	if router == nil {
		panic("注册永久清理预检接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	purgePreview := handler.PurgePreview(preview)

	// 2. 在正式 GET 路径上依次执行认证和预检处理。
	router.GET("/api/v1/trash/:id/purge-preview", protected, purgePreview)
}
