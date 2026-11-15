package http

import (
	"context"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterCreateDirectoryRoute 使用共享授权与幂等创建服务注册正式创建路径。
func RegisterCreateDirectoryRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	create func(context.Context, domain.Actor, uint64, string, *string, string) (json.RawMessage, error),
) {
	// 1. 检查路由器，构造认证链和创建处理函数。
	if router == nil {
		panic("注册创建文件夹接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	createDirectory := handler.CreateDirectory(create)

	// 2. 为正式创建路径挂载认证与处理函数。
	router.POST("/api/v1/directories", protected, createDirectory)
}
