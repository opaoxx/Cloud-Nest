package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterUserSessionsRoute 使用已有授权和列表服务注册正式会话列表路径。
func RegisterUserSessionsRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	listSessions func(context.Context, domain.Actor, uint64, int) (*domain.UserSessionList, error),
) {
	// 1. 检查路由器，构造认证链和列表处理函数。
	if router == nil {
		panic("注册会话列表接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	userSessions := handler.UserSessions(listSessions)

	// 2. 为正式会话列表路径挂载认证及处理函数。
	router.GET("/api/v1/sessions", protected, userSessions)
}
