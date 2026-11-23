package repository

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MarkOfflineSyncDeviceResetRequired 复核单条候选并提交待重建状态。
// 候选已失效时返回 ErrOfflineSyncDeviceCandidateStale，事务提交后才返回成功。
func (store *Store) MarkOfflineSyncDeviceResetRequired(
	ctx context.Context, cutoff time.Time, candidate domain.OfflineSyncDeviceCandidate,
) error {
	// 1. 在所属账号事务内复核候选。
	return store.WithOfflineSyncDeviceTransaction(ctx, cutoff, candidate, func(txContext context.Context, txStore *Store, current domain.OfflineSyncDeviceCandidate) error {
		return txStore.markOfflineSyncDeviceResetRequired(txContext, cutoff.UTC(), current)
	})
}

// markOfflineSyncDeviceResetRequired 使用复核回调提供的事务和当前候选。
func (store *Store) markOfflineSyncDeviceResetRequired(
	ctx context.Context, cutoff time.Time, current domain.OfflineSyncDeviceCandidate,
) error {
	// 1. 为已通过复核的设备生成完整新代次。
	if store.loginUserID == 0 || store.loginUserID != current.UserID {
		return domain.ErrInvalidSyncDeviceData
	}
	var newGeneration domain.SyncCursorGeneration
	rand.Read(newGeneration[:])
	if newGeneration == (domain.SyncCursorGeneration{}) || newGeneration == current.Generation {
		return domain.ErrInvalidSyncDeviceData
	}

	// 2. 条件写入待重建状态，保留上次成功同步时间。
	result := store.database.WithContext(ctx).Exec(`
		UPDATE sync_devices
		SET status = ?, ack_sequence = 0, cursor_generation = ?, updated_at = UTC_TIMESTAMP(6)
		WHERE user_id = ? AND device_id = ? AND status = ?
		  AND cursor_generation = ? AND last_seen_at = ? AND last_seen_at < ?`,
		domain.SyncDeviceStatusResetRequired, newGeneration[:], current.UserID, current.DeviceID,
		domain.SyncDeviceStatusActive, current.Generation[:], current.LastSeenAt, cutoff)
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Error != nil {
		return fmt.Errorf("标记离线同步设备待重建失败: %w", result.Error)
	}

	// 3. 核对只更新一行，由外层事务提交。
	switch result.RowsAffected {
	case 0:
		return domain.ErrOfflineSyncDeviceCandidateStale
	case 1:
		return nil
	default:
		return domain.ErrInvalidSyncDeviceData
	}
}
