package repository

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RevokeUserDevice 在已认证用户事务中撤销目标设备及其会话，并使该设备同步基线失效。
// 重复调用保留已有撤销时间，缺失或其他账号的目标返回 ErrDeviceNotFound。
// ctx 与 Store 来自 WithUserTransaction，外层事务提交成功后确认生效。
func (store *Store) RevokeUserDevice(ctx context.Context, targetDeviceID uint64) error {
	// 1. 检查已认证事务、目标设备 ID 和上下文。
	if ctx == nil || targetDeviceID == 0 || store.authenticatedActor == nil || store.loginUserID == 0 {
		return fmt.Errorf("撤销指定设备需要上下文、非零目标 ID 和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return fmt.Errorf("撤销指定设备必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return domain.ErrInvalidSessionData
	}
	updateContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := updateContext.Err(); err != nil {
		return err
	}

	// 2. 只读确认目标归属，再锁定目标设备。
	// 普通读取先拒绝其他账号的 ID，避免等待其设备行锁。
	var scope models.LoginDevice
	queryError := store.database.WithContext(updateContext).
		Select("id", "user_id").Where("user_id = ? AND id = ?", actor.UserID, targetDeviceID).Take(&scope).Error
	if err := updateContext.Err(); err != nil {
		return err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return domain.ErrDeviceNotFound
	}
	if queryError != nil {
		return fmt.Errorf("定位待撤销设备失败: %w", queryError)
	}
	if scope.ID != targetDeviceID || scope.UserID != actor.UserID {
		return domain.ErrInvalidDeviceData
	}
	// 外层持有用户锁，同账号的认证写入按该锁串行，此处读取设备当前状态。
	var target models.LoginDevice
	queryError = store.database.WithContext(updateContext).
		Select("id", "user_id", "revoked_at").Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND id = ?", actor.UserID, targetDeviceID).Take(&target).Error
	if err := updateContext.Err(); err != nil {
		return err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return domain.ErrDeviceNotFound
	}
	if queryError != nil {
		return fmt.Errorf("锁定待撤销设备失败: %w", queryError)
	}
	if target.ID != targetDeviceID || target.UserID != actor.UserID {
		return domain.ErrInvalidDeviceData
	}

	// 3. 目标尚未撤销时，写入设备撤销时间。
	if target.RevokedAt == nil {
		result := store.database.WithContext(updateContext).Exec(
			"UPDATE user_devices SET revoked_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) "+
				"WHERE user_id = ? AND id = ? AND revoked_at IS NULL", actor.UserID, targetDeviceID)
		if err := updateContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("撤销指定设备失败: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidDeviceData
		}
	}

	// 4. 撤销该设备下尚未撤销的全部会话。
	// 已撤销设备也执行此条件更新，以补齐历史记录中尚未撤销的会话。
	result := store.database.WithContext(updateContext).Exec(
		"UPDATE auth_sessions SET revoked_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) "+
			"WHERE user_id = ? AND device_id = ? AND revoked_at IS NULL", actor.UserID, targetDeviceID)

	if err := updateContext.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("撤销设备关联会话失败: %w", result.Error)
	}
	if result.RowsAffected < 0 {
		return domain.ErrInvalidSessionData
	}

	// 5. 在同一事务使目标同步代次失效，外层负责提交或回滚。
	return store.invalidateRevokedDeviceSyncState(updateContext, target.UserID, target.ID)
}

// invalidateRevokedDeviceSyncState 仅由已持有账号和目标设备锁的撤销事务调用。
func (store *Store) invalidateRevokedDeviceSyncState(ctx context.Context, userID, deviceID uint64) error {
	// 1. 锁读目标设备的同步行。尚未同步过的设备可以直接完成撤销。
	current, err := store.readSyncDeviceState(ctx, userID, deviceID, true)
	if errors.Is(err, domain.ErrSyncDeviceNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	// 2. 换代并清零位置。重复撤销时也使已有待确认快照失效。
	var generation domain.SyncCursorGeneration
	rand.Read(generation[:])
	if generation == (domain.SyncCursorGeneration{}) || generation == current.Generation {
		return domain.ErrInvalidSyncDeviceData
	}
	result := store.database.WithContext(ctx).Exec(`
		UPDATE sync_devices
		SET status = ?, ack_sequence = 0, cursor_generation = ?, updated_at = UTC_TIMESTAMP(6)
		WHERE user_id = ? AND device_id = ? AND status = ? AND cursor_generation = ? AND ack_sequence = ?`,
		domain.SyncDeviceStatusResetRequired, generation[:], userID, deviceID,
		current.Status, current.Generation[:], current.AckSequence)

	// 3. 核对更新结果，由外层确认三类写入一起提交。
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("使已撤销设备的同步基线失效失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.ErrInvalidSyncDeviceData
	}
	return nil
}
