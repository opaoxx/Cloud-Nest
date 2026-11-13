package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterRevokeSessionRoute 使用已有授权及撤销服务注册正式指定会话撤销路径。
func RegisterRevokeSessionRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	revokeSession func(context.Context, domain.Actor, uint64) (*domain.RevokeSessionResult, error),
) {
	// 1. 检查路由器，构造认证链和撤销处理函数。
	if router == nil {
		panic("注册指定会话撤销接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	revoke := handler.RevokeSession(revokeSession)

	// 2. 为正式会话撤销路径挂载认证与处理函数。
	router.DELETE("/api/v1/sessions/:id", protected, revoke)
}
