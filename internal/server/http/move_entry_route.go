package http

import (
	"context"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterMoveEntryRoute 使用共享授权和幂等移动服务注册正式路径。
func RegisterMoveEntryRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	move func(context.Context, domain.Actor, uint64, uint64, uint64, string) (json.RawMessage, error),
) {
	// 1. 检查路由器，构造认证链和移动处理函数。
	if router == nil {
		panic("注册目录项移动接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	moveEntry := handler.MoveEntry(move)

	// 2. 为正式移动路径挂载认证与处理函数。
	router.POST("/api/v1/entries/:id/move", protected, moveEntry)
}
