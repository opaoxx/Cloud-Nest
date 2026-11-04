// Package http 负责服务端路由和 HTTP 层的装配。
package http

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// NewRouter 创建探测、存活与就绪检查路由，不启动监听。
// checkReadiness 由调用方提供，路由器不持有数据库或存储客户端。
func NewRouter(checkReadiness func(context.Context) bool) *gin.Engine {
	// 1. 创建带日志和异常恢复中间件的路由器。
	router := gin.Default()

	// 2. 在注册路由前启用请求 ID，让后续处理共享同一个标识。
	router.Use(middleware.RequestID())

	// 3. 注册系统探测接口，就绪检查绑定调用方提供的检查能力。
	router.GET("/ping", handler.Ping)
	router.GET("/health/live", handler.Live)
	router.GET("/health/ready", handler.Ready(checkReadiness))

	// 4. 返回路由器，交给启动入口监听。
	return router
}
