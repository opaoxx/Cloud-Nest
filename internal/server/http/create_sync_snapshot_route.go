package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterCreateSyncSnapshotRoute 注册受 Bearer 认证保护的一致性快照创建路由。
func RegisterCreateSyncSnapshotRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	create func(context.Context, domain.Actor, string, int) (*domain.SyncSnapshotPageResult, error),
) {
	// 1. 构造共享认证链和创建处理函数。
	if router == nil {
		panic("注册同步快照创建接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	createSnapshot := handler.CreateSyncSnapshot(create)

	// 2. 在正式 POST 路径上按认证、创建的顺序挂载。
	router.POST("/api/v1/sync/snapshots", protected, createSnapshot)
}
