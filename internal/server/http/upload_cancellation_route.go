package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterUploadCancellationRoute 注册受 Bearer 授权保护的上传取消接口。
func RegisterUploadCancellationRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	beginCancellation func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error),
) {
	// 1. 使用现有授权服务构造认证中间件并绑定取消 Handler。
	if router == nil {
		panic("注册上传取消接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	cancelUpload := handler.UploadCancellation(beginCancellation)

	// 2. 为正式 POST 路径挂载认证和处理函数。
	router.POST("/api/v1/uploads/:id/cancel", protected, cancelUpload)
}
