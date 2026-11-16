package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterSetStarredRoute 使用共享授权及收藏服务注册正式收藏路径。
func RegisterSetStarredRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	setStarred func(context.Context, domain.Actor, uint64, bool) (*domain.EntryPreference, error),
) {
	// 1. 检查路由器，构造认证链和收藏处理函数。
	if router == nil {
		panic("注册收藏接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	set := handler.SetStarred(setStarred)

	// 2. 为正式收藏路径挂载认证与处理函数。
	router.PUT("/api/v1/entries/:id/star", protected, set)
}
