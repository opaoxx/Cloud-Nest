// Package handler 接收 HTTP 请求并返回响应。
package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Ping 返回原有开发探测响应，方便本地工具确认服务可响应。
func Ping(requestContext *gin.Context) {
	// 1. 返回固定的探测消息，保持现有接口响应。
	requestContext.JSON(http.StatusOK, gin.H{
		"message": "CloudNest API is running",
	})
}

// Live 返回进程存活状态，不检查数据库或对象存储依赖。
func Live(requestContext *gin.Context) {
	// 1. 返回当前 HTTP 服务可响应的状态。
	requestContext.JSON(http.StatusOK, gin.H{
		"status": "ok",
	})
}

// Ready 将就绪检查函数绑定为 Gin 处理函数，每次请求都重新执行检查。
// 检查使用当前请求上下文，只返回状态，不向调用方暴露底层错误。
func Ready(checkReadiness func(context.Context) bool) gin.HandlerFunc {
	return func(requestContext *gin.Context) {
		// 1. 禁止缓存结果，让依赖恢复后下次请求能够重新检查。
		requestContext.Header("Cache-Control", "no-store")

		// 2. 调用服务层检查，失败、超时或取消时统一返回 503。
		if !checkReadiness(requestContext.Request.Context()) {
			requestContext.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
			return
		}

		// 3. 所有依赖检查通过时返回 200，表示当前就绪。
		requestContext.JSON(http.StatusOK, gin.H{"status": "ready"})
	}
}
