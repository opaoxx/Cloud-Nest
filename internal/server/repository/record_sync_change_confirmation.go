package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// RecordSyncChangeConfirmation 记录当前活动设备的位置与同步活动时间。
// 调用方须在同一账号事务内复核游标、设备状态和区间，只传入向前或等值的确认。
// true 表示条件命中，仍须提交事务。等值确认在同一微秒无字段变化时也可命中。
func (store *Store) RecordSyncChangeConfirmation(
	ctx context.Context, generation domain.SyncCursorGeneration, expectedSequence, confirmedSequence uint64,
) (bool, error) {
	// 1. 核对认证账号事务、代次和确认位置。
	if ctx == nil || store == nil || store.database == nil || store.loginUserID == 0 || store.authenticatedActor == nil {
		return false, fmt.Errorf("记录增量确认需要已认证账号事务和上下文")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return false, fmt.Errorf("记录增量确认必须使用账号事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return false, domain.ErrInvalidSessionData
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if generation == (domain.SyncCursorGeneration{}) || confirmedSequence < expectedSequence {
		return false, domain.ErrInvalidSyncDeviceData
	}

	// 2. 按账号、设备、活动状态、代次和原位置条件更新。
	result := store.database.WithContext(ctx).Exec(
		"UPDATE sync_devices SET ack_sequence = ?, last_seen_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) "+
			"WHERE user_id = ? AND device_id = ? AND status = ? AND cursor_generation = ? AND ack_sequence = ?",
		confirmedSequence, actor.UserID, actor.DeviceID, domain.SyncDeviceStatusActive, generation[:], expectedSequence,
	)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if result.Error != nil {
		return false, fmt.Errorf("保存设备增量确认失败: %w", result.Error)
	}
	if result.RowsAffected > 1 {
		return false, domain.ErrInvalidSyncDeviceData
	}
	if result.RowsAffected == 1 || confirmedSequence > expectedSequence {
		return result.RowsAffected == 1, nil
	}

	// 3. 等值更新无变化时复核同一行，确认条件是否仍然满足。
	state, err := store.readSyncDeviceState(ctx, actor.UserID, actor.DeviceID, true)
	if errors.Is(err, domain.ErrSyncDeviceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state.Status == domain.SyncDeviceStatusActive && state.Generation == generation && state.AckSequence == expectedSequence, nil
}
