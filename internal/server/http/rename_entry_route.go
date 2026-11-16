package http

import (
	"context"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterRenameEntryRoute 使用共享授权和幂等重命名服务注册正式路径。
func RegisterRenameEntryRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	rename func(context.Context, domain.Actor, uint64, uint64, string, string) (json.RawMessage, error),
) {
	// 1. 检查路由器，构造认证链和重命名处理函数。
	if router == nil {
		panic("注册目录项重命名接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	renameEntry := handler.RenameEntry(rename)

	// 2. 为正式重命名路径挂载认证与处理函数。
	router.PATCH("/api/v1/entries/:id", protected, renameEntry)
}
