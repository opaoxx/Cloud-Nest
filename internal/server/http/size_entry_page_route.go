package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
)

// RegisterSizeEntryPageRoute 装配目录、修改时间、最近、名称和大小五类分页入口。
func RegisterSizeEntryPageRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	directoryPages func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error),
	globalPages func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.GlobalEntryPageResult, error),
	recentPages func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.RecentEntryPageResult, error),
	namePages func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.NameEntryPageResult, error),
	sizePages func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.SizeEntryPageResult, error),
) {
	// 1. 检查并绑定五类分页服务。
	if router == nil {
		panic("注册大小列表接口需要路由器")
	}
	directoryPage := handler.DirectoryPage(directoryPages)
	globalPage := handler.GlobalEntryPage(globalPages)
	recentPage := handler.RecentEntryPage(recentPages)
	namePage := handler.NameEntryPage(namePages)
	sizePage := handler.SizeEntryPage(sizePages)

	// 2. 复用认证及统一查询分派，绑定大小排序能力。
	registerEntryPageRoute(router, authorize, directoryPage, globalPage, recentPage, namePage, sizePage)
}
