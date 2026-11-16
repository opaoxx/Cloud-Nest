package http

import (
	"context"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterDirectoryPageRoute 注册目录列表路径，可追加一个全局分页服务。
// 单个 scope=all 进入全局查询，其余输入交由目录查询 DTO 校验。
func RegisterDirectoryPageRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	listPage func(context.Context, domain.Actor, uint64, string, int) (*domain.DirectoryPageResult, error),
	globalPages ...func(context.Context, domain.Actor, domain.GlobalEntryQuery, string) (*domain.GlobalEntryPageResult, error),
) {
	// 1. 检查依赖，构造认证及分页处理函数。
	if router == nil {
		panic("注册目录列表接口需要路由器")
	}
	if len(globalPages) > 1 {
		panic("列表路由最多绑定一个全局分页服务")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	directoryPage := handler.DirectoryPage(listPage)

	// 2. 识别单个 scope=all，选择全局或目录处理函数。
	entriesPage := directoryPage
	if len(globalPages) == 1 {
		globalPage := handler.GlobalEntryPage(globalPages[0])
		entriesPage = func(requestContext *gin.Context) {
			values, err := url.ParseQuery(requestContext.Request.URL.RawQuery)
			scopes := values["scope"]
			if err == nil && len(scopes) == 1 && scopes[0] == domain.GlobalEntryScopeAll {
				globalPage(requestContext)
				return
			}
			directoryPage(requestContext)
		}
	}

	// 3. 将认证和查询分派挂载到正式列表路径。
	router.GET("/api/v1/entries", protected, entriesPage)
}
