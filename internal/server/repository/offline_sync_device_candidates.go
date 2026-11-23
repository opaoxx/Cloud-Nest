package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// ListOfflineSyncDeviceCandidates 只读扫描离线设备，结果供所属账号事务复核。
// cutoff 是本轮固定的数据库 UTC 截止时间。首批从 afterDeviceID 为 0 开始。
func (store *Store) ListOfflineSyncDeviceCandidates(
	ctx context.Context, cutoff time.Time, afterDeviceID uint64, limit int,
) ([]domain.OfflineSyncDeviceCandidate, error) {
	// 1. 校验扫描入口、截止时间和页大小。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > domain.MaxOfflineSyncDeviceCandidates {
		return nil, fmt.Errorf("扫描离线同步设备需要数据入口、上下文和 1～%d 条的扫描上限", domain.MaxOfflineSyncDeviceCandidates)
	}
	if cutoff.IsZero() || cutoff.Nanosecond()%int(time.Microsecond) != 0 {
		return nil, fmt.Errorf("扫描离线同步设备需要有效的微秒精度截止时间")
	}
	cutoff = cutoff.UTC()
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨账号扫描离线同步设备须使用事务外的数据入口")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 按设备 ID 读取本页符合条件的候选。
	const query = `SELECT sync.user_id, sync.device_id, sync.cursor_generation, sync.last_seen_at
		FROM sync_devices AS sync
		JOIN user_devices AS device ON device.user_id = sync.user_id AND device.id = sync.device_id
		WHERE sync.status = ? AND device.revoked_at IS NULL
		  AND sync.last_seen_at < ? AND sync.device_id > ?
		ORDER BY sync.device_id ASC
		LIMIT ?`
	rows, err := store.database.WithContext(queryContext).
		Raw(query, domain.SyncDeviceStatusActive, cutoff, afterDeviceID, limit).Rows()
	if err != nil {
		return nil, fmt.Errorf("扫描离线同步设备失败: %w", err)
	}
	defer rows.Close()

	// 3. 核对每条候选的归属、代次、时间和分页顺序。
	candidates := make([]domain.OfflineSyncDeviceCandidate, 0, limit)
	previousDeviceID := afterDeviceID
	for rows.Next() {
		var candidate domain.OfflineSyncDeviceCandidate
		var generation []byte
		if err := rows.Scan(&candidate.UserID, &candidate.DeviceID, &generation, &candidate.LastSeenAt); err != nil {
			return nil, fmt.Errorf("读取离线同步设备候选失败: %w", err)
		}
		if len(generation) != len(candidate.Generation) {
			return nil, domain.ErrInvalidSyncDeviceData
		}
		candidate.Generation = domain.SyncCursorGeneration(generation)
		candidate.LastSeenAt = candidate.LastSeenAt.UTC()
		if !candidate.IsValid() || candidate.DeviceID <= previousDeviceID || !candidate.LastSeenAt.Before(cutoff) {
			return nil, domain.ErrInvalidSyncDeviceData
		}
		candidates = append(candidates, candidate)
		previousDeviceID = candidate.DeviceID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("扫描离线同步设备失败: %w", err)
	}
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}
