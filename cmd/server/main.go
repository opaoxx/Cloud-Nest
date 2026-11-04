package main

import (
	"os"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func main() {
	// 1. 初始化固定格式的启动日志，让配置错误也能以相同格式输出。
	logger := logging.New(os.Stdout)

	// 2. 读取并校验监听配置，失败时记录原因并以非零状态码退出。
	listenAddress, err := config.LoadHTTPAddress()
	if err != nil {
		logger.Error("读取 HTTP 监听配置失败", "stage", "config", "error", err)
		os.Exit(1)
	}
	logger.Info("HTTP 监听配置读取成功", "address", listenAddress)

	// 3. 创建路由，注册用于确认服务可用的 /ping 接口。
	router := gin.Default()

	router.GET("/ping", func(requestContext *gin.Context) {
		requestContext.JSON(200, gin.H{
			"message": "CloudNest API is running",
		})
	})

	// 4. 记录监听尝试；实际启动失败时记录地址和原因，再结束进程。
	logger.Info("准备启动 HTTP 服务", "address", listenAddress)
	if err := router.Run(listenAddress); err != nil {
		logger.Error("HTTP 服务启动失败", "stage", "http", "address", listenAddress, "error", err)
		os.Exit(1)
	}
}
