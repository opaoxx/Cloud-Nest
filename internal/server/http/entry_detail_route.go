package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterEntryDetailRoute 使用已有授权和详情服务注册正式目录项详情路径。
func RegisterEntryDetailRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	getDetail func(context.Context, domain.Actor, uint64) (*domain.EntryDetail, error),
) {
	// 1. 检查路由器，构造认证链和详情处理函数。
	if router == nil {
		panic("注册目录项详情接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	detail := handler.EntryDetail(getDetail)

	// 2. 为正式目录项详情路径挂载认证及处理函数。
	router.GET("/api/v1/entries/:id", protected, detail)
}
