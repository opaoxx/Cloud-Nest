// Package http 负责服务端路由和 HTTP 层的装配。
package http

import (
	"context"
	"log/slog"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
)

// NewRouter 创建探测、存活与就绪检查路由，不启动监听。
// logger 和 checkReadiness 由调用方提供，路由器不持有数据库或存储客户端。
func NewRouter(logger *slog.Logger, checkReadiness func(context.Context) bool) *gin.Engine {
	// 1. 创建空路由器，显式配置公共中间件，避免重复的默认访问日志。
	router := gin.New()

	// 2. 先分配 ID，再记录处理结果；Recovery 在内层处理异常且不转储原始请求。
	router.Use(middleware.RequestID(), middleware.RequestLog(logger), gin.RecoveryWithWriter(nil))

	// 3. 注册系统探测接口，就绪检查绑定调用方提供的检查能力。
	router.GET("/ping", handler.Ping)
	router.GET("/health/live", handler.Live)
	router.GET("/health/ready", handler.Ready(checkReadiness))

	// 4. 返回路由器，交给启动入口监听。
	return router
}
