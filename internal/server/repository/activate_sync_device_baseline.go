package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ActivateSyncDeviceBaseline 在已认证账号事务内条件激活当前设备。
// 调用方须在同一事务复核凭据期限与截止序号。返回 true 表示本事务已更新一行，仍需提交。
// 返回 false、nil 表示现有记录未满足更新条件，调用方可依据锁读的状态处理重试或失效。
func (store *Store) ActivateSyncDeviceBaseline(ctx context.Context, generation domain.SyncCursorGeneration, highWaterSequence uint64) (bool, error) {
	// 1. 校验事务身份与代次，账号和设备只取自已复核的 Actor。
	if ctx == nil || store == nil || store.database == nil || store.loginUserID == 0 || store.authenticatedActor == nil {
		return false, fmt.Errorf("激活同步设备基线需要已认证账号事务和上下文")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return false, fmt.Errorf("激活同步设备基线必须使用账号事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return false, domain.ErrInvalidSessionData
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if generation == (domain.SyncCursorGeneration{}) {
		return false, domain.ErrInvalidSyncDeviceData
	}

	// 2. 只激活仍处于待确认状态、确认位置为零且完整代次匹配的记录。
	result := store.database.WithContext(ctx).Exec(
		"UPDATE sync_devices SET status = ?, ack_sequence = ?, last_seen_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6) "+
			"WHERE user_id = ? AND device_id = ? AND status = ? AND ack_sequence = 0 AND cursor_generation = ?",
		domain.SyncDeviceStatusActive, highWaterSequence, actor.UserID, actor.DeviceID, domain.SyncDeviceStatusResetRequired, generation[:],
	)

	// 3. 区分单行更新、条件未命中和执行故障，提交由外层事务负责。
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if result.Error != nil {
		return false, fmt.Errorf("激活同步设备基线失败: %w", result.Error)
	}
	if result.RowsAffected > 1 {
		return false, domain.ErrInvalidSyncDeviceData
	}
	return result.RowsAffected == 1, nil
}
