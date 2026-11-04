// Package http 负责服务端路由和 HTTP 层的装配。
package http

import (
	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
)

// NewRouter 创建注册了开发探测与存活检查接口的路由器，不启动监听。
// 创建路由器不需要数据库或存储，服务启动时的依赖检查由调用方负责。
func NewRouter() *gin.Engine {
	// 1. 创建带日志和异常恢复中间件的路由器。
	router := gin.Default()

	// 2. 注册 /ping 和 /health/live 的处理函数。
	router.GET("/ping", handler.Ping)
	router.GET("/health/live", handler.Live)

	// 3. 返回路由器，交给启动入口监听。
	return router
}
