package http

import (
	"context"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterTrashEntryRoute 注册受 Bearer 授权保护的单项回收路由。
func RegisterTrashEntryRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	trash func(context.Context, domain.Actor, uint64, uint64, string) (json.RawMessage, error),
) {
	// 1. 绑定共享授权中间件和回收 Handler。
	if router == nil {
		panic("注册目录项回收接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	trashEntry := handler.TrashEntry(trash)

	// 2. 在正式 POST 路径上依次执行授权与回收处理。
	router.POST("/api/v1/entries/:id/trash", protected, trashEntry)
}
