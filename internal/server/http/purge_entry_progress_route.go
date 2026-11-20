package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterPurgeEntryProgressRoute 注册受 Bearer 认证保护的永久清理进度路由。
func RegisterPurgeEntryProgressRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	getProgress func(context.Context, domain.Actor, uint64) (*domain.PurgeProgress, error),
) {
	// 1. 绑定共享 Bearer 认证和只读进度处理器。
	if router == nil {
		panic("注册永久清理进度接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	purgeProgress := handler.PurgeEntryProgress(getProgress)

	// 2. 在正式 GET 路径上依次执行认证和进度查询。
	router.GET("/api/v1/trash/:id/purge", protected, purgeProgress)
}
