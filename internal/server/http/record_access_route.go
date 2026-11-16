package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterRecordAccessRoute 使用共享授权及访问服务注册正式访问记录路径。
func RegisterRecordAccessRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	recordAccess func(context.Context, domain.Actor, uint64, string) (*domain.EntryPreference, error),
) {
	// 1. 检查路由依赖，构造认证和访问处理链。
	if router == nil {
		panic("注册访问记录接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	record := handler.RecordAccess(recordAccess)

	// 2. 注册正式 POST 访问记录路径。
	router.POST("/api/v1/entries/:id/access", protected, record)
}
