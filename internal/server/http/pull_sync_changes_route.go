package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterPullSyncChangesRoute 注册受 Bearer 认证保护的增量拉取路由。
func RegisterPullSyncChangesRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	pull func(context.Context, domain.Actor, string, string, int) (*domain.SyncChangePullResult, error),
) {
	// 1. 绑定共享 Bearer 认证和增量拉取处理函数。
	if router == nil {
		panic("注册增量变更拉取接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	pullChanges := handler.PullSyncChanges(pull)

	// 2. 在正式 POST 路径上按认证、拉取处理的顺序注册。
	router.POST("/api/v1/sync/changes/pull", protected, pullChanges)
}
