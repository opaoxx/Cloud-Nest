package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterTrashPageRoute 注册回收站列表路由。
func RegisterTrashPageRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	listPage func(context.Context, domain.Actor, int, string) (*domain.TrashEntryPageResult, error),
) {
	// 1. 绑定共享 Bearer 认证和回收站分页处理函数。
	if router == nil {
		panic("注册回收站列表接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	trashPage := handler.TrashPage(listPage)

	// 2. 将两者按顺序挂到 GET /api/v1/trash。
	router.GET("/api/v1/trash", protected, trashPage)
}
