package main

import (
	"context"
	"encoding/json"
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
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
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

// run 装配服务所需资源并启动 HTTP，返回前等待后台任务退出，再释放连接。
// 各失败环节在此记录原因，main 在清理完成后设置进程退出码。
func run(ctx context.Context, logger *slog.Logger) error {
	return runWithPurgeRootWorker(ctx, logger, startServerPurgeRootWorker)
}

// runWithPurgeRootWorker 在同一启动流程中绑定调用方提供的永久清理 worker。
func runWithPurgeRootWorker(
	ctx context.Context,
	logger *slog.Logger,
	startPurgeRoot func(context.Context, func(context.Context, int, int) (service.PurgeRootBatchResult, error), *slog.Logger) func(),
) error {
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

	// 5. 读取并校验上传临时目录配置。
	uploadTempConfig, err := config.LoadUploadTempConfig()
	if err != nil {
		logger.Error("读取上传临时目录配置失败", "stage", "config", "error", err)
		return err
	}

	// 6. 创建目录并验证读写，失败时结束启动。
	if err := storage.PrepareUploadTempDirectory(ctx, uploadTempConfig); err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("上传临时目录初始化失败", "stage", "upload_temp", "error", err)
		}
		return err
	}

	// 7. 记录检查通过，继续后续启动流程。
	logger.Info("上传临时目录检查通过", "directory", uploadTempConfig.Directory)

	// 8. 加载环境密钥或本地持久化 JWT 配置。
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

	// 9. 建立连接池并检查连通性，成功后登记退出此方法时的关闭操作。
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

	// 10. 只读检查迁移版本、状态和摘要，不兼容时返回并关闭连接池。
	if err := repository.CheckSchema(ctx, connectionPool); err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("数据库迁移检查失败", "stage", "schema", "database", databaseConfig.DatabaseName, "error", err)
		}
		return err
	}
	logger.Info("数据库迁移检查通过", "database", databaseConfig.DatabaseName)

	// 11. 创建存储客户端并只读检查已有桶，使用结束后释放空闲连接。
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

	// 12. 复用连接池，构造令牌校验、请求授权、用户资料、会话及设备列表服务。
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

	// 13. 装配真实凭证、共享失败限流及登录会话、令牌服务。
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

	// 14. 复用已有 Store、刷新用户事务和签发器，装配刷新凭证服务。
	refreshCredentials := service.NewRefreshCredentialService(store.FindRefreshSession)
	refreshSessions := service.NewRefreshSessionService(refreshCredentials.Verify,
		func(ctx context.Context, verified domain.VerifiedRefreshSession, action func(context.Context, service.RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
			return store.WithRefreshUserTransaction(ctx, verified, func(txContext context.Context, txStore *repository.Store, current domain.VerifiedRefreshSession) error {
				return action(txContext, txStore, current)
			})
		})
	refreshTokens := service.NewRefreshTokenService(refreshSessions.Refresh, signer.Issue)

	// 15. 复用已认证用户事务，装配退出、会话撤销、设备改名及撤销服务。
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
	renameService := service.NewRenameDeviceService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RenameDeviceTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		})

	revokeDeviceService := service.NewRevokeDeviceService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RevokeDeviceTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		})

	// 16. 复用目录快照入口与持久化签名配置，装配目录分页服务。
	directoryListService := service.NewDirectoryListService(
		func(ctx context.Context, action func(context.Context, service.DirectoryListReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *repository.Store) error {
				return action(snapshotContext, snapshotStore)
			})
		})
	directoryCursorSigner, err := service.NewDirectoryCursorSigner(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配目录游标签名器失败", "stage", "application", "error", err)
		return err
	}
	directoryCursorVerifier, err := service.NewDirectoryCursorVerifier(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配目录游标校验器失败", "stage", "application", "error", err)
		return err
	}
	directoryPageService := service.NewDirectoryPageService(directoryCursorVerifier.Verify, directoryListService.List, directoryCursorSigner.Sign)

	// 17. 复用只读快照与持久化主密钥，装配全局分页服务。
	// 1. 绑定同一请求快照中的候选和祖先读取。
	globalListService := service.NewGlobalEntryListService(
		func(ctx context.Context, action func(context.Context, service.GlobalEntryListReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *repository.Store) error {
				return action(snapshotContext, snapshotStore)
			})
		})

	// 2. 派生全局用途的游标签名及校验能力。
	globalCursorSigner, err := service.NewGlobalEntryCursorSigner(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配全局游标签名器失败", "stage", "application", "error", err)
		return err
	}
	globalCursorVerifier, err := service.NewGlobalEntryCursorVerifier(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配全局游标校验器失败", "stage", "application", "error", err)
		return err
	}

	// 3. 组合文本游标分页入口。
	globalPageService := service.NewGlobalEntryPageService(globalCursorVerifier.Verify, globalListService.List, globalCursorSigner.Sign)

	// 1. 绑定同一只读快照内的最近候选和祖先读取。
	recentListService := service.NewRecentEntryListService(
		func(ctx context.Context, action func(context.Context, service.RecentEntryListReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *repository.Store) error {
				return action(snapshotContext, snapshotStore)
			})
		})

	// 2. 使用持久化主密钥派生最近使用游标能力。
	recentCursorSigner, err := service.NewRecentEntryCursorSigner(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配最近使用游标签名器失败", "stage", "application", "error", err)
		return err
	}
	recentCursorVerifier, err := service.NewRecentEntryCursorVerifier(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配最近使用游标校验器失败", "stage", "application", "error", err)
		return err
	}

	// 3. 组合分页服务，注册认证后的查询分派。
	recentPageService := service.NewRecentEntryPageService(recentCursorVerifier.Verify, recentListService.List, recentCursorSigner.Sign)

	// 1. 绑定同一只读快照中的名称候选和祖先读取。
	nameListService := service.NewNameEntryListService(
		func(ctx context.Context, action func(context.Context, service.NameEntryListReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *repository.Store) error {
				return action(snapshotContext, snapshotStore)
			})
		})

	// 2. 使用持久化主密钥派生名称游标能力。
	nameCursorSigner, err := service.NewNameEntryCursorSigner(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配名称游标签名器失败", "stage", "application", "error", err)
		return err
	}
	nameCursorVerifier, err := service.NewNameEntryCursorVerifier(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配名称游标校验器失败", "stage", "application", "error", err)
		return err
	}

	// 3. 组合名称分页服务，交给正式列表路由。
	namePageService := service.NewNameEntryPageService(nameCursorVerifier.Verify, nameListService.List, nameCursorSigner.Sign)

	// 1. 绑定同一只读快照中的大小候选和祖先读取。
	sizeListService := service.NewSizeEntryListService(
		func(ctx context.Context, action func(context.Context, service.SizeEntryListReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *repository.Store) error {
				return action(snapshotContext, snapshotStore)
			})
		})

	// 2. 使用持久化主密钥派生大小游标能力。
	sizeCursorSigner, err := service.NewSizeEntryCursorSigner(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配大小游标签名器失败", "stage", "application", "error", err)
		return err
	}
	sizeCursorVerifier, err := service.NewSizeEntryCursorVerifier(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配大小游标校验器失败", "stage", "application", "error", err)
		return err
	}

	// 3. 组合大小分页服务，交给正式列表路由。
	sizePageService := service.NewSizeEntryPageService(sizeCursorVerifier.Verify, sizeListService.List, sizeCursorSigner.Sign)

	// 18. 复用目录快照入口，装配指定目录项详情服务。
	entryDetailService := service.NewEntryDetailService(
		func(ctx context.Context, action func(context.Context, service.EntryDetailReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *repository.Store) error {
				return action(snapshotContext, snapshotStore)
			})
		})

	// 19. 绑定用户事务与创建结果编解码，装配幂等创建服务。
	createDirectoryService := service.NewIdempotentCreateDirectoryService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentCreateDirectoryTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		},
		func(entry *domain.FileEntry, colorKey *string) (json.RawMessage, error) {
			data, err := dto.NewCreateDirectoryData(entry, colorKey)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body json.RawMessage) error {
			_, err := dto.RestoreCreateDirectoryData(body)
			return err
		},
	)

	// 20. 绑定用户事务与重命名结果编解码，装配幂等重命名服务。
	renameEntryService := service.NewIdempotentRenameEntryService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentRenameEntryTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		},
		func(detail *domain.EntryDetail) (json.RawMessage, error) {
			data, err := dto.NewRenameEntryData(detail)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body json.RawMessage) error {
			_, err := dto.RestoreRenameEntryData(body)
			return err
		},
	)

	// 21. 绑定用户事务和各自的快照编解码，装配幂等移动、回收与恢复服务。
	moveEntryService := service.NewIdempotentMoveEntryService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentMoveEntryTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		},
		func(detail *domain.EntryDetail) (json.RawMessage, error) {
			data, err := dto.NewMoveEntryData(detail)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body json.RawMessage) error {
			_, err := dto.RestoreMoveEntryData(body)
			return err
		},
	)

	trashEntryService := service.NewIdempotentTrashEntryService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentTrashEntryTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		},
		func(entry *domain.TrashedFileEntry) (json.RawMessage, error) {
			data, err := dto.NewTrashEntryData(entry)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body json.RawMessage) error {
			_, err := dto.RestoreTrashEntryData(body)
			return err
		},
	)

	restoreEntryService := service.NewIdempotentRestoreEntryService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentRestoreEntryTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		},
		func(entry *domain.FileEntry) (json.RawMessage, error) {
			data, err := dto.NewRestoreEntryData(entry)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body json.RawMessage) error {
			_, err := dto.DecodeRestoreEntryData(body)
			return err
		},
	)

	// 1. 使用目录项只读快照读取回收候选和原父路径。
	trashListService := service.NewTrashEntryListService(func(ctx context.Context, read func(context.Context, service.TrashEntryListReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *repository.Store) error {
			return read(snapshotContext, snapshotStore)
		})
	})

	// 2. 从已有签名配置派生回收站游标的专用密钥。
	trashCursorSigner, err := service.NewTrashEntryCursorSigner(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配回收站游标签发器失败", "stage", "application", "error", err)
		return err
	}
	trashCursorVerifier, err := service.NewTrashEntryCursorVerifier(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配回收站游标校验器失败", "stage", "application", "error", err)
		return err
	}
	// 3. 组合游标校验、快照列表和下一页签发。
	trashPageService := service.NewTrashEntryPageService(trashCursorVerifier.Verify, trashListService.List, trashCursorSigner.Sign)

	// 1. 用现有签名配置创建预检签发器。
	purgePreviewSigner, err := service.NewPurgePreviewSigner(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配永久清理预检签发器失败", "stage", "application", "error", err)
		return err
	}

	// 2. 绑定 MySQL 只读快照和应用时钟，组装预检服务。
	purgePreviewService := service.NewPurgePreviewService(func(ctx context.Context, read func(context.Context, service.PurgePreviewReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *repository.Store) error {
			return read(snapshotContext, snapshotStore)
		})
	}, purgePreviewSigner, time.Now)

	// 3. 用与预检签发器相同的主密钥创建凭证校验器。
	purgePreviewVerifier, err := service.NewPurgePreviewVerifier(jwtConfig.SigningKey)
	if err != nil {
		logger.Error("装配永久清理预检校验器失败", "stage", "application", "error", err)
		return err
	}

	// 4. 绑定用户事务、快照编解码和时钟，装配幂等清理确认。
	purgeEntryService := service.NewIdempotentPurgeEntryService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentPurgeEntryTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		},
		purgePreviewVerifier,
		func(entry *domain.FileEntry) (json.RawMessage, error) {
			data, err := dto.NewPurgeEntryAcceptedData(entry)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body []byte) (string, error) {
			data, err := dto.DecodePurgeEntryAcceptedData(body)
			if err != nil {
				return "", err
			}
			return data.EntryID, nil
		},
		time.Now,
	)

	// 5. 将进度服务绑定到已有的 MySQL 只读快照。
	purgeProgressService := service.NewPurgeProgressService(func(ctx context.Context, read func(context.Context, service.PurgeProgressReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *repository.Store) error {
			return read(snapshotContext, snapshotStore)
		})
	})

	// 22. 绑定当前用户事务，装配收藏核心服务。
	setStarredService := service.NewSetStarredService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.SetStarredTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
			return action(txContext, txStore)
		})
	})

	// 23. 绑定当前用户事务，装配访问记录核心服务。
	recordAccessService := service.NewRecordAccessService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RecordAccessTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
			return action(txContext, txStore)
		})
	})

	// 24. 绑定已有用户范围查询，装配上传任务读取服务。
	uploadTaskService := service.NewUploadTaskService(store.FindUserUploadTask)

	// 25. 复用存储配置、用户事务及稳定结果编解码，装配幂等上传创建服务。
	createUploadService := service.NewIdempotentCreateUploadService(storageConfig.Provider, storageConfig.Bucket,
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentCreateUploadTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		},
		func(task *domain.UploadTask) (json.RawMessage, error) {
			data, err := dto.NewCreateUploadData(task)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body json.RawMessage) error {
			_, err := dto.RestoreCreateUploadData(body)
			return err
		},
	)

	// 26. 用已检查的目录创建共享上传临时区。
	uploadSpace := service.NewUploadTempSpace()
	uploadFiles, err := service.NewUploadContentFileService(uploadTempConfig.Directory, uploadSpace)
	if err != nil {
		logger.Error("装配上传临时文件服务失败", "stage", "upload_temp", "error", err)
		return err
	}

	// 27. 绑定用户事务、租约准备和 MinIO 写入。
	prepareUploadContent := service.NewUploadContentPreparationService(storageConfig.Provider, storageConfig.Bucket,
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.UploadContentPreparationTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		},
	)
	receiveUploadContent := service.NewUploadContentReceiveService(uploadFiles, storageConfig.Provider, storageConfig.Bucket,
		prepareUploadContent.Prepare, storageClient.PutUploadContent,
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.UploadContentReceiveTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		},
	)

	// 分片签发使用同一份存储配置，显式续期复用账号事务。
	uploadPartTargets := service.NewUploadPartSigningTargetService(storageConfig.Provider, storageConfig.Bucket,
		store.FindUserUploadTaskForPartSigning, store.FindUserStoredObject)
	uploadPartURLs := service.NewUploadPartURLService(uploadPartTargets.Get, storageClient.SignUploadPartURL, store.RenewUserUploadPartExpiry)

	// 1. 用现有分片目标、MinIO 分页和仓储事务装配完成服务。
	const uploadReceiptPageSize = 1000
	listUploadParts := func(ctx context.Context, objectKey, uploadID string) ([]domain.UploadPartReceipt, error) {
		return storageClient.ListUploadParts(ctx, objectKey, uploadID, uploadReceiptPageSize)
	}
	uploadPartReceipts := service.NewUploadPartReceiptService(uploadPartTargets.GetForReceipts, listUploadParts)
	completionReceipts := service.NewUploadCompletionReceiptService(uploadPartTargets.GetForCompletion, listUploadParts)
	multipartCompletion := service.NewMultipartUploadCompletionService(store.FindUserUploadTaskForPartSigning,
		store.ClaimUserMultipartCompletionLease, completionReceipts.Verify, store.CommitUserMultipartCompletion, store.ReleaseUserMultipartCompletionLease)

	// 下载目标在只读快照内检查，签发后通过当前用户事务复查并记录访问。
	downloadTargets := service.NewEntryDownloadTargetService(func(ctx context.Context, read func(context.Context, service.EntryDownloadTargetReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *repository.Store) error {
			return read(snapshotContext, snapshotStore)
		})
	})
	downloads := service.NewEntryDownloadService(downloadTargets.Get, storageClient.CheckDownloadObject, storageClient.SignDownloadURL,
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.DownloadAccessTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		},
	)

	// 28. 注册业务接口，将上传流程及下载授权绑定到正式路由。
	healthService := service.NewHealthService(connectionPool.PingContext, storageClient.CheckBucket)
	router := serverhttp.NewRouter(logger, allowedOrigins, healthService.Ready)
	serverhttp.RegisterSizeEntryPageRoute(router, authorizationService.Authorize, directoryPageService.List, globalPageService.List, recentPageService.List, namePageService.List, sizePageService.List)
	serverhttp.RegisterEntryDetailRoute(router, authorizationService.Authorize, entryDetailService.Get)
	serverhttp.RegisterUploadTaskRoute(router, authorizationService.Authorize, uploadTaskService.Get)
	serverhttp.RegisterCreateUploadRoute(router, authorizationService.Authorize, createUploadService.Create)
	serverhttp.RegisterUploadPartURLsRoute(router, authorizationService.Authorize, uploadPartURLs.Create)
	serverhttp.RegisterUploadPartsRoute(router, authorizationService.Authorize, uploadPartReceipts.List)
	serverhttp.RegisterUploadContentRoute(router, authorizationService.Authorize, uploadTaskService.Get, receiveUploadContent.Receive)
	// 2. 在原完成路径注册双模式入口，复用 Bearer 认证。
	serverhttp.RegisterUploadCompletionByModeRoute(router, authorizationService.Authorize, uploadTaskService.Get, multipartCompletion.Complete)
	serverhttp.RegisterUploadCancellationRoute(router, authorizationService.Authorize, store.BeginUserUploadCancellation)
	serverhttp.RegisterCreateDirectoryRoute(router, authorizationService.Authorize, createDirectoryService.Create)
	serverhttp.RegisterRenameEntryRoute(router, authorizationService.Authorize, renameEntryService.Rename)
	serverhttp.RegisterMoveEntryRoute(router, authorizationService.Authorize, moveEntryService.Move)
	serverhttp.RegisterTrashEntryRoute(router, authorizationService.Authorize, trashEntryService.Trash)
	serverhttp.RegisterTrashPageRoute(router, authorizationService.Authorize, trashPageService.List)
	serverhttp.RegisterRestoreEntryRoute(router, authorizationService.Authorize, restoreEntryService.Restore)
	serverhttp.RegisterPurgePreviewRoute(router, authorizationService.Authorize, purgePreviewService.Preview)
	serverhttp.RegisterPurgeEntryRoute(router, authorizationService.Authorize, purgeEntryService.Begin)
	serverhttp.RegisterPurgeEntryProgressRoute(router, authorizationService.Authorize, purgeProgressService.Get)
	serverhttp.RegisterSetStarredRoute(router, authorizationService.Authorize, setStarredService.Set)
	serverhttp.RegisterRecordAccessRoute(router, authorizationService.Authorize, recordAccessService.Record)
	serverhttp.RegisterCreateDownloadRoute(router, authorizationService.Authorize, downloads.Create)
	serverhttp.RegisterCurrentUserRoute(router, authorizationService.Authorize, profileService.Get)
	serverhttp.RegisterUserSessionsRoute(router, authorizationService.Authorize, sessionListService.List)
	serverhttp.RegisterUserDevicesRoute(router, authorizationService.Authorize, deviceListService.List)
	serverhttp.RegisterRenameDeviceRoute(router, authorizationService.Authorize, renameService.Rename)
	serverhttp.RegisterRevokeDeviceRoute(router, authorizationService.Authorize, revokeDeviceService.Revoke)
	serverhttp.RegisterRevokeSessionRoute(router, authorizationService.Authorize, revokeService.Revoke)
	serverhttp.RegisterLoginRoute(router, allowedOrigins, loginGate.Authenticate, loginTokens.Create, profileService.Get, secureCookie)
	serverhttp.RegisterRefreshRoute(router, allowedOrigins, refreshTokens.Refresh, secureCookie)
	serverhttp.RegisterLogoutRoute(router, allowedOrigins, authorizationService.Authorize, logoutService.Logout, secureCookie)
	httpServer := &http.Server{
		Addr:              listenAddress,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// 29. 启动上传校验，退出时先停止并等待 worker，再清理存储和数据库连接。
	uploadVerificationBatch := newServerUploadVerificationBatch(store, storageClient)
	stopUploadVerification := startServerUploadVerificationWorker(ctx, uploadVerificationBatch.RunOnce, logger)
	defer stopUploadVerification()

	// 30. 启动单文件过期处理，退出时等待过期批次结束。
	uploadExpirationBatch := newServerUploadExpirationBatch(store)
	stopUploadExpiration := startServerUploadExpirationWorker(ctx, uploadExpirationBatch.RunOnce, logger)
	defer stopUploadExpiration()

	// 31. 启动单文件取消收尾，退出时等待取消 worker 停止。
	uploadCancellationBatch := newServerUploadCancellationBatch(store)
	stopUploadCancellation := startServerUploadCancellationWorker(ctx, uploadCancellationBatch.RunOnce, logger)
	defer stopUploadCancellation()

	// 32. 用真实 MySQL 扫描和结算启动永久清理，退出时等待清理事务结束。
	purgeRootBatch := service.NewPurgeRootBatch(store.ListPurgeRootCandidates, store.CompletePurgeLeafBatch)
	stopPurgeRoot := startPurgeRoot(ctx, purgeRootBatch.RunOnce, logger)
	defer stopPurgeRoot()

	// 33. 用已有 MySQL 仓储和 MinIO 客户端装配回收批次。
	objectDeletion := service.NewStoredObjectDeletionProcessor(storageClient.DeleteStoredObject, store.CompleteStoredObjectGC)
	objectDeletionRecovery := service.NewStoredObjectDeletionRecoveryBatch(store.ListDeletingStoredObjects, objectDeletion.Process)
	objectGCCandidate := service.NewStoredObjectGCCandidateProcessor(store.ClaimStoredObjectGC, objectDeletion.Process)
	objectGCBatch := service.NewStoredObjectGCBatch(store.ListStoredObjectGCCandidates, objectGCCandidate.Process)
	objectGCRound := service.NewStoredObjectGCRound(objectDeletionRecovery.RunOnce, objectGCBatch.RunOnce)

	// 34. 启动回收 worker，登记取消并等待的回调，使其先于连接清理执行。
	stopObjectGC := startServerStoredObjectGCWorker(ctx, objectGCRound.RunOnce, logger)
	defer stopObjectGC()

	// 35. 绑定已有到期扫描和单项条件标记。
	expiredTrashRoots := service.NewExpiredTrashRootBatch(store.ListExpiredTrashRootCandidates, store.MarkExpiredTrashRootPurging)

	// 36. 启动到期标记 worker，登记取消并等待的回调，使其先于数据库清理执行。
	stopExpiredTrashRoots := startServerExpiredTrashRootWorker(ctx, expiredTrashRoots.RunOnce, logger)
	defer stopExpiredTrashRoots()

	// 37. 启动分片初始化，退出时先等待 worker，再清理 MinIO 和 MySQL 连接。
	multipartInitialization := newServerMultipartUploadInitializationBatch(store, storageClient, storageConfig)
	stopMultipartInitialization := startServerMultipartUploadInitializationWorker(ctx, multipartInitialization.RunOnce, logger)
	defer stopMultipartInitialization()

	// 38. 启动分片合并，无受信实例确认时保留缺失任务的重试；释放连接前等待 worker 退出。
	uploadMergeBatch := newServerUploadMergeBatch(store, storageClient, storageConfig, nil)
	stopUploadMerge := startServerUploadMergeWorker(ctx, uploadMergeBatch.RunOnce, logger)
	defer stopUploadMerge()

	// 39. 启动分片校验，释放 MinIO 与 MySQL 连接前先等待校验 worker 退出。
	multipartVerification := newServerMultipartUploadVerificationBatch(store, storageClient.OpenUploadVerificationObject)
	stopMultipartVerification := startServerMultipartUploadVerificationWorker(ctx, multipartVerification.RunOnce, logger)
	defer stopMultipartVerification()

	// 40. 仅在选定本机 Compose 项目时装配实例证明，供分片取消复查使用。
	var cancellationProof storage.MultipartSessionLockVerifier
	if project := storageConfig.MinIOProofComposeProject; project != "" {
		cancellationProof = newLocalMinIOSessionLockVerifier(project, newLocalMinIODockerSnapshotReader(project))
	}

	// 41. 启动分片取消；无受信证明时，已绑定任务保留预占并重试。
	multipartCancellation := newServerMultipartUploadCancellationBatch(store, storageClient, storageConfig, cancellationProof)
	stopMultipartCancellation := startServerMultipartUploadCancellationWorker(ctx, multipartCancellation.RunOnce, logger)
	// 42. 登记关停等待，使分片取消 worker 先于连接释放退出。
	defer stopMultipartCancellation()

	// 43. 启动 HTTP 并处理退出通知。
	logger.Info("准备启动 HTTP 服务", "address", listenAddress)
	if err := serverhttp.Serve(ctx, httpServer, 30*time.Second, logger); err != nil {
		if !errors.Is(err, context.Canceled) {
			logger.Error("HTTP 服务运行或退出失败", "stage", "http", "address", listenAddress, "error", err)
		}
		return err
	}
	return nil
}
