package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func main() {
	// 1. 初始化固定格式的启动日志，再执行可返回错误的启动流程。
	logger := logging.New(os.Stdout)
	if err := run(logger); err != nil {
		// 2. run 已记录错误并执行资源清理，此处只设置失败退出码。
		os.Exit(1)
	}
}

// run 装配服务所需资源并启动 HTTP，返回前关闭已创建的数据库连接池。
// 各失败环节在此记录原因，main 在清理完成后设置进程退出码。
func run(logger *slog.Logger) error {
	// 1. 读取并校验监听配置，失败时记录原因并停止启动。
	listenAddress, err := config.LoadHTTPAddress()
	if err != nil {
		logger.Error("读取 HTTP 监听配置失败", "stage", "config", "error", err)
		return err
	}
	logger.Info("HTTP 监听配置读取成功", "address", listenAddress)

	// 2. 读取数据库配置，只记录地址和库名，配置错误时停止启动。
	databaseConfig, err := config.LoadDatabaseConfig()
	if err != nil {
		logger.Error("读取 MySQL 连接配置失败", "stage", "config", "error", err)
		return err
	}
	logger.Info("MySQL 连接配置读取成功", "address", databaseConfig.Address, "database", databaseConfig.DatabaseName)

	// 3. 建立连接池并检查连通性，成功后登记退出此方法时的关闭操作。
	connectionPool, err := repository.OpenMySQL(context.Background(), databaseConfig)
	if err != nil {
		logger.Error("MySQL 连接失败", "stage", "database", "address", databaseConfig.Address, "database", databaseConfig.DatabaseName, "error", err)
		return err
	}
	defer func() {
		if err := connectionPool.Close(); err != nil {
			logger.Error("关闭 MySQL 连接池失败", "stage", "database", "error", err)
		}
	}()
	logger.Info("MySQL 连接检查通过", "address", databaseConfig.Address, "database", databaseConfig.DatabaseName)

	// 4. 创建路由，注册用于确认服务可用的 /ping 接口。
	router := gin.Default()

	router.GET("/ping", func(requestContext *gin.Context) {
		requestContext.JSON(200, gin.H{
			"message": "CloudNest API is running",
		})
	})

	// 5. 记录监听尝试；实际启动失败时记录地址和原因，再结束进程。
	logger.Info("准备启动 HTTP 服务", "address", listenAddress)
	if err := router.Run(listenAddress); err != nil {
		logger.Error("HTTP 服务启动失败", "stage", "http", "address", listenAddress, "error", err)
		return err
	}
	return nil
}
