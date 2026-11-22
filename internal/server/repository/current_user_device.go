package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FindCurrentUserDevice 在 WithUserTransaction 回调内读取会话所属设备的快照。
// 使用事务中已锁定的当前设备，为同账号内容复用提供来源名称。
func (store *Store) FindCurrentUserDevice(ctx context.Context) (*domain.UserDevice, error) {
	// 1. 确认调用来自已认证的账号事务。
	if store == nil || store.database == nil || ctx == nil || store.loginUserID == 0 || store.authenticatedActor == nil {
		return nil, fmt.Errorf("读取当前设备需要上下文和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("读取当前设备必须使用已认证事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 从事务身份取得用户 ID 和当前设备 ID。
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}

	// 3. 读取已锁定的设备记录，校验后返回快照。
	// 设备锁已在认证事务入口获得，当前读可取得最新字段而不依赖事务的普通读快照。
	query := store.database.Clauses(clause.Locking{Strength: "UPDATE"})
	device, err := findUserDeviceSnapshot(ctx, query, actor.UserID, actor.DeviceID)
	if err != nil {
		return nil, err
	}
	if device.RevokedAt != nil {
		return nil, domain.ErrDeviceRevoked
	}
	return device, nil
}
