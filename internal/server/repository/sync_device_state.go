package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// FindCurrentSyncDeviceState 在 WithUserTransaction 的回调内锁读当前设备的同步状态。
// 设备归属取自已复查的身份，读到的状态仅供当前事务作出确认决定。
func (store *Store) FindCurrentSyncDeviceState(ctx context.Context) (domain.SyncDeviceState, error) {
	// 1. 检查调用来自已复核身份的账号事务。
	var empty domain.SyncDeviceState
	if ctx == nil || store == nil || store.database == nil || store.loginUserID == 0 || store.authenticatedActor == nil {
		return empty, fmt.Errorf("读取同步设备状态需要已认证账号事务和上下文")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return empty, fmt.Errorf("读取同步设备状态必须使用账号事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return empty, domain.ErrInvalidSessionData
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}

	// 2. 按账号和设备锁读当前行，取得当前已提交状态。
	return store.readSyncDeviceState(ctx, actor.UserID, actor.DeviceID, true)
}

func (store *Store) readSyncDeviceState(ctx context.Context, userID, deviceID uint64, forUpdate bool) (domain.SyncDeviceState, error) {
	// 1. 读取指定账号与设备的同步记录，写事务需要对该行加锁。
	var empty domain.SyncDeviceState
	var state domain.SyncDeviceState
	var generation []byte
	query := "SELECT status, cursor_generation, ack_sequence FROM sync_devices WHERE user_id = ? AND device_id = ?"
	if forUpdate {
		query += " FOR UPDATE"
	}
	err := store.database.WithContext(ctx).Raw(query, userID, deviceID).
		Row().Scan(&state.Status, &generation, &state.AckSequence)
	if contextError := ctx.Err(); contextError != nil {
		return empty, contextError
	}
	if errors.Is(err, sql.ErrNoRows) {
		return empty, domain.ErrSyncDeviceNotFound
	}
	if err != nil {
		return empty, fmt.Errorf("读取同步设备状态失败: %w", err)
	}

	// 2. 核对状态和完整代次。待确认记录必须仍在零位置。
	if len(generation) != len(state.Generation) {
		return empty, domain.ErrInvalidSyncDeviceData
	}
	state.Generation = domain.SyncCursorGeneration(generation)
	if state.Generation == empty.Generation {
		return empty, domain.ErrInvalidSyncDeviceData
	}
	switch state.Status {
	case domain.SyncDeviceStatusActive:
		return state, nil
	case domain.SyncDeviceStatusResetRequired:
		if state.AckSequence == 0 {
			return state, nil
		}
	}
	return empty, domain.ErrInvalidSyncDeviceData
}
