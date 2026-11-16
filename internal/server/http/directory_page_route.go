package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
)

// RegisterDirectoryPageRoute 注册目录列表路径，可追加一个全局分页服务。
// 单个 all/starred 进入全局分页处理，其余输入交由目录查询 DTO 校验。
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
	directoryPage := handler.DirectoryPage(listPage)
	var globalPage gin.HandlerFunc
	if len(globalPages) == 1 {
		globalPage = handler.GlobalEntryPage(globalPages[0])
	}

	// 2. 复用正式列表的认证与查询分派。
	registerEntryPageRoute(router, authorize, directoryPage, globalPage, nil, nil)
}
