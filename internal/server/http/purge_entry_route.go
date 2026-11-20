package http

import (
	"context"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterPurgeEntryRoute 注册受 Bearer 认证保护的永久清理确认路由。
func RegisterPurgeEntryRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	beginPurge func(context.Context, domain.Actor, uint64, uint64, string, string) (json.RawMessage, error),
) {
	// 1. 绑定共享 Bearer 认证和清理确认处理函数。
	if router == nil {
		panic("注册永久清理确认接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	purgeEntry := handler.PurgeEntry(beginPurge)

	// 2. 在正式 DELETE 路径上依次执行认证和确认处理。
	router.DELETE("/api/v1/trash/:id", protected, purgeEntry)
}
