// Package handler 接收 HTTP 请求并返回响应。
package handler

import (
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
