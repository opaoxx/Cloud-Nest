package repository

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ResetRestoredSyncDevice 在所属账号事务内复核并重置一条恢复同步记录。
// 调用方使用已核对且停服的恢复目标。事务提交后才返回成功。
func (store *Store) ResetRestoredSyncDevice(ctx context.Context, candidate domain.RestoredSyncDeviceCandidate) error {
	// 1. 在所属账号事务内复核候选。
	return store.WithRestoredSyncDeviceTransaction(ctx, candidate, func(txContext context.Context, txStore *Store, current domain.RestoredSyncDeviceCandidate) error {
		return txStore.resetRestoredSyncDevice(txContext, current)
	})
}

func (store *Store) resetRestoredSyncDevice(ctx context.Context, current domain.RestoredSyncDeviceCandidate) error {
	// 2. 生成新代次，条件写入待重建状态和零确认位置。
	updated, err := store.markLockedSyncDeviceResetRequired(ctx, current.UserID, current.DeviceID, current.State, "重置恢复同步设备失败")
	if err != nil {
		return err
	}

	// 3. 核对只更新一行，由外层事务提交或回滚。
	if !updated {
		return domain.ErrRestoredSyncDeviceCandidateStale
	}
	return nil
}
