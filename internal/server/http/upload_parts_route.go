package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterUploadPartsRoute 注册受 Bearer 授权保护的只读分片回执查询接口。
func RegisterUploadPartsRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	list func(context.Context, domain.Actor, uint64) (*domain.UploadPartReceiptList, error),
) {
	// 1. 检查路由器，装配共享认证和只读回执处理器。
	if router == nil {
		panic("注册分片回执查询接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	listParts := handler.UploadParts(list)

	// 2. 在正式 GET 路径上依次执行认证和回执查询。
	router.GET("/api/v1/uploads/:id/parts", protected, listParts)
}
