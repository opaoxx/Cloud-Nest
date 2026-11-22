package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterSyncSnapshotPageRoute 注册受 Bearer 认证保护的一致性快照续页路由。
func RegisterSyncSnapshotPageRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	readPage func(context.Context, domain.Actor, uuid.UUID, string) (*domain.SyncSnapshotPageResult, error),
) {
	// 1. 绑定共享 Bearer 认证和快照续页处理函数。
	if router == nil {
		panic("注册同步快照续页接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	snapshotPage := handler.SyncSnapshotPage(readPage)

	// 2. 注册 GET 路径，按认证、续页处理的顺序执行。
	router.GET("/api/v1/sync/snapshots/:snapshot_id", protected, snapshotPage)
}
