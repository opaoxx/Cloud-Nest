package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// WithRestoredSyncDeviceTransaction 在所属账号事务内加锁复核恢复设备候选。
// 调用者使用已核对且停服的恢复目标，回调只在本次事务内使用传入的上下文和 Store。
func (store *Store) WithRestoredSyncDeviceTransaction(
	ctx context.Context,
	candidate domain.RestoredSyncDeviceCandidate,
	action func(context.Context, *Store, domain.RestoredSyncDeviceCandidate) error,
) error {
	// 1. 校验扫描候选，进入所属账号事务。禁用账号仍参与恢复。
	if store == nil || store.database == nil || ctx == nil || action == nil {
		return fmt.Errorf("复核恢复同步设备需要数据入口、上下文和事务回调")
	}
	if candidate.UserID == 0 || candidate.DeviceID == 0 || !candidate.State.IsValid() {
		return domain.ErrInvalidSyncDeviceData
	}
	const allowDisabled = true
	return store.withLockedUserTransaction(ctx, candidate.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		// 2. 锁定认证设备，复核账号和设备归属。已撤销设备仍参与换代。
		transaction := txStore.database.WithContext(txContext)
		var deviceID uint64
		err := transaction.Raw("SELECT id FROM user_devices FORCE INDEX (uk_devices_owner_id) WHERE user_id = ? AND id = ? FOR UPDATE",
			txStore.loginUserID, candidate.DeviceID).Row().Scan(&deviceID)
		if contextError := txContext.Err(); contextError != nil {
			return contextError
		}
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrInvalidSyncDeviceData
		}
		if err != nil {
			return fmt.Errorf("锁定恢复认证设备失败: %w", err)
		}
		if deviceID != candidate.DeviceID {
			return domain.ErrInvalidSyncDeviceData
		}

		// 3. 锁读同步行，对照账号最大序号及扫描时的状态。
		state, err := txStore.readSyncDeviceState(txContext, txStore.loginUserID, deviceID, true)
		if err != nil {
			return err
		}
		var highWater uint64
		err = transaction.Raw("SELECT change_seq FROM users WHERE id = ? FOR UPDATE", txStore.loginUserID).Row().Scan(&highWater)
		if contextError := txContext.Err(); contextError != nil {
			return contextError
		}
		if err != nil {
			return fmt.Errorf("复核恢复账号的最大变更序号失败: %w", err)
		}
		if state.AckSequence > highWater {
			return domain.ErrInvalidSyncDeviceData
		}
		if state != candidate.State {
			return domain.ErrRestoredSyncDeviceCandidateStale
		}
		current := domain.RestoredSyncDeviceCandidate{UserID: txStore.loginUserID, DeviceID: deviceID, State: state}
		return action(txContext, txStore, current)
	})
}
