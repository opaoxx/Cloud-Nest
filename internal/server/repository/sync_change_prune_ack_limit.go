package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// readSyncChangePruneAckLimit 在持有账号锁的仓储事务内只读计算裁剪的设备确认上界。
func (store *Store) readSyncChangePruneAckLimit(ctx context.Context) (uint64, error) {
	// 1. 从已有账号事务取得归属，并复用日志续读区间查询。
	if ctx == nil || store == nil || store.database == nil || store.loginUserID == 0 {
		return 0, fmt.Errorf("读取日志裁剪确认上界需要账号事务和上下文")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return 0, fmt.Errorf("读取日志裁剪确认上界必须持有账号锁")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	bounds, err := store.readSyncChangeSequenceBounds(ctx, store.loginUserID)
	if err != nil {
		return 0, err
	}

	// 2. 在锁内按共同归属关联同步行与设备。保留无法匹配的行，避免把异常归属算成没有设备。
	const query = `SELECT states.device_id, devices.id, devices.revoked_at, states.status,
		states.cursor_generation, states.ack_sequence
		FROM sync_devices AS states
		LEFT JOIN user_devices AS devices ON devices.user_id = states.user_id AND devices.id = states.device_id
		WHERE states.user_id = ?`
	rows, err := store.database.WithContext(ctx).Raw(query, store.loginUserID).Rows()
	if err != nil {
		return 0, fmt.Errorf("读取日志裁剪相关设备失败: %w", err)
	}
	defer rows.Close()

	// 3. 只让有效设备限制上界。所有参与位置经校验后，返回最小值。
	ackLimit := bounds.HighWaterSequence
	for rows.Next() {
		var deviceID, ackSequence uint64
		var ownedDeviceID sql.Null[uint64]
		var revokedAt sql.NullTime
		var status domain.SyncDeviceStatus
		var generation []byte
		if err := rows.Scan(&deviceID, &ownedDeviceID, &revokedAt, &status, &generation, &ackSequence); err != nil {
			return 0, fmt.Errorf("读取设备确认位置失败: %w", err)
		}
		if deviceID == 0 || !ownedDeviceID.Valid || ownedDeviceID.V != deviceID {
			return 0, domain.ErrInvalidSyncDeviceData
		}
		switch status {
		case domain.SyncDeviceStatusResetRequired:
			continue
		case domain.SyncDeviceStatusActive:
		default:
			return 0, domain.ErrInvalidSyncDeviceData
		}
		if revokedAt.Valid {
			continue
		}
		var emptyGeneration domain.SyncCursorGeneration
		if len(generation) != len(emptyGeneration) || domain.SyncCursorGeneration(generation) == emptyGeneration ||
			ackSequence < bounds.MinimumPosition || ackSequence > bounds.HighWaterSequence {
			return 0, domain.ErrInvalidSyncDeviceData
		}
		ackLimit = min(ackLimit, ackSequence)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("读取日志裁剪相关设备失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return ackLimit, nil
}
