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
// 调用方提供日志、已校验的来源配置和就绪回调，路由器不持有具体依赖客户端。
func NewRouter(logger *slog.Logger, allowedOrigins []string, checkReadiness func(context.Context) bool) *gin.Engine {
	// 1. 创建空路由器，显式配置公共中间件，避免重复的默认访问日志。
	router := gin.New()
	// 禁止路由匹配阶段直接跳转，让尾斜杠请求也经过来源检查和统一 404。
	router.RedirectTrailingSlash = false

	// 2. 先分配 ID，再记录处理结果；Recovery 在内层处理异常且不转储原始请求。
	router.Use(
		middleware.RequestID(), middleware.RequestLog(logger), middleware.Recovery(handler.InternalError),
		// 上传正文收尾覆盖来源拒绝及其后的认证和接收响应。
		finishUploadContentBody,
		middleware.CORS(allowedOrigins, handler.CORSForbidden),
	)

	// 3. 注册系统探测接口，就绪检查绑定调用方提供的检查能力。
	router.GET("/ping", handler.Ping)
	router.GET("/health/live", handler.Live)
	router.GET("/health/ready", handler.Ready(checkReadiness))

	// 4. 接管未匹配请求，继续沿用前面的请求 ID 和完成日志中间件。
	router.NoRoute(handler.NotFound)

	// 5. 返回路由器，交给启动入口监听。
	return router
}
