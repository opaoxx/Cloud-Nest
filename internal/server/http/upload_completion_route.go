package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

const uploadCompletionRoutePath = "/api/v1/uploads/:id/complete"

// RegisterUploadCompletionRoute 注册受 Bearer 授权保护的单文件上传完成确认接口。
func RegisterUploadCompletionRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	confirmCompletion func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error),
) {
	// 1. 装配共享授权中间件，绑定完成确认 Handler。
	if router == nil {
		panic("注册上传完成确认接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	completeUpload := handler.UploadCompletion(confirmCompletion)

	// 2. 在正式 POST 路径上依次执行授权与完成确认。
	router.POST(uploadCompletionRoutePath, protected, completeUpload)
}

// RegisterUploadCompletionByModeRoute 注册受 Bearer 授权保护的双模式上传完成接口。
func RegisterUploadCompletionByModeRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	getTask func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error),
	completeMultipart func(context.Context, domain.Actor, uint64, func() ([]domain.UploadPartCompletionClaim, error)) (*domain.UploadTask, error),
) {
	// 1. 检查路由器，装配现有 Bearer 认证和双模式完成处理器。
	if router == nil {
		panic("注册双模式上传完成接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	completeUpload := handler.UploadCompletionByMode(getTask, completeMultipart)

	// 2. 在正式 POST 路径上按顺序挂载认证和完成处理。
	router.POST(uploadCompletionRoutePath, protected, completeUpload)
}
