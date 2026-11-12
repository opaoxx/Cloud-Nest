package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
)

// RegisterLogoutRoute 使用已有授权、来源配置和退出服务注册正式退出路径。
func RegisterLogoutRoute(
	router *gin.Engine,
	allowedOrigins []string,
	authorize func(context.Context, string) (*domain.Actor, error),
	logout func(context.Context, domain.Actor) (*domain.LogoutResult, error),
	secureCookie bool,
) {
	// 1. 检查路由器，构造认证链和退出处理函数。
	if router == nil {
		panic("注册退出接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	policy := security.NewLoginSourcePolicy(allowedOrigins)
	logoutHandler := handler.Logout(policy, logout, secureCookie)

	// 2. 为正式退出路径挂载认证与处理函数。
	router.POST("/api/v1/auth/logout", protected, logoutHandler)
}
