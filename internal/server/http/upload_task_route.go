package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// RegisterUploadTaskRoute 使用已有授权与任务读取服务注册正式查询路径。
func RegisterUploadTaskRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	getTask func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error),
) {
	// 1. 检查路由器，构造认证链和任务查询处理函数。
	if router == nil {
		panic("注册上传任务查询接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	query := handler.UploadTask(getTask)

	// 2. 为正式上传任务路径挂载认证及处理函数。
	router.GET("/api/v1/uploads/:id", protected, query)
}
