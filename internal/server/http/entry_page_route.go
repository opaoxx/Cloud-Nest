package http

import (
	"context"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterEntryPageRoute 装配目录、全局、最近使用及可选名称升序分页入口。
func RegisterEntryPageRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	directoryPages func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error),
	globalPages func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.GlobalEntryPageResult, error),
	recentPages func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.RecentEntryPageResult, error),
	namePages ...func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.NameEntryPageResult, error),
) {
	// 1. 检查依赖并绑定分页处理函数。
	if router == nil {
		panic("注册列表接口需要路由器")
	}
	directoryPage := handler.DirectoryPage(directoryPages)
	globalPage := handler.GlobalEntryPage(globalPages)
	recentPage := handler.RecentEntryPage(recentPages)
	// 2. 可选绑定名称升序分页，检查单个服务依赖。
	if len(namePages) > 1 {
		panic("列表路由最多绑定一个名称分页服务")
	}
	var namePage gin.HandlerFunc
	if len(namePages) == 1 {
		namePage = handler.NameEntryPage(namePages[0])
	}

	// 3. 注册认证后的统一查询分派。
	registerEntryPageRoute(router, authorize, directoryPage, globalPage, recentPage, namePage, nil)
}

// registerEntryPageRoute 复用认证和查询分派，缺省处理能力由目录 DTO 校验输入。
func registerEntryPageRoute(router *gin.Engine, authorize func(context.Context, string) (*domain.Actor, error), directoryPage, globalPage, recentPage, namePage, sizePage gin.HandlerFunc) {
	// 1. 先构造认证中间件，再识别完整查询中的单个范围。
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	entriesPage := func(requestContext *gin.Context) {
		values, err := url.ParseQuery(requestContext.Request.URL.RawQuery)
		scopes := values["scope"]

		// 2. 按范围调用对应处理函数，目录输入由目录 DTO 校验。
		if err == nil && len(scopes) == 1 {
			switch scopes[0] {
			case domain.GlobalEntryScopeAll, domain.GlobalEntryScopeStarred:
				// 1. 单个名称或大小排序进入对应处理，其余排序由全局 DTO 校验。
				sorts := values["sort"]
				if namePage != nil && len(sorts) == 1 && sorts[0] == domain.GlobalEntrySortNameAsc {
					namePage(requestContext)
					return
				}
				if sizePage != nil && len(sorts) == 1 && sorts[0] == domain.GlobalEntrySortSizeDesc {
					sizePage(requestContext)
					return
				}
				if globalPage != nil {
					globalPage(requestContext)
					return
				}
			case domain.GlobalEntryScopeRecent:
				if recentPage != nil {
					recentPage(requestContext)
					return
				}
			}
		}
		directoryPage(requestContext)
	}

	// 3. 把认证及查询分派注册到正式列表路径。
	router.GET("/api/v1/entries", protected, entriesPage)
}
