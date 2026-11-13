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
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
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

	// 4. 读取并校验前端来源白名单，随后显式传给路由器。
	allowedOrigins, err := config.LoadAllowedOrigins()
	if err != nil {
		logger.Error("读取前端来源白名单失败", "stage", "config", "error", err)
		return err
	}
	logger.Info("前端来源白名单读取成功", "allowed_origins", allowedOrigins)

	// 5. 加载环境密钥或本地持久化 JWT 配置。
	jwtConfig, err := config.LoadServerJWTConfig()
	if err != nil {
		logger.Error("读取 JWT 配置失败", "stage", "config", "error", err)
		return err
	}
	logger.Info("JWT 配置读取成功", "issuer", jwtConfig.Issuer, "audience", jwtConfig.Audience)
	secureCookie, err := config.LoadRefreshCookieSecure()
	if err != nil {
		logger.Error("读取刷新 Cookie 配置失败", "stage", "config", "error", err)
		return err
	}

	// 6. 建立连接池并检查连通性，成功后登记退出此方法时的关闭操作。
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

	// 7. 只读检查迁移版本、状态和摘要，不兼容时返回并关闭连接池。
	if err := repository.CheckSchema(ctx, connectionPool); err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("数据库迁移检查失败", "stage", "schema", "database", databaseConfig.DatabaseName, "error", err)
		}
		return err
	}
	logger.Info("数据库迁移检查通过", "database", databaseConfig.DatabaseName)

	// 8. 创建存储客户端并只读检查已有桶，使用结束后释放空闲连接。
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

	// 9. 复用连接池，构造令牌校验、请求授权、用户资料、会话及设备列表服务。
	store, err := repository.NewStore(connectionPool)
	if err != nil {
		logger.Error("装配数据访问入口失败", "stage", "application", "error", err)
		return err
	}
	verifier, err := service.NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		logger.Error("装配访问令牌校验器失败", "stage", "application", "error", err)
		return err
	}
	authorizationService := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	profileService := service.NewUserProfileService(store.FindUserProfile)
	sessionListService := service.NewUserSessionService(store.ListUserSessions)
	deviceListService := service.NewUserDeviceService(store.ListUserDevices)

	// 10. 装配真实凭证、共享失败限流及登录会话、令牌服务。
	credentialService := service.NewCredentialService(store.FindLoginAccountByUsername)
	loginLimiter := service.NewLoginFailureLimiter()
	loginGate := security.NewLoginAttemptGate(loginLimiter.Begin, credentialService.Authenticate)
	signer, err := service.NewAccessTokenSigner(jwtConfig)
	if err != nil {
		logger.Error("装配访问令牌签发器失败", "stage", "application", "error", err)
		return err
	}
	sessionService, err := service.NewLoginSessionService(jwtConfig,
		func(ctx context.Context, userID uint64, action func(context.Context, service.LoginSessionTransaction) error) error {
			return store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		})
	if err != nil {
		logger.Error("装配登录会话服务失败", "stage", "application", "error", err)
		return err
	}
	loginTokens := service.NewLoginTokenService(sessionService.Create, signer.Issue)

	// 11. 复用已有 Store、刷新用户事务和签发器，装配刷新凭证服务。
	refreshCredentials := service.NewRefreshCredentialService(store.FindRefreshSession)
	refreshSessions := service.NewRefreshSessionService(refreshCredentials.Verify,
		func(ctx context.Context, verified domain.VerifiedRefreshSession, action func(context.Context, service.RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
			return store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *repository.Store, current domain.VerifiedRefreshSession) error {
				return action(txContext, txStore, current)
			})
		})
	refreshTokens := service.NewRefreshTokenService(refreshSessions.Refresh, signer.Issue)

	// 12. 复用已认证用户事务，装配当前会话退出及指定会话撤销服务。
	logoutService := service.NewLogoutService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.LogoutTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		})
	revokeService := service.NewRevokeSessionService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RevokeSessionTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		})

	// 13. 注册系统检查、当前用户、会话及设备列表、会话撤销、登录、刷新与退出接口。
	healthService := service.NewHealthService(connectionPool.PingContext, storageClient.CheckBucket)
	router := serverhttp.NewRouter(logger, allowedOrigins, healthService.Ready)
	serverhttp.RegisterCurrentUserRoute(router, authorizationService.Authorize, profileService.Get)
	serverhttp.RegisterUserSessionsRoute(router, authorizationService.Authorize, sessionListService.List)
	serverhttp.RegisterUserDevicesRoute(router, authorizationService.Authorize, deviceListService.List)
	serverhttp.RegisterRevokeSessionRoute(router, authorizationService.Authorize, revokeService.Revoke)
	serverhttp.RegisterLoginRoute(router, allowedOrigins, loginGate.Authenticate, loginTokens.Create, profileService.Get, secureCookie)
	serverhttp.RegisterRefreshRoute(router, allowedOrigins, refreshTokens.Refresh, secureCookie)
	serverhttp.RegisterLogoutRoute(router, allowedOrigins, authorizationService.Authorize, logoutService.Logout, secureCookie)
	httpServer := &http.Server{
		Addr:              listenAddress,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// 14. 启动 HTTP 并处理退出通知；返回后依次清理存储和数据库连接。
	logger.Info("准备启动 HTTP 服务", "address", listenAddress)
	if err := serverhttp.Serve(ctx, httpServer, 30*time.Second, logger); err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("HTTP 服务运行或退出失败", "stage", "http", "address", listenAddress, "error", err)
		}
		return err
	}
	return nil
}
