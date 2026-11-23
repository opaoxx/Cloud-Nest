package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store 封装业务数据访问，连接池或专用连接由调用方管理。
type Store struct {
	// database 只在 Repository 内部使用，不向 HTTP 或 Service 暴露 ORM 操作。
	database *gorm.DB
	// loginUserID 仅在登录事务锁定并复查用户后设置，限定设备写入归属。
	loginUserID uint64
	// refreshSession 仅由刷新事务入口绑定，保存已锁定并复查的会话副本。
	refreshSession *domain.VerifiedRefreshSession
	// authenticatedActor 由已认证用户事务入口绑定，限定设备及会话写入的归属与当前身份。
	authenticatedActor *domain.Actor
	// purgeRoot 仅在后台清理事务确认并锁定根项后绑定。
	purgeRoot *domain.PurgeRootCandidate
	// syncSnapshot 仅在一致性快照事务读取并核对头部后绑定。
	syncSnapshot *syncSnapshotScope
	// syncChangePullUserID 仅在增量拉取读视图复核身份及设备状态后绑定。
	syncChangePullUserID uint64
}

// NewStore 用现有连接池装配 GORM，不创建连接池、不执行探测或迁移，也不接管关闭操作。
func NewStore(connectionPool *sql.DB) (*Store, error) {
	// 1. 检查连接池参数，避免静默创建其他连接。
	if connectionPool == nil {
		return nil, fmt.Errorf("创建数据访问入口需要现有 MySQL 连接池")
	}
	return newStore(connectionPool)
}

// NewRestoredSyncStore 只用身份核对通过的专用连接装配停服恢复仓储。
// 调用方继续管理该连接，并在扫描和换代完成后关闭。
func NewRestoredSyncStore(
	ctx context.Context, connection *sql.Conn, expectedServerUUID uuid.UUID, expectedDatabase string,
) (*Store, error) {
	// 1. 核对传入连接的实例 UUID 和库名。
	if err := CheckRestoredSyncTarget(ctx, connection, expectedServerUUID, expectedDatabase); err != nil {
		return nil, err
	}

	// 2. 用同一条连接装配仓储，生命周期仍由调用方管理。
	return newStore(connection)
}

func newStore(connection gorm.ConnPool) (*Store, error) {
	// 1. 复用传入的数据库入口，禁用隐式网络探测和原始 SQL 日志。
	database, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      connection,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               logger.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("装配 MySQL 数据访问入口失败: %w", err)
	}

	// 2. 返回数据访问入口，底层连接仍由原调用方关闭。
	return &Store{database: database}, nil
}
