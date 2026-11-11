package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterCurrentUserRoute 将当前用户接口接入已配置公共中间件的路由器。
// authorize 和 getProfile 分别绑定授权服务与资料服务的方法。
func RegisterCurrentUserRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	getProfile func(context.Context, uint64) (*domain.UserProfile, error),
) {
	// 1. 检查路由器，构造认证及当前用户处理函数。
	if router == nil {
		panic("注册当前用户接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	currentUser := handler.CurrentUser(getProfile)

	// 2. 仅为当前用户路径挂载这条认证处理链。
	router.GET("/api/v1/me", protected, currentUser)
}
