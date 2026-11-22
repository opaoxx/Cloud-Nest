package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ResetSyncDeviceBaseline 在已认证账号事务内，为当前设备登记待确认的新基线代次。
// 调用方先完成快照截止序号复核，将任何失败交给外层事务回滚。
func (store *Store) ResetSyncDeviceBaseline(ctx context.Context, generation domain.SyncCursorGeneration) error {
	// 1. 校验账号事务、当前设备身份与新代次。
	if ctx == nil || store == nil || store.database == nil || store.loginUserID == 0 || store.authenticatedActor == nil {
		return fmt.Errorf("重置同步设备基线需要已认证账号事务和上下文")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return fmt.Errorf("重置同步设备基线必须使用账号事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return domain.ErrInvalidSessionData
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var zeroGeneration domain.SyncCursorGeneration
	if generation == zeroGeneration {
		return domain.ErrInvalidSyncDeviceData
	}

	// 2. 在账号与设备锁内读取当前同步代次，区分首次同步和重建。
	transaction := store.database.WithContext(ctx)
	var storedGeneration []byte
	err := transaction.Raw(
		"SELECT cursor_generation FROM sync_devices WHERE user_id = ? AND device_id = ? FOR UPDATE",
		actor.UserID, actor.DeviceID,
	).Row().Scan(&storedGeneration)
	if contextError := ctx.Err(); contextError != nil {
		return contextError
	}
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return fmt.Errorf("读取待重置的同步设备代次失败: %w", err)
	}

	// 3. 写入待确认状态和新代次。重建保留原创建时间。
	var result *gorm.DB
	if missing {
		result = transaction.Exec(
			"INSERT INTO sync_devices (device_id, user_id, ack_sequence, status, cursor_generation, last_seen_at) "+
				"VALUES (?, ?, 0, 'RESET_REQUIRED', ?, UTC_TIMESTAMP(6))",
			actor.DeviceID, actor.UserID, generation[:])
	} else {
		if len(storedGeneration) != len(generation) {
			return domain.ErrInvalidSyncDeviceData
		}
		current := domain.SyncCursorGeneration(storedGeneration)
		if current == zeroGeneration || current == generation {
			return domain.ErrInvalidSyncDeviceData
		}
		result = transaction.Exec(
			"UPDATE sync_devices SET ack_sequence = 0, status = 'RESET_REQUIRED', cursor_generation = ?, "+
				"last_seen_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) WHERE user_id = ? AND device_id = ?",
			generation[:], actor.UserID, actor.DeviceID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("保存待确认的同步设备基线失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.ErrInvalidSyncDeviceData
	}
	return nil
}
