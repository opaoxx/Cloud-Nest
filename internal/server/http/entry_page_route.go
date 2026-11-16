package http

import (
	"context"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterEntryPageRoute 装配目录、全局与最近使用三类分页的正式列表入口。
func RegisterEntryPageRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	directoryPages func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error),
	globalPages func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.GlobalEntryPageResult, error),
	recentPages func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.RecentEntryPageResult, error),
) {
	// 1. 检查依赖并绑定三类处理函数。
	if router == nil {
		panic("注册列表接口需要路由器")
	}
	directoryPage := handler.DirectoryPage(directoryPages)
	globalPage := handler.GlobalEntryPage(globalPages)
	recentPage := handler.RecentEntryPage(recentPages)

	// 2. 注册认证后的统一查询分派。
	registerEntryPageRoute(router, authorize, directoryPage, globalPage, recentPage)
}

// registerEntryPageRoute 复用认证和查询分派，缺省处理能力由目录 DTO 校验输入。
func registerEntryPageRoute(router *gin.Engine, authorize func(context.Context, string) (*domain.Actor, error), directoryPage, globalPage, recentPage gin.HandlerFunc) {
	// 1. 先构造认证中间件，再识别完整查询中的单个范围。
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	entriesPage := func(requestContext *gin.Context) {
		values, err := url.ParseQuery(requestContext.Request.URL.RawQuery)
		scopes := values["scope"]

		// 2. 按范围调用对应处理函数，目录输入由目录 DTO 校验。
		if err == nil && len(scopes) == 1 {
			switch scopes[0] {
			case domain.GlobalEntryScopeAll, domain.GlobalEntryScopeStarred:
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
