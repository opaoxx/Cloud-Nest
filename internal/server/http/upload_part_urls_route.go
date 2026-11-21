package http

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterUploadPartURLsRoute 注册受 Bearer 授权保护的分片地址批量签发接口。
func RegisterUploadPartURLsRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	create func(context.Context, domain.Actor, uint64, []domain.UploadPartDeclaration, *time.Time) (*domain.UploadPartURLBatch, error),
) {
	// 1. 检查路由器，构造共享认证和签发处理函数。
	if router == nil {
		panic("注册分片地址批量签发接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	issue := handler.UploadPartURLs(create)

	// 2. 在正式 POST 路径上依次执行认证和签发处理。
	router.POST("/api/v1/uploads/:id/part-urls", protected, issue)
}
