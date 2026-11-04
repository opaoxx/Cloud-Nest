package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func main() {
	// 1. 将退出信号转换为取消通知，让启动检查和 HTTP 服务共用。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 2. 初始化日志并执行启动流程，普通取消正常退出，真实错误使用非零退出码。
	logger := logging.New(os.Stdout)
	if err := run(ctx, logger); err != nil {
		if errors.Is(err, context.Canceled) {
			logger.Info("服务启动已取消")
			return
		}
		os.Exit(1)
	}
	logger.Info("服务已退出")
}

// run 装配服务所需资源并启动 HTTP，返回前关闭已创建的数据库连接池。
// 各失败环节在此记录原因，main 在清理完成后设置进程退出码。
func run(ctx context.Context, logger *slog.Logger) error {
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

	// 3. 读取存储配置，只记录地址与桶等非凭证字段。
	storageConfig, err := config.LoadStorageConfig()
	if err != nil {
		logger.Error("读取对象存储配置失败", "stage", "config", "error", err)
		return err
	}
	logger.Info("对象存储配置读取成功", "provider", storageConfig.Provider, "endpoint", storageConfig.EndpointURL.String(), "public_endpoint", storageConfig.PublicEndpointURL.String(), "bucket", storageConfig.Bucket)

	// 4. 建立连接池并检查连通性，成功后登记退出此方法时的关闭操作。
	connectionPool, err := repository.OpenMySQL(ctx, databaseConfig)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("MySQL 连接失败", "stage", "database", "address", databaseConfig.Address, "database", databaseConfig.DatabaseName, "error", err)
		}
		return err
	}
	defer func() {
		if err := connectionPool.Close(); err != nil {
			logger.Error("关闭 MySQL 连接池失败", "stage", "database", "error", err)
			return
		}
		logger.Info("MySQL 连接池已关闭")
	}()
	logger.Info("MySQL 连接检查通过", "address", databaseConfig.Address, "database", databaseConfig.DatabaseName)

	// 5. 只读检查迁移版本、状态和摘要，不兼容时返回并关闭连接池。
	if err := repository.CheckSchema(ctx, connectionPool); err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("数据库迁移检查失败", "stage", "schema", "database", databaseConfig.DatabaseName, "error", err)
		}
		return err
	}
	logger.Info("数据库迁移检查通过", "database", databaseConfig.DatabaseName)

	// 6. 创建存储客户端并只读检查已有桶，使用结束后释放空闲连接。
	storageClient, err := storage.NewClient(ctx, storageConfig)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("MinIO 桶访问检查失败", "stage", "storage", "bucket", storageConfig.Bucket, "error", err)
		}
		return err
	}
	defer func() {
		storageClient.Close()
		logger.Info("MinIO 空闲连接已释放")
	}()
	logger.Info("MinIO 桶访问检查通过", "bucket", storageConfig.Bucket)

	// 7. 由 HTTP 层创建路由，启动入口只负责装配与监听。
	router := serverhttp.NewRouter()
	httpServer := &http.Server{
		Addr:              listenAddress,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// 8. 启动 HTTP 并处理退出通知；返回后依次清理存储和数据库连接。
	logger.Info("准备启动 HTTP 服务", "address", listenAddress)
	if err := serverhttp.Serve(ctx, httpServer, 30*time.Second, logger); err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("HTTP 服务运行或退出失败", "stage", "http", "address", listenAddress, "error", err)
		}
		return err
	}
	return nil
}
