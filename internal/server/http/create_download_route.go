package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterCreateDownloadRoute 注册受 Bearer 授权保护的文件下载授权接口。
func RegisterCreateDownloadRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	create func(context.Context, domain.Actor, uint64, uint64, string) (*domain.EntryDownloadAuthorization, error),
) {
	// 1. 装配共享授权中间件，绑定下载授权 Handler。
	if router == nil {
		panic("注册下载授权接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	createDownload := handler.CreateDownload(create)

	// 2. 在正式 POST 路径上依次执行授权与下载处理。
	router.POST("/api/v1/entries/:id/download", protected, createDownload)
}
