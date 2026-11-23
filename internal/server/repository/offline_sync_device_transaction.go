package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WithOfflineSyncDeviceTransaction 在所属账号事务内加锁复核单条离线设备候选。
// 回调仅使用传入的上下文和 Store，锁随事务结束释放。
func (store *Store) WithOfflineSyncDeviceTransaction(
	ctx context.Context,
	cutoff time.Time,
	candidate domain.OfflineSyncDeviceCandidate,
	action func(context.Context, *Store, domain.OfflineSyncDeviceCandidate) error,
) error {
	// 1. 校验候选和截止时间，进入所属账号事务。
	if store == nil || store.database == nil || ctx == nil || action == nil {
		return fmt.Errorf("复核离线同步设备需要数据入口、上下文和事务回调")
	}
	if cutoff.IsZero() || cutoff.Nanosecond()%int(time.Microsecond) != 0 {
		return fmt.Errorf("复核离线同步设备需要有效的微秒精度截止时间")
	}
	if !candidate.IsValid() {
		return domain.ErrInvalidSyncDeviceData
	}
	const allowDisabled = true
	return store.withLockedUserTransaction(ctx, candidate.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		// 2. 锁读认证设备，核对归属和撤销状态。
		var device models.LoginDevice
		err := txStore.database.WithContext(txContext).
			Table("user_devices FORCE INDEX (uk_devices_owner_id)").
			Select("id", "user_id", "revoked_at").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("user_id = ? AND id = ?", candidate.UserID, candidate.DeviceID).Take(&device).Error
		if contextError := txContext.Err(); contextError != nil {
			return contextError
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.ErrOfflineSyncDeviceCandidateStale
		}
		if err != nil {
			return fmt.Errorf("锁定离线认证设备失败: %w", err)
		}
		if device.UserID != candidate.UserID || device.ID != candidate.DeviceID {
			return domain.ErrInvalidSyncDeviceData
		}
		if device.RevokedAt != nil {
			return domain.ErrOfflineSyncDeviceCandidateStale
		}

		// 3. 锁读同步行，核对状态、代次和活动时间，再执行回调。
		current := domain.OfflineSyncDeviceCandidate{UserID: device.UserID, DeviceID: device.ID}
		var status domain.SyncDeviceStatus
		var sequence uint64
		var generation []byte
		err = txStore.database.WithContext(txContext).Raw(`SELECT status, ack_sequence, cursor_generation, last_seen_at
			FROM sync_devices WHERE user_id = ? AND device_id = ? FOR UPDATE`, current.UserID, current.DeviceID).
			Row().Scan(&status, &sequence, &generation, &current.LastSeenAt)
		if contextError := txContext.Err(); contextError != nil {
			return contextError
		}
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrOfflineSyncDeviceCandidateStale
		}
		if err != nil {
			return fmt.Errorf("锁定离线设备同步记录失败: %w", err)
		}
		if len(generation) != len(current.Generation) {
			return domain.ErrInvalidSyncDeviceData
		}
		current.Generation = domain.SyncCursorGeneration(generation)
		current.LastSeenAt = current.LastSeenAt.UTC()
		if !current.IsValid() {
			return domain.ErrInvalidSyncDeviceData
		}
		switch status {
		case domain.SyncDeviceStatusActive:
		case domain.SyncDeviceStatusResetRequired:
			if sequence != 0 {
				return domain.ErrInvalidSyncDeviceData
			}
			return domain.ErrOfflineSyncDeviceCandidateStale
		default:
			return domain.ErrInvalidSyncDeviceData
		}
		if current.Generation != candidate.Generation || !current.LastSeenAt.Equal(candidate.LastSeenAt) ||
			!current.LastSeenAt.Before(cutoff) {
			return domain.ErrOfflineSyncDeviceCandidateStale
		}
		return action(txContext, txStore, current)
	})
}
