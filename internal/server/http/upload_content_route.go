package http

import (
	"context"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

const uploadContentRoutePath = "/api/v1/uploads/:id/content"

// RegisterUploadContentRoute 在 NewRouter 创建的路由器上注册内容上传。
func RegisterUploadContentRoute(
	router *gin.Engine,
	authorize func(context.Context, string) (*domain.Actor, error),
	getTask func(context.Context, domain.Actor, uint64) (*domain.UploadTask, error),
	receive func(context.Context, domain.Actor, *domain.UploadTask, io.Reader) (*domain.UploadTask, error),
) {
	// 1. 校验路由依赖，构造认证和内容接收处理函数。
	if router == nil {
		panic("注册上传内容接口需要路由器")
	}
	protected := middleware.RequireAuthorization(authorize, handler.AuthorizationFailure)
	upload := handler.UploadContent(getTask, receive)

	// 2. 挂载 PUT 路由，继承公共层的正文收尾和来源检查。
	router.PUT(uploadContentRoutePath, protected, upload)
}

func finishUploadContentBody(requestContext *gin.Context) {
	// 1. 只对已匹配的上传内容操作启用收尾。
	if requestContext.Request.Method == http.MethodPut && requestContext.FullPath() == uploadContentRoutePath {
		middleware.FinishUnreadUploadBody(requestContext)
		return
	}
	// 2. 让其他请求直接进入后续公共处理。
	requestContext.Next()
}
