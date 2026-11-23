package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterConfirmSyncSnapshotBaselineRoute 注册受 Bearer 认证保护的基线确认路由。
func RegisterConfirmSyncSnapshotBaselineRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	confirm func(context.Context, domain.Actor, uuid.UUID, string) (uint64, error),
) {
	// 1. 绑定共享 Bearer 授权和基线确认处理函数。
	if router == nil {
		panic("注册同步快照基线确认接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	confirmBaseline := handler.ConfirmSyncSnapshotBaseline(confirm)

	// 2. 注册 POST 路径，先授权，再处理确认请求。
	router.POST("/api/v1/sync/snapshots/:snapshot_id/confirm", protected, confirmBaseline)
}
