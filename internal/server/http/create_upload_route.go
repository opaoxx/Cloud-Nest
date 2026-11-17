package http

import (
	"context"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterCreateUploadRoute 使用共享授权和上传创建幂等服务注册正式入口。
func RegisterCreateUploadRoute(router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	create func(context.Context, domain.Actor, domain.CreateUploadInput, string) (json.RawMessage, error),
) {
	// 1. 检查路由依赖，构造认证与上传处理链。
	if router == nil {
		panic("注册上传创建接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	createUpload := handler.CreateUpload(create)

	// 2. 将处理链挂载到 POST /api/v1/uploads。
	router.POST("/api/v1/uploads", protected, createUpload)
}
