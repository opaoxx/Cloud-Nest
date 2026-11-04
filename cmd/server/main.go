package main

import (
	"log"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/config"
)

func main() {
	// 1. 读取并校验监听配置，配置错误时给出原因并结束启动。
	listenAddress, err := config.LoadHTTPAddress()
	if err != nil {
		log.Fatalf("读取 HTTP 监听配置失败: %v", err)
	}

	// 2. 创建路由，注册用于确认服务可用的 /ping 接口。
	router := gin.Default()

	router.GET("/ping", func(requestContext *gin.Context) {
		requestContext.JSON(200, gin.H{
			"message": "CloudNest API is running",
		})
	})

	// 3. 使用校验通过的地址启动 HTTP 服务，并报告实际监听失败。
	log.Printf("Server starting on %s", listenAddress)
	if err := router.Run(listenAddress); err != nil {
		log.Fatal(err)
	}
}
