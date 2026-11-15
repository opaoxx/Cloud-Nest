package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterDirectoryPageRoute 使用已有授权与分页服务注册正式目录列表路径。
func RegisterDirectoryPageRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	listPage func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error),
) {
	// 1. 检查路由器，构造认证链和目录分页处理函数。
	if router == nil {
		panic("注册目录列表接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	directoryPage := handler.DirectoryPage(listPage)

	// 2. 为正式目录列表路径挂载认证及处理函数。
	router.GET("/api/v1/entries", protected, directoryPage)
}
